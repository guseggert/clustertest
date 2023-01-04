package cluster

import (
	"context"
	"io"
	"net"
	"time"
)

// BasicCluster is a basic cluster implementation around a Cluster.
// The Cluster interface is designed for minimal implementation footprint.
// BasicCluster adds convenience methods around a Cluster to make it easier to use.
// Test authors should generally use this instead of coding against a Cluster directly.
type BasicCluster struct {
	cluster Cluster
}

func New(c Cluster) *BasicCluster {
	return &BasicCluster{cluster: c}
}

func (c *BasicCluster) NewNode(ctx context.Context) (*BasicNode, error) {
	nodes, err := c.cluster.NewNodes(ctx, 1)
	if err != nil {
		return nil, err
	}
	return &BasicNode{node: nodes[0]}, nil
}

func (c *BasicCluster) NewNodes(ctx context.Context, n int) ([]*BasicNode, error) {
	var basicNodes []*BasicNode
	nodes, err := c.cluster.NewNodes(ctx, n)
	for _, n := range nodes {
		basicNodes = append(basicNodes, &BasicNode{node: n})
	}
	return basicNodes, err
}

func (c *BasicCluster) Cleanup(ctx context.Context) error {
	return c.cluster.Cleanup(ctx)
}

// BasicNode is a basic node implementation around a Node.
// The Node interface is designed for minimal implementation footprint.
// BasicNode adds convenience methods around a Node to make it easier to use.
type BasicNode struct {
	node Node
}

func (n *BasicNode) Run(ctx context.Context, req RunRequest) (RunResultWaiter, error) {
	return n.node.Run(ctx, req)
}

func (n *BasicNode) SendFile(ctx context.Context, filePath string, contents io.Reader) error {
	return n.node.SendFile(ctx, SendFileRequest{FilePath: filePath, Contents: contents})
}

func (n *BasicNode) Stop(ctx context.Context) error {
	return n.node.Stop(ctx)
}

func (n *BasicNode) Connect(ctx context.Context, req ConnectRequest) (net.Conn, error) {
	return n.node.Connect(ctx, req)
}

type BasicRunResult struct {
	StartTime time.Time
	EndTime   time.Time
	ExitCode  int
	Stdout    string
	Stderr    string
}
