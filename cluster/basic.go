package cluster

import (
	"context"
	"fmt"
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

// Run runs the given command on the node and returns a function that waits for the exit code.
func (n *BasicNode) Run(ctx context.Context, req RunRequest) (RunResultWaiter, error) {
	return n.node.Run(ctx, req)
}

// RunWait runs the given command on the node and waits for it to finish, returning its exit code.
func (n *BasicNode) RunWait(ctx context.Context, req RunRequest) (int, error) {
	wait, err := n.node.Run(ctx, req)
	if err != nil {
		return -1, err
	}
	code, err := wait(ctx)
	if code != 0 {
		return -1, fmt.Errorf("non-zero exit code %d: %w", code, err)
	}
	return code, nil
}

func (n *BasicNode) SendFile(ctx context.Context, filePath string, contents io.Reader) error {
	return n.node.SendFile(ctx, SendFileRequest{FilePath: filePath, Contents: contents})
}

func (n *BasicNode) RootDir() string {
	if rootDirer, ok := n.node.(interface{ RootDir() string }); ok {
		return rootDirer.RootDir()
	}
	return "/"
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
