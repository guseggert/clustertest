package basic

import (
	"context"
	"fmt"

	clusteriface "github.com/guseggert/clustertest/cluster"
	"go.uber.org/zap"
)

// Cluster wraps a clusteriface.Cluster with convenience functionality.
// The clusteriface.Cluster interface is designed for minimal implementation footprint.
// Cluster adds convenience methods around a clusteriface.Cluster to make it easier to use.
// Test authors should generally use this instead of coding against a clusteriface.Cluster directly.
type Cluster struct {
	Cluster clusteriface.Cluster
	Log     *zap.SugaredLogger
	Ctx     context.Context
}

func (c *Cluster) WithLogger(l *zap.SugaredLogger) *Cluster {
	c.Log = l.Named(loggerName)
	return c
}

func (c *Cluster) Context(ctx context.Context) *Cluster {
	newC := *c
	newC.Ctx = ctx
	return &newC
}

func New(c clusteriface.Cluster) *Cluster {
	return &Cluster{
		Cluster: c,
		Log:     defaultLogger,
		Ctx:     context.Background(),
	}
}

func (c *Cluster) NewNode() (*Node, error) {
	nodes, err := c.NewNodes(1)
	if err != nil {
		return nil, err
	}
	if len(nodes) != 1 {
		return nil, fmt.Errorf("expected 1 node, got %d", len(nodes))
	}
	return nodes[0], err
}

func (c *Cluster) MustNewNode() *Node {
	n, err := c.NewNode()
	Must(err)
	return n
}

func (c *Cluster) NewNodes(n int) ([]*Node, error) {
	var basicNodes []*Node
	nodes, err := c.Cluster.NewNodes(c.Ctx, n)
	for _, n := range nodes {
		basicNodes = append(basicNodes, &Node{
			Node: n,
			Log:  c.Log.Named("basic_node"),
			Ctx:  context.Background(),
		})
	}
	return basicNodes, err
}

func (c *Cluster) MustNewNodes(n int) []*Node {
	nodes, err := c.NewNodes(n)
	Must(err)
	return nodes
}

func (c *Cluster) Cleanup() error {
	return c.Cluster.Cleanup(c.Ctx)
}

func (c *Cluster) MustCleanup() {
	Must(c.Cleanup())
}
