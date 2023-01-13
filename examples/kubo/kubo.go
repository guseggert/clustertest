package kubo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/guseggert/clustertest/cluster"
	"github.com/guseggert/clustertest/cluster/docker"
	"github.com/guseggert/clustertest/cluster/local"
	shell "github.com/ipfs/go-ipfs-api"
	httpapi "github.com/ipfs/go-ipfs-http-client"
	"github.com/ipfs/kubo/config"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/zap"
)

type Cluster struct{ *cluster.BasicCluster }

func (c *Cluster) NewNodes(ctx context.Context, n int, opts ...NodeOption) ([]*Node, error) {
	var kuboNodes []*Node
	nodes, err := c.BasicCluster.NewNodes(ctx, n)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		kn, err := NewNode(ctx, n, opts...)
		if err != nil {
			return nil, err
		}
		kuboNodes = append(kuboNodes, kn)
	}
	return kuboNodes, nil
}

type Node struct {
	*cluster.BasicNode

	Log        *zap.SugaredLogger
	HTTPClient *http.Client
	Version    string

	versionsMut sync.Mutex
	versions    VersionMap

	apiAddr multiaddr.Multiaddr

	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

type NodeOption func(*Node)

func WithNodeLogger(l *zap.SugaredLogger) NodeOption {
	return func(kn *Node) {
		kn.Log = l.Named("kubo_node")
	}
}

func WithKuboVersion(version string) NodeOption {
	return func(kn *Node) {
		kn.Version = version
	}
}

func NewNode(ctx context.Context, node *cluster.BasicNode, opts ...NodeOption) (*Node, error) {
	newTransport := http.DefaultTransport.(*http.Transport).Clone()
	newTransport.DialContext = node.Dial
	httpClient := http.Client{Transport: newTransport}

	kn := &Node{
		BasicNode:  node,
		HTTPClient: &httpClient,
	}

	for _, o := range opts {
		o(kn)
	}

	if kn.Log == nil {
		l, err := zap.NewProduction()
		if err != nil {
			return nil, err
		}
		WithNodeLogger(l.Sugar())(kn)
	}

	if kn.Version == "" {
		WithKuboVersion("v0.17.0")(kn)
	}

	return kn, nil
}

func (n *Node) getOrFetchVersions(ctx context.Context) (VersionMap, error) {
	n.versionsMut.Lock()
	defer n.versionsMut.Unlock()
	if n.versions == nil {
		versionMap, err := FetchVersions(ctx)
		if err != nil {
			return nil, fmt.Errorf("fetching versions: %w", err)
		}
		n.versions = versionMap
	}
	return n.versions, nil
}

func (n *Node) LoadBinary(ctx context.Context) error {
	versions, err := n.getOrFetchVersions(ctx)
	if err != nil {
		return fmt.Errorf("fetching versions: %w", err)
	}
	var rc io.ReadCloser
	_, isLocalNode := n.Node.(*local.Node)
	_, isDockerNode := n.Node.(*docker.Node)
	if isLocalNode || isDockerNode {
		// if we're running locally, use local disk cache
		rc, err = versions.FetchArchiveWithCaching(ctx, n.Version)
		if err != nil {
			return err
		}
		defer rc.Close()
		err = n.SendFile(ctx, filepath.Join(n.RootDir(), "kubo.tar.gz"), rc)
		if err != nil {
			return err
		}

	} else if fetcher, ok := n.Node.(cluster.Fetcher); ok {
		vi, ok := versions[n.Version]
		if !ok {
			return fmt.Errorf("no such version %q", n.Version)
		}
		err = fetcher.Fetch(ctx, vi.URL, filepath.Join(n.RootDir(), "kubo.tar.gz"))
		if err != nil {
			return err
		}
	} else {
		rc, err = versions.FetchArchive(ctx, n.Version)
		if err != nil {
			return err
		}
	}

	code, err := n.Run(ctx, cluster.StartProcRequest{
		Command: "tar",
		Args:    []string{"xzf", "kubo.tar.gz"},
		WD:      n.RootDir(),
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("non-zero exit code %d when unarchiving", code)
	}
	return nil
}

func (n *Node) RunDaemon(ctx context.Context) error {
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}
	proc, err := n.StartProc(ctx, cluster.StartProcRequest{
		Env:     []string{"IPFS_PATH=" + n.IPFSPath()},
		Command: filepath.Join(n.RootDir(), "kubo", "ipfs"),
		Args:    []string{"daemon"},
		Stdout:  stdout,
		Stderr:  stderr,
	})
	if err != nil {
		return err
	}
	code, err := proc.Wait(ctx)
	if err != nil {
		return err
	}

	if code != 0 {
		n.Log.Debugf("non-zero daemon exit code %d\nstdout:\n%s\nstderr:\n%s\n", code, stdout, stderr)
		return fmt.Errorf("non-zero daemon exit code %d", code)
	}

	n.stdout = stdout
	n.stderr = stderr

	return nil
}

func (n *Node) Init(ctx context.Context) error {
	err := n.RunKubo(ctx, cluster.StartProcRequest{
		Args: []string{"init"},
	})
	if err != nil {
		return fmt.Errorf("initializing Kubo: %w", err)
	}

	return nil
}

func (n *Node) SetConfig(ctx context.Context, cfg map[string]string) error {
	// note that this must be serialized since the CLI barfs if it can't acquire the repo lock
	for k, v := range cfg {
		var stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
		err := n.RunKubo(ctx, cluster.StartProcRequest{
			Args:   []string{"config", "--json", k, v},
			Stdout: stdout,
			Stderr: stderr,
		})
		if err != nil {
			return fmt.Errorf("setting config %q when initializing: %w", k, err)
		}
	}
	return nil
}

func (n *Node) ConfigureForLocal(ctx context.Context) error {
	return n.UpdateConfig(ctx, func(cfg *config.Config) {
		cfg.Bootstrap = nil
		cfg.Addresses.Swarm = []string{"/ip4/127.0.0.1/tcp/0"}
		cfg.Addresses.API = []string{"/ip4/127.0.0.1/tcp/0"}
		cfg.Addresses.Gateway = []string{"/ip4/127.0.0.1/tcp/0"}
		cfg.Swarm.DisableNatPortMap = true
		cfg.Discovery.MDNS.Enabled = false
	})
}

func (n *Node) ConfigureForRemote(ctx context.Context) error {
	return n.UpdateConfig(ctx, func(cfg *config.Config) {
		cfg.Bootstrap = nil
		cfg.Addresses.Swarm = []string{"/ip4/0.0.0.0/tcp/0"}
		cfg.Addresses.API = []string{"/ip4/127.0.0.1/tcp/0"}
		cfg.Addresses.Gateway = []string{"/ip4/127.0.0.1/tcp/0"}
		cfg.Swarm.DisableNatPortMap = true
		cfg.Discovery.MDNS.Enabled = false
	})
}

// func (n *Node) ConfigureForLocal(ctx context.Context) error {
// 	return n.SetConfig(ctx, map[string]string{
// 		"Bootstrap":               "[]",
// 		"Addresses.Swarm":         `["/ip4/127.0.0.1/tcp/0"]`,
// 		"Addresses.API":           `["/ip4/127.0.0.1/tcp/0"]`,
// 		"Addresses.Gateway":       `["/ip4/127.0.0.1/tcp/0"]`,
// 		"Swarm.DisableNatPortMap": "true",
// 		"Discovery.MDNS.Enabled":  "false",
// 	})
// }

// func (n *Node) ConfigureForRemote(ctx context.Context) error {
// 	return n.SetConfig(ctx, map[string]string{
// 		"Bootstrap":               "[]",
// 		"Addresses.Swarm":         `["/ip4/0.0.0.0/tcp/0"]`,
// 		"Addresses.API":           `["/ip4/127.0.0.1/tcp/0"]`,
// 		"Addresses.Gateway":       `["/ip4/127.0.0.1/tcp/0"]`,
// 		"Swarm.DisableNatPortMap": "true",
// 		"Discovery.MDNS.Enabled":  "false",
// 	})
// }

func (n *Node) IPFSPath() string {
	return filepath.Join(n.RootDir(), ".ipfs")
}

func (n *Node) APIAddr(ctx context.Context) (multiaddr.Multiaddr, error) {
	if n.apiAddr != nil {
		return n.apiAddr, nil
	}
	rc, err := n.ReadFile(ctx, filepath.Join(n.IPFSPath(), "api"))
	if err != nil {
		return nil, fmt.Errorf("opening api file: %w", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("reading api file: %w", err)
	}
	ma, err := multiaddr.NewMultiaddr(string(b))
	if err != nil {
		return nil, err
	}
	n.apiAddr = ma
	return ma, nil
}

func (n *Node) RunKubo(ctx context.Context, req cluster.StartProcRequest) error {
	req.Env = append(req.Env, "IPFS_PATH="+n.IPFSPath())

	req.Command = filepath.Join(n.RootDir(), "kubo", "ipfs")

	code, err := n.Run(ctx, req)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("kubo had non-zero exit code %d", code)
	}
	return nil
}

// RPCHTTPClient returns an HTTP RPC client configured for this node (https://github.com/ipfs/go-ipfs-http-client)
// We call this the "HTTP Client" to distinguish it from the "API Client". Both are very similar, but slightly different.
func (n *Node) RPCHTTPClient(ctx context.Context) (*httpapi.HttpApi, error) {
	apiMA, err := n.APIAddr(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting API address: %w", err)
	}
	return httpapi.NewApiWithClient(apiMA, n.HTTPClient)
}

// RPCClient returns an RPC client configured for this node (https://github.com/ipfs/go-ipfs-api)
// We call this the "API Client" to distinguish it from the "HTTP Client". Both are very similar, but slightly different.
func (n *Node) RPCAPIClient(ctx context.Context) (*shell.Shell, error) {
	apiMA, err := n.APIAddr(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting API address: %w", err)
	}
	ipAddr, err := apiMA.ValueForProtocol(multiaddr.P_IP4)
	if err != nil {
		return nil, fmt.Errorf("getting ipv4 address: %w", err)
	}
	port, err := apiMA.ValueForProtocol(multiaddr.P_TCP)
	if err != nil {
		return nil, fmt.Errorf("getting TCP port; %w", err)
	}
	u := fmt.Sprintf("%s:%s", ipAddr, port)
	return shell.NewShellWithClient(u, n.HTTPClient), nil
}

func (n *Node) AddrInfo(ctx context.Context) (*peer.AddrInfo, error) {
	sh, err := n.RPCAPIClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("building API client: %w", err)
	}
	idOutput, err := sh.ID()
	if err != nil {
		return nil, fmt.Errorf("fetching id: %w", err)
	}
	peerID, err := peer.Decode(idOutput.ID)
	if err != nil {
		return nil, fmt.Errorf("decoding peer ID: %w", err)
	}
	var multiaddrs []multiaddr.Multiaddr
	for _, a := range idOutput.Addresses {
		ma, err := multiaddr.NewMultiaddr(a)
		if err != nil {
			return nil, fmt.Errorf("decoding multiaddr %q: %w", a, err)
		}
		multiaddrs = append(multiaddrs, ma)
	}
	return &peer.AddrInfo{
		ID:    peerID,
		Addrs: multiaddrs,
	}, nil

}

