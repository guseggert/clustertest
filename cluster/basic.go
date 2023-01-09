package cluster

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// BasicCluster is a basic cluster implementation around a Cluster.
// The Cluster interface is designed for minimal implementation footprint.
// BasicCluster adds convenience methods around a Cluster to make it easier to use.
// Test authors should generally use this instead of coding against a Cluster directly.
type BasicCluster struct {
	cluster Cluster
	Log     *zap.SugaredLogger
}

type Option func(c *BasicCluster)

func WithLogger(l *zap.SugaredLogger) Option {
	return func(c *BasicCluster) {
		c.Log = l.Named("basic_cluster")
	}
}

func New(c Cluster, opts ...Option) (*BasicCluster, error) {
	logger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("constructing default logger: %w", err)
	}

	bc := &BasicCluster{
		cluster: c,
	}

	WithLogger(logger.Sugar())(bc)

	for _, o := range opts {
		o(bc)
	}

	return bc, nil
}

func (c *BasicCluster) NewNode(ctx context.Context) (*BasicNode, error) {
	nodes, err := c.cluster.NewNodes(ctx, 1)
	if err != nil {
		return nil, err
	}
	return &BasicNode{
		Node: nodes[0],
		Log:  c.Log.Named("basic_node"),
	}, nil
}

func (c *BasicCluster) NewNodes(ctx context.Context, n int) ([]*BasicNode, error) {
	var basicNodes []*BasicNode
	nodes, err := c.cluster.NewNodes(ctx, n)
	for _, n := range nodes {
		basicNodes = append(basicNodes, &BasicNode{
			Node: n,
			Log:  c.Log.Named("basic_node"),
		})
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
	Node
	Log *zap.SugaredLogger
}

// RunWait runs the given command on the node and waits for it to finish, returning its exit code.
func (n *BasicNode) RunWait(ctx context.Context, req StartProcRequest) (int, error) {
	proc, err := n.StartProc(ctx, req)
	if err != nil {
		return -1, err
	}
	code, err := proc.Wait(ctx)
	if code != 0 {
		return -1, fmt.Errorf("non-zero exit code %d: %w", code, err)
	}
	return code, nil
}

func (n *BasicNode) RootDir() string {
	if rootDirer, ok := n.Node.(interface{ RootDir() string }); ok {
		return rootDirer.RootDir()
	}
	return "/"
}

type BasicRunResult struct {
	StartTime time.Time
	EndTime   time.Time
	ExitCode  int
	Stdout    string
	Stderr    string
}
