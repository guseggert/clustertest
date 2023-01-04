package cluster

import "context"

// Cluster holds the state for a set of nodes, and defines how to create and destroy them.
// Cluster implementations are generally not goroutine-safe.
type Cluster interface {
	// NewNodes creates nodes and adds them to the cluster. Generally this should return when the nodes are ready to use.
	NewNodes(ctx context.Context, n int) (Nodes, error)

	// Cleanup destroys all cluster nodes and any other state related to the cluster.
	Cleanup(ctx context.Context) error
}
