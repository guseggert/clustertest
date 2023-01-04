package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/google/uuid"
	clusteriface "github.com/guseggert/clustertest/cluster"
	"github.com/guseggert/clustertest/internal/files"
	"github.com/guseggert/clustertest/internal/net"
)

// Cluster is a local Cluster that runs processes directly on the underlying host.
// These processes are not sandboxed, so they can see each other and everything else on the host.
// Because nodes are not sandboxed, they share the same filesystem and other namespaces,
// so code that assumes separate sandboxes/hosts may not be portable with this.
// The main benefit from using this is performance, since there are no external processes or resources to create for launching nodes.
// The performance makes this suitable for fast-feedback unit tests.
type Cluster struct {
	dir            string
	nodes          []*node
	env            map[string]string
	nodeAgentToken string
}

type Option func(c *Cluster)

func NewCluster(baseImage string, opts ...Option) (*Cluster, error) {
	token := uuid.New().String()
	return &Cluster{
		nodeAgentToken: token,
	}, nil
}

func (c *Cluster) NewNodes(ctx context.Context, n int) (clusteriface.Nodes, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("getting wd: %w", err)
	}

	nodeAgentBin := files.FindUp("nodeagent", wd)
	if nodeAgentBin == "" {
		return nil, errors.New("unable to find nodeagent bin")
	}

	startID := len(c.nodes)
	var newNodes []clusteriface.Node
	for i := 0; i < n; i++ {
		id := startID + i

		port, err := net.GetEphemeralTCPPort()
		if err != nil {
			return nil, fmt.Errorf("acquiring ephemeral port: %w", err)
		}

		cmd := exec.Command(
			nodeAgentBin,
			"--tls-cert",
			"--on-heartbeat-failure", "exit",
			"--listen-addr", fmt.Sprintf("127.0.0.1:%d", port),
		)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags: syscall.CLONE_NEWNS,
		}

		node := &node{
			ID:  id,
			Env: map[string]string{},
		}

		newNodes = append(newNodes, node)
		c.nodes = append(c.nodes, node)
	}
	return newNodes, nil
}

func (c *Cluster) Cleanup(ctx context.Context) error {
	return nil
}
