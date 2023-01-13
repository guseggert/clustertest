package docker

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"

	"strconv"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/guseggert/clustertest/agent"
	clusteriface "github.com/guseggert/clustertest/cluster"
	"github.com/guseggert/clustertest/internal/files"
	"github.com/guseggert/clustertest/internal/net"
	"go.uber.org/zap"
)

const chars = "abcefghijklmnopqrstuvwxyz0123456789"

func init() {
	rand.Seed(time.Now().UnixNano())
}

func randString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

// Cluster is a local Cluster that runs nodes as Docker containers.
// The underlying host must have a Docker daemon running.
// This supports standard environment variables for configuring the Docker client (DOCKER_HOST etc.).
type Cluster struct {
	Log             *zap.SugaredLogger
	Certs           *agent.Certs
	NodeAgentBin    string
	BaseImage       string
	ContainerPrefix string
	DockerClient    *client.Client

	Nodes []*Node

	imagePulled bool
}

type Option func(c *Cluster)

func WithLogger(l *zap.SugaredLogger) Option {
	return func(c *Cluster) {
		c.Log = l.Named("docker_cluster")
	}
}

func WithNodeAgentBin(p string) Option {
	return func(c *Cluster) {
		c.NodeAgentBin = p
	}
}

// NewCluster creates a new local Docker cluster.
// By default, this looks for the node agent binary by searching up from PWD for a "nodeagent" file.
func NewCluster(baseImage string, opts ...Option) (*Cluster, error) {
	log, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("instantiating default logger: %w", err)
	}
	dockerClient, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("building Docker client: %w", err)
	}
	cert, err := agent.GenerateCerts()
	if err != nil {
		return nil, fmt.Errorf("generating TLS cert: %w", err)
	}
	c := &Cluster{
		Certs:           cert,
		BaseImage:       baseImage,
		DockerClient:    dockerClient,
		ContainerPrefix: randString(6),
	}

	WithLogger(log.Sugar())(c)

	for _, o := range opts {
		o(c)
	}

	if c.NodeAgentBin == "" {
		nab, err := files.FindNodeAgentBin()
		if err != nil {
			return nil, fmt.Errorf("finding node agent bin: %w", err)
		}
		c.NodeAgentBin = nab
	}

	return c, nil
}

func (c *Cluster) ensureImagePulled(ctx context.Context) error {
	if c.imagePulled {
		return nil
	}
	out, err := c.DockerClient.ImagePull(ctx, c.BaseImage, types.ImagePullOptions{})
	if err != nil {
		if out != nil {
			out.Close()
		}
		return err
	}
	defer out.Close()
	_, err = io.Copy(io.Discard, out)
	if err != nil {
		return fmt.Errorf("reading Docker pull response: %w", err)
	}
	c.imagePulled = true
	return nil
}

func (c *Cluster) NewNodes(ctx context.Context, n int) (clusteriface.Nodes, error) {
	err := c.ensureImagePulled(ctx)
	if err != nil {
		return nil, fmt.Errorf("pulling image: %w", err)
	}

	startID := len(c.Nodes)
	var newNodes []clusteriface.Node
	for i := 0; i < n; i++ {
		id := startID + i
		containerName := fmt.Sprintf("clustertest-%s-%d", c.ContainerPrefix, id)

		hostPort, err := net.GetEphemeralTCPPort()
		if err != nil {
			return nil, fmt.Errorf("acquiring ephemeral port: %w", err)
		}

		caCertPEMEncoded := base64.StdEncoding.EncodeToString(c.Certs.CA.CertPEMBytes)
		certPEMEncoded := base64.StdEncoding.EncodeToString(c.Certs.Server.CertPEMBytes)
		keyPEMEncoded := base64.StdEncoding.EncodeToString(c.Certs.Server.KeyPEMBytes)

		createResp, err := c.DockerClient.ContainerCreate(
			ctx,
			&container.Config{
				Image: c.BaseImage,
				Entrypoint: []string{"/nodeagent",
					"--ca-cert-pem", caCertPEMEncoded,
					"--cert-pem", certPEMEncoded,
					"--key-pem", keyPEMEncoded,
					"--on-heartbeat-failure", "exit",
					"--listen-addr", "0.0.0.0:8080",
				},
				ExposedPorts: nat.PortSet{"8080": struct{}{}},
			},
			&container.HostConfig{
				Binds:        []string{fmt.Sprintf("%s:/nodeagent", c.NodeAgentBin)},
				PortBindings: nat.PortMap{"8080": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(hostPort)}}},
			},
			nil,
			nil,
			containerName,
		)
		if err != nil {
			return nil, fmt.Errorf("creating Docker container: %w", err)
		}

		containerID := createResp.ID

		err = c.DockerClient.ContainerStart(ctx, containerID, types.ContainerStartOptions{})
		if err != nil {
			return nil, fmt.Errorf("starting container %q: %w", containerID, err)
		}

		agentClient, err := agent.NewClient(c.Log, c.Certs, "127.0.0.1", hostPort, agent.WithClientWaitInterval(100*time.Millisecond))
		if err != nil {
			return nil, fmt.Errorf("building nodeagent client: %w", err)
		}

		node := &Node{
			ID:            id,
			ContainerName: containerName,
			ContainerID:   createResp.ID,
			HostPort:      hostPort,
			Env:           map[string]string{},
			agentClient:   agentClient,
			dockerClient:  c.DockerClient,
		}

		newNodes = append(newNodes, node)
		c.Nodes = append(c.Nodes, node)
	}

	for _, n := range newNodes {
		n.(*Node).agentClient.WaitForServer(ctx)
	}
	return newNodes, nil
}

func (c *Cluster) Cleanup(ctx context.Context) error {
	for _, n := range c.Nodes {
		err := c.DockerClient.ContainerStop(ctx, n.ContainerID, nil)
		if err != nil {
			return fmt.Errorf("stopping node %d: %w", n.ID, err)
		}
	}
	return nil
}
