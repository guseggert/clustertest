package clustertest

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/guseggert/clustertest/cluster"
	"github.com/guseggert/clustertest/cluster/basic"
	"github.com/guseggert/clustertest/cluster/docker"
	"github.com/guseggert/clustertest/cluster/local"
	"github.com/guseggert/clustertest/internal/test"
	"github.com/stretchr/testify/assert"
	"golang.org/x/sync/errgroup"
)

func TestStdoutFile(t *testing.T) {
	run := func(t *testing.T, name string, impl cluster.Cluster, isInteg bool) {
		t.Run(name, func(t *testing.T) {
			if isInteg {
				test.Integration(t)
			}
			t.Parallel()

			c := basic.New(impl)
			t.Cleanup(c.MustCleanup)

			nodes := c.MustNewNodes(1)

			// In parallel, write a test file on each node and "cat" its contents back.
			group, groupCtx := errgroup.WithContext(context.Background())
			for _, node := range nodes {
				node := node.Context(groupCtx)
				group.Go(func() error {
					filePath := filepath.Join(node.RootDir(), "hello")
					err := node.SendFile(filePath, bytes.NewBuffer([]byte("hello")))
					if err != nil {
						return err
					}

					stdoutFile := filepath.Join(node.RootDir(), "stdout")
					res, err := node.Run(cluster.StartProcRequest{
						Command:    "cat",
						Args:       []string{filePath},
						StdoutFile: stdoutFile,
					})
					if err != nil {
						return err
					}

					assert.NoError(t, err)
					assert.Equal(t, 0, res.ExitCode)

					rc, err := node.ReadFile(stdoutFile)
					if err != nil {
						return err
					}
					defer rc.Close()
					b, err := io.ReadAll(rc)
					if err != nil {
						return err
					}
					assert.Equal(t, "hello", string(b))
					return nil
				})
			}
			err := group.Wait()
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	run(t, "local cluster", local.NewCluster(), false)
	run(t, "Docker cluster", docker.MustNewCluster(), true)
}
