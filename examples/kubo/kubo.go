package kubo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/guseggert/clustertest/cluster"
	shell "github.com/ipfs/go-ipfs-api"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/zap"
)

type versionInfo struct {
	URL string
	CID string
}

var (
	kuboVersions = map[string]versionInfo{
		"0.17.0": {
			URL: "https://dist.ipfs.tech/kubo/v0.17.0/kubo_v0.17.0_linux-amd64.tar.gz",
			CID: "QmNhHcEZgSt2sbRaE8C9GVTPt8S9sP9J1FAEs22kdAHFzo",
		},
		"0.16.0": {
			URL: "https://dist.ipfs.tech/kubo/v0.16.0/kubo_v0.16.0_linux-amd64.tar.gz",
			CID: "QmPwfD1YBrgh3vN6ca5398bXRLkpGNUukhMqEFKbQuPqYT",
		},
	}
)

// ensureKuboCached ensures the kubo archive is cached locally, and returns paths to them.
func ensureKuboCached(t *testing.T) (map[string]string, error) {
	paths := map[string]string{}
	tempDir := os.TempDir()
	for vers, info := range kuboVersions {
		kuboArchive := filepath.Join(tempDir, info.CID)
		_, err := os.Stat(kuboArchive)
		if err != nil {
			if os.IsNotExist(err) {
				t.Logf("downloading archive for kubo %s", vers)
				resp, err := http.Get(info.URL)
				if err != nil {
					return nil, fmt.Errorf("fetching kubo archive: %w", err)
				}
				defer resp.Body.Close()
				f, err := os.Create(kuboArchive)
				if err != nil {
					return nil, fmt.Errorf("creating kubo archive: %w", err)
				}
				_, err = io.Copy(f, resp.Body)
				if err != nil {
					return nil, fmt.Errorf("copying Kubo archive: %w", err)
				}
			} else {
				return nil, err
			}
		}
		paths[vers] = kuboArchive
	}
	return paths, nil
}

type KuboCluster struct{ *cluster.BasicCluster }

func (c *KuboCluster) NewKuboNodes(ctx context.Context, n int) ([]*KuboNode, error) {
	var kuboNodes []*KuboNode
	nodes, err := c.BasicCluster.NewNodes(ctx, n)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		kuboNodes = append(kuboNodes, NewKuboNode(n, n.Log))
	}
	return kuboNodes, nil
}

type KuboNode struct {
	*cluster.BasicNode

	Log *zap.SugaredLogger

	Version string

	HTTPClient *http.Client

	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

func NewKuboNode(node *cluster.BasicNode, log *zap.SugaredLogger) *KuboNode {
	newTransport := http.DefaultTransport.(*http.Transport).Clone()
	newTransport.DialContext = node.Dial
	httpClient := http.Client{Transport: newTransport}
	return &KuboNode{
		BasicNode:  node,
		Log:        log.Named("kubo_node"),
		HTTPClient: &httpClient,
	}
}

func (n *KuboNode) RunDaemon(ctx context.Context) error {
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

func (n *KuboNode) Init(ctx context.Context) error {
	err := n.RunKubo(ctx, cluster.StartProcRequest{
		Args: []string{"init"},
	})
	if err != nil {
		return fmt.Errorf("initializing Kubo: %w", err)
	}

	configs := map[string]string{
		"Bootstrap":               "[]",
		"Addresses.Swarm":         `["/ip4/127.0.0.1/tcp/0"]`,
		"Addresses.API":           `["/ip4/127.0.0.1/tcp/0"]`,
		"Addresses.Gateway":       `["/ip4/127.0.0.1/tcp/0"]`,
		"Swarm.DisableNatPortMap": "true",
		"Discovery.MDNS.Enabled":  "false",
	}

	for k, v := range configs {
		var stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
		err = n.RunKubo(ctx, cluster.StartProcRequest{
			Args:   []string{"config", "--json", k, v},
			Stdout: stdout,
			Stderr: stderr,
		})
		if err != nil {
			fmt.Printf("stdout: %s\n", stdout)
			fmt.Printf("stderr: %s\n", stderr)
			return fmt.Errorf("setting config %q when initializing: %w", k, err)
		}
	}

	return nil
}

func (n *KuboNode) IPFSPath() string {
	return filepath.Join(n.RootDir(), ".ipfs")
}

func (n *KuboNode) APIAddr(ctx context.Context) (multiaddr.Multiaddr, error) {
	rc, err := n.ReadFile(ctx, filepath.Join(n.IPFSPath(), "api"))
	if err != nil {
		return nil, fmt.Errorf("opening api file: %w", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("reading api file: %w", err)
	}
	return multiaddr.NewMultiaddr(string(b))
}

func (n *KuboNode) RunKubo(ctx context.Context, req cluster.StartProcRequest) error {
	req.Env = append(req.Env, "IPFS_PATH="+n.IPFSPath())

	req.Command = filepath.Join(n.RootDir(), "kubo", "ipfs")

	code, err := n.RunWait(ctx, req)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("kubo had non-zero exit code %d", code)
	}
	return nil
}

func (n *KuboNode) RPCClient(ctx context.Context) (*shell.Shell, error) {
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

func (n *KuboNode) WaitOnAPI(ctx context.Context) bool {
	n.Log.Debug("waiting on API")
	for i := 0; i < 50; i++ {
		if n.checkAPI(ctx) {
			n.Log.Debugf("daemon API found")
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	n.Log.Debug("node failed to come online: \n%s\n\n%s", n.stderr, n.stdout)
	return false
}

func (n *KuboNode) checkAPI(ctx context.Context) bool {
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