func (n *Node) WaitOnAPI(ctx context.Context) error {
	n.Log.Debug("waiting on API")
	for i := 0; i < 500; i++ {
		if n.checkAPI(ctx) {
			n.Log.Debugf("daemon API found")
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	n.Log.Debug("node failed to come online: \n%s\n\n%s", n.stderr, n.stdout)
	return errors.New("timed out waiting on API")
}

func (n *Node) checkAPI(ctx context.Context) bool {
	apiAddr, err := n.APIAddr(ctx)
	if err != nil {
		n.Log.Debugf("API addr not available yet: %s", err.Error())
		return false
	}
	ip, err := apiAddr.ValueForProtocol(multiaddr.P_IP4)
	if err != nil {
		n.Log.Fatal(err)
	}
	port, err := apiAddr.ValueForProtocol(multiaddr.P_TCP)
	if err != nil {
		n.Log.Fatal(err)
	}
	url := fmt.Sprintf("http://%s:%s/api/v0/id", ip, port)

	n.Log.Debugf("checking API at %s", url)

	httpResp, err := n.HTTPClient.Post(url, "", nil)
	if err != nil {
		n.Log.Debugf("API check error: %s", err.Error())
		return false
	}
	defer httpResp.Body.Close()
	resp := struct {
		ID string
	}{}

	respBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		n.Log.Debugf("error reading API check response: %s", err.Error())
		return false
	}
	n.Log.Debugf("got API check response: %s", string(respBytes))

	err = json.Unmarshal(respBytes, &resp)
	if err != nil {
		n.Log.Debugf("error decoding API check response: %s", err.Error())
		return false
	}
	if resp.ID == "" {
		n.Log.Debugf("API check response for did not contain a Peer ID")
		return false
	}
	n.Log.Debug("API check successful")
	return true
}

func (n *Node) ReadConfig(ctx context.Context) (*config.Config, error) {
	rc, err := n.ReadFile(ctx, filepath.Join(n.IPFSPath(), "config"))
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var cfg config.Config
	err = json.NewDecoder(rc).Decode(&cfg)
	return &cfg, err
}

func (n *Node) WriteConfig(ctx context.Context, c *config.Config) error {
	b, err := config.Marshal(c)
	if err != nil {
		return err
	}
	err = n.SendFile(ctx, filepath.Join(n.IPFSPath(), "config"), bytes.NewReader(b))
	if err != nil {
		return err
	}
	return nil
}

func (n *Node) UpdateConfig(ctx context.Context, f func(cfg *config.Config)) error {
	cfg, err := n.ReadConfig(ctx)
	if err != nil {
		return err
	}
	f(cfg)
	return n.WriteConfig(ctx, cfg)
}

func MultiaddrContains(ma multiaddr.Multiaddr, component *multiaddr.Component) (bool, error) {
	v, err := ma.ValueForProtocol(component.Protocol().Code)
	if err == multiaddr.ErrProtocolNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == component.Value(), nil
}

func IsLoopback(ma multiaddr.Multiaddr) (bool, error) {
	ip4Loopback, err := multiaddr.NewComponent("ip4", "127.0.0.1")
	if err != nil {
		return false, err
	}
	ip6Loopback, err := multiaddr.NewComponent("ip6", "::1")
	if err != nil {
		return false, err
	}
	ip4MapperIP6Loopback, err := multiaddr.NewComponent("ip6", "::ffff:127.0.0.1")
	if err != nil {
		return false, err
	}
	loopbackComponents := []*multiaddr.Component{ip4Loopback, ip6Loopback, ip4MapperIP6Loopback}
	for _, lc := range loopbackComponents {
		contains, err := MultiaddrContains(ma, lc)
		if err != nil {
			return false, err
		}
		if contains {
			return true, nil
		}
	}
	return false, nil
}

func RemoveLocalAddrs(ai *peer.AddrInfo) error {
	var newMAs []multiaddr.Multiaddr
	for _, addr := range ai.Addrs {
		isLoopback, err := IsLoopback(addr)
		if err != nil {
			return err
		}
		if !isLoopback {
			newMAs = append(newMAs, addr)
		}
	}
	ai.Addrs = newMAs
	return nil
}
