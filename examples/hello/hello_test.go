package hello

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/guseggert/clustertest/cluster"
	"github.com/guseggert/clustertest/cluster/aws"
	"github.com/guseggert/clustertest/cluster/basic"
	"github.com/guseggert/clustertest/cluster/docker"
	"github.com/guseggert/clustertest/cluster/local"
	"github.com/stretchr/testify/assert"
	"golang.org/x/sync/errgroup"
)

// TestHello creates a multi-node cluster, writes a test file on each node,
// and then cats the contents back to the test runner for verification.
func TestHello(t *testing.T) {
	run := func(t *testing.T, name string, impl cluster.Cluster) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// Create the cluster.
			c := basic.New(impl)
			t.Cleanup(c.MustCleanup)

			t.Logf("Launching %s nodes", name)
			nodes := c.MustNewNodes(5)

			// In parallel, write a test file on each node and "cat" its contents back.
			group, groupCtx := errgroup.WithContext(context.Background())
			for i, node := range nodes {
				node := node.Context(groupCtx)
				i := i
				group.Go(func() error {
					t.Logf("sending file to %s node %d", name, i)
					filePath := filepath.Join(node.RootDir(), "hello")
					err := node.SendFile(filePath, bytes.NewBuffer([]byte("hello")))
					if err != nil {
						return err
					}

					t.Logf("running 'cat' on %s node %d", name, i)
					stdout := &bytes.Buffer{}
					res, err := node.Run(cluster.StartProcRequest{
						Command: "cat",
						Args:    []string{filePath},
						Stdout:  stdout,
					})
					if err != nil {
						return err
					}

					assert.NoError(t, err)
					assert.Equal(t, 0, res.ExitCode)
					assert.Equal(t, "hello", stdout.String())
					return nil
				})
			}
			err := group.Wait()
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	run(t, "local cluster", local.NewCluster())
	run(t, "Docker cluster", docker.MustNewCluster())
	run(t, "AWS cluster", aws.NewCluster())
}
