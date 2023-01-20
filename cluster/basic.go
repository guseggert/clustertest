package cluster

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// BasicCluster wraps a Cluster with convenience functionality.
// The Cluster interface is designed for minimal implementation footprint.
// BasicCluster adds convenience methods around a Cluster to make it easier to use.
// Test authors should generally use this instead of coding against a Cluster directly.
type BasicCluster struct {
	Cluster
	Log *zap.SugaredLogger
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

	bc := &BasicCluster{Cluster: c}

	WithLogger(logger.Sugar())(bc)

	for _, o := range opts {
		o(bc)
	}

	return bc, nil
}

func (c *BasicCluster) NewNode(ctx context.Context) (*BasicNode, error) {
	nodes, err := c.NewNodes(ctx, 1)
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
	nodes, err := c.Cluster.NewNodes(ctx, n)
	for _, n := range nodes {
		basicNodes = append(basicNodes, &BasicNode{
			Node: n,
			Log:  c.Log.Named("basic_node"),
		})
	}
	return basicNodes, err
}

// BasicNode is a basic node implementation around a Node.
// The Node interface is designed for minimal implementation footprint.
// BasicNode adds convenience methods around a Node to make it easier to use.
type BasicNode struct {
	Node
	Log *zap.SugaredLogger
}

// Run starts the given command on the node and waits for the process to exit.
func (n *BasicNode) Run(ctx context.Context, req StartProcRequest) (*ProcessResult, error) {
	proc, err := n.StartProc(ctx, req)
	if err != nil {
		return nil, err
	}
	res, err := proc.Wait(ctx)
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("non-zero exit code %d: %w", res.ExitCode, err)
	}
	return res, nil
}

// RootDir returns the root directory of the node.
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
