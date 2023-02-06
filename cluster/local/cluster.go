package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	clusteriface "github.com/guseggert/clustertest/cluster"
)

// Cluster is a local Cluster that runs processes directly on the underlying host.
// These processes are not sandboxed, so they can see each other and everything else on the host.
// Because nodes are not sandboxed, they share the same filesystem and other namespaces,
// so code that assumes separate sandboxes/hosts may not be portable with this.
// The main benefit from using this is performance, since there are no external processes or resources to create for launching nodes.
// The performance makes this suitable for fast-feedback unit tests.
type Cluster struct {
	nodes []*Node
	env   map[string]string

	initMut sync.Mutex
	dir     string
}

func NewCluster() *Cluster {
	return &Cluster{}
}

func (c *Cluster) init() error {
	c.initMut.Lock()
	defer c.initMut.Unlock()
	if c.dir != "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	c.dir = dir
	return nil
}

func (c *Cluster) NewNodes(ctx context.Context, n int) (clusteriface.Nodes, error) {
	if err := c.init(); err != nil {
		return nil, err
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
	if err := c.init(); err != nil {
		return err
	}
	for _, node := range c.nodes {
		err := node.Stop(ctx)
		if err != nil {
			return fmt.Errorf("stopping node %d: %w", node.ID, err)
		}
	}
	return os.RemoveAll(c.dir)
}
