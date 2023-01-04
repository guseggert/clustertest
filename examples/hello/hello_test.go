package hello

import (
	"bytes"
	"context"
	"testing"

	"github.com/guseggert/clustertest/cluster"
	"github.com/guseggert/clustertest/cluster/aws"
	"github.com/guseggert/clustertest/cluster/docker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThing(t *testing.T) {
	ctx := context.Background()

	awsCluster, err := aws.NewCluster()
	require.NoError(t, err)
	dockerCluster, err := docker.NewCluster("ubuntu")
	require.NoError(t, err)

	run := func(t *testing.T, name string, clusterImpl cluster.Cluster) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := cluster.New(clusterImpl)

			if err != nil {
				t.Fatal(err)
			}
			node, err := c.NewNode(ctx)
			if err != nil {
				t.Fatal(err)
			}

			err = node.SendFile(ctx, "/tmp/hello", bytes.NewBuffer([]byte("hello")))
			if err != nil {
				t.Fatal(err)
			}

			stdout := &bytes.Buffer{}
			wait, err := node.Run(ctx, cluster.RunRequest{
				Command: "cat",
				Args:    []string{"/tmp/hello"},
				Stdout:  stdout,
			})
			if err != nil {
				t.Fatal(err)
			}

			exitCode, err := wait(ctx)

			assert.NoError(t, err)
			assert.Equal(t, 0, exitCode)
			assert.Equal(t, "hello", stdout.String())

			err = node.Stop(ctx)
			if err != nil {
				t.Fatal(err)
			}
		})
	}

	run(t, "AWS cluster", awsCluster)
	run(t, "Docker cluster", dockerCluster)
}
