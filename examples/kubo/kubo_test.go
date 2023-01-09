package kubo

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/guseggert/clustertest/cluster"
	"github.com/guseggert/clustertest/cluster/local"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

var logger *zap.SugaredLogger

func init() {
	l, err := zap.NewProduction()
	// l, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	logger = l.Sugar()
}

func TestKubo(t *testing.T) {
	ctx := context.Background()
	// clusterImpl, err := docker.NewCluster("ubuntu", docker.WithLogger(logger))
	clusterImpl, err := local.NewCluster()
	// clusterImpl, err := aws.NewCluster()
	require.NoError(t, err)

	bc, err := cluster.New(clusterImpl, cluster.WithLogger(logger))
	require.NoError(t, err)

	c := KuboCluster{BasicCluster: bc}

	defer c.Cleanup(ctx)

	numNodes := len(kuboVersions)

	nodes, err := c.NewKuboNodes(ctx, numNodes)
	require.NoError(t, err)

	kuboPaths, err := ensureKuboCached(t)
	require.NoError(t, err)

	// send the kubo archive to each node in parallel
	var wg sync.WaitGroup
	wg.Add(numNodes)
	curNode := 0
	for vers := range kuboVersions {
		go func(curNode int, vers string) {
			defer wg.Done()
			kuboPath := kuboPaths[vers]
			node := nodes[curNode]

			f, err := os.Open(kuboPath)
			if !assert.NoError(t, err) {
				return
			}
			defer f.Close()

			err = node.SendFile(ctx, filepath.Join(node.RootDir(), "kubo.tar.gz"), f)
			if !assert.NoError(t, err) {
				return
			}

			code, err := node.RunWait(ctx, cluster.StartProcRequest{
				Command: "tar",
				Args:    []string{"xzf", "kubo.tar.gz"},
				WD:      node.RootDir(),
			})
			if !assert.NoError(t, err) {
				return
			}
			if !assert.Equal(t, 0, code) {
				return
			}
		}(curNode, vers)
		curNode++
	}
	wg.Wait()

	// run the daemon on each node
	daemonCtx, daemonCancel := context.WithCancel(ctx)
	wg = sync.WaitGroup{}
	daemonWG := sync.WaitGroup{}
	for _, node := range nodes {
		wg.Add(1)
		node := node
		go func() {
			defer wg.Done()
			err := node.Init(ctx)
			if !assert.NoError(t, err) {
				return
			}
			daemonWG.Add(1)
			go func() {
				defer daemonWG.Done()
				err := node.RunDaemon(daemonCtx)
				assert.ErrorIs(t, err, context.Canceled)
			}()
		}()
	}
	wg.Wait()

	actualVersions := map[string]bool{}
	for _, node := range nodes {
		node.WaitOnAPI(ctx)
		rpcClient, err := node.RPCClient(ctx)
		require.NoError(t, err)
		vers, _, err := rpcClient.Version()
		require.NoError(t, err)
		actualVersions[vers] = true
	}

	assert.Equal(t, map[string]bool{"0.17.0": true, "0.16.0": true}, actualVersions)

	// cancel the context and observe that daemons exit
	daemonCancel()
	daemonWG.Wait()
}
