package hello

import (
	"bytes"
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/guseggert/clustertest/cluster"
	"github.com/guseggert/clustertest/cluster/aws"
	"github.com/guseggert/clustertest/cluster/docker"
	"github.com/guseggert/clustertest/cluster/local"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHello creates a multi-node cluster,
// writes a test file on each node,
// and then cats the contents back to the test runner for verification.
func TestHello(t *testing.T) {
	ctx := context.Background()

	awsCluster, err := aws.NewCluster()
	require.NoError(t, err)
	dockerCluster, err := docker.NewCluster("ubuntu")
	require.NoError(t, err)
	localCluster, err := local.NewCluster()
	require.NoError(t, err)

	numberNodes := 5

	run := func(t *testing.T, name string, clusterImpl cluster.Cluster) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c, err := cluster.New(clusterImpl)
			require.NoError(t, err)

			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { c.Cleanup(ctx) })

			// Launch the nodes. When this returns, all nodes are ready to use.
			nodes, err := c.NewNodes(ctx, numberNodes)
			if err != nil {
				t.Fatal(err)
			}

			// Write a test file on each node and "cat" its contents back.
			wg := sync.WaitGroup{}
			wg.Add(len(nodes))
			for i, node := range nodes {
				go func(nodeNum int, node *cluster.BasicNode) {
					defer wg.Done()
					filePath := filepath.Join(node.RootDir(), "hello")
					err := node.SendFile(ctx, filePath, bytes.NewBuffer([]byte("hello")))
					if err != nil {
						t.Errorf("sending file to node %d: %s", nodeNum, err)
						return
					}

					stdout := &bytes.Buffer{}
					proc, err := node.StartProc(ctx, cluster.StartProcRequest{
						Command: "cat",
						Args:    []string{filePath},
						Stdout:  stdout,
					})
					if err != nil {
						t.Errorf(`running "cat" on node %d: %s`, nodeNum, err)
						return
					}

					res, err := proc.Wait(ctx)

					assert.NoError(t, err)
					assert.Equal(t, 0, res.ExitCode)
					assert.Equal(t, "hello", stdout.String())

				}(i, node)
			}
			wg.Wait()
		})
	}

	run(t, "AWS cluster", awsCluster)
	run(t, "Docker cluster", dockerCluster)
	run(t, "Local cluster", localCluster)
}
