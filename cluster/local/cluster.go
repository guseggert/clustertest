package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	clusteriface "github.com/guseggert/clustertest/cluster"
	"github.com/guseggert/clustertest/internal/files"
)

// Cluster is a local Cluster that runs processes directly on the underlying host.
// These processes are not sandboxed, so they can see each other and everything else on the host.
// Because nodes are not sandboxed, they share the same filesystem and other namespaces,
// so code that assumes separate sandboxes/hosts may not be portable with this.
// The main benefit from using this is performance, since there are no external processes or resources to create for launching nodes.
// The performance makes this suitable for fast-feedback unit tests.
type Cluster struct {
	dir   string
	nodes []*Node
	env   map[string]string
}

type Option func(c *Cluster)

func NewCluster(opts ...Option) (*Cluster, error) {
	dir, err := os.MkdirTemp("", "")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}
	return &Cluster{
		dir: dir,
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
		nodeDir := filepath.Join(c.dir, strconv.Itoa(id))

		err := os.Mkdir(nodeDir, 0777)
		if err != nil {
			return nil, fmt.Errorf("creating dir for node %d: %w", id, err)
		}

		node := &Node{
			ID:  id,
			Env: map[string]string{},
			Dir: nodeDir,
		}

		newNodes = append(newNodes, node)
		c.nodes = append(c.nodes, node)
	}
	return newNodes, nil
}

func (c *Cluster) Cleanup(ctx context.Context) error {
	for _, node := range c.nodes {
		err := node.Stop(ctx)
		if err != nil {
			return fmt.Errorf("stopping node %d: %w", node.ID, err)
		}
	}
	return os.RemoveAll(c.dir)
}
