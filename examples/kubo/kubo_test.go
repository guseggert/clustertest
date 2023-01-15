package kubo

import (
	"context"
	"fmt"
	"testing"
	"time"

	_ "net/http/pprof"

	"github.com/guseggert/clustertest/cluster"
	"github.com/guseggert/clustertest/cluster/local"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

var logger *zap.SugaredLogger

func init() {
	// l, err := zap.NewProduction()
	l, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	logger = l.Sugar()
}

// TestKubo launches a cluster of 4 kubo nodes and verifies that they can all connect to each other.
func TestKubo(t *testing.T) {
	ctx := context.Background()
	// clusterImpl, err := docker.NewCluster("ubuntu", docker.WithLogger(logger))
	clusterImpl, err := local.NewCluster()
	// clusterImpl, err := aws.NewCluster()
	require.NoError(t, err)

	bc, err := cluster.New(clusterImpl, cluster.WithLogger(logger))
	require.NoError(t, err)

	c := Cluster{BasicCluster: bc}

	defer c.Cleanup(ctx)

	versions := []string{"v0.15.0", "v0.16.0", "v0.17.0", "v0.18.0-rc1"}

	nodes, err := c.NewNodes(ctx, len(versions))
	require.NoError(t, err)

	// For each version, load the Kubo binary, initialize the repo, and run the daemon.
	group, groupCtx := errgroup.WithContext(ctx)
	daemonCtx, daemonCancel := context.WithCancel(ctx)
	daemonGroup, daemonGroupCtx := errgroup.WithContext(daemonCtx)
	for i, version := range versions {
		node := nodes[i]
		node.Version = version
		group.Go(func() error {
			err := node.LoadBinary(groupCtx)
			if err != nil {
				return fmt.Errorf("loading binary: %w", err)
			}
			err = node.Init(groupCtx)
			if err != nil {
				return err
			}

			err = node.ConfigureForRemote(groupCtx)
			if err != nil {
				return err
			}
			daemonGroup.Go(func() error { return node.RunDaemon(daemonGroupCtx) })
			err = node.WaitOnAPI(ctx)
			if err != nil {
				return err
			}
			return nil
		})
	}
	require.NoError(t, group.Wait())

	// verify that the versions are what we expect
	actualVersions := map[string]bool{}
	for _, node := range nodes {
		apiClient, err := node.RPCAPIClient(ctx)
		require.NoError(t, err)
		vers, _, err := apiClient.Version()
		require.NoError(t, err)
		actualVersions[vers] = true
	}

	expectedVersions := map[string]bool{
		"0.18.0-rc1": true,
		"0.17.0":     true,
		"0.16.0":     true,
		"0.15.0":     true,
	}
	assert.Equal(t, expectedVersions, actualVersions)

	ensureConnected := func(from *Node, to *Node) {
		connected := false
		httpClient, err := from.RPCHTTPClient(ctx)
		require.NoError(t, err)
		fromCIs, err := httpClient.Swarm().Peers(ctx)
		require.NoError(t, err)
		for _, ci := range fromCIs {
			toAI, err := to.AddrInfo(ctx)
			require.NoError(t, err)
			if ci.ID() == toAI.ID {
				connected = true
			}
		}
		assert.True(t, connected)
	}

	// Connect every node to every other node, then verify that the connect succeded
	for _, from := range nodes {
		for _, to := range nodes {
			fromAI, err := from.AddrInfo(ctx)
			require.NoError(t, err)
			toAI, err := to.AddrInfo(ctx)
			require.NoError(t, err)

			if fromAI.ID == toAI.ID {
				continue
			}

			RemoveLocalAddrs(toAI)

			apiClient, err := from.RPCAPIClient(ctx)
			require.NoError(t, err)

			err = apiClient.SwarmConnect(ctx, toAI.Addrs[0].String())
			require.NoError(t, err)

			// TODO poll, don't sleep
			time.Sleep(1 * time.Second)

			ensureConnected(from, to)
		}
	}

	// cancel the context and observe that daemons exit
	daemonCancel()
	require.ErrorIs(t, context.Canceled, daemonGroup.Wait())
}
