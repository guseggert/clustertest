package kubo

import (
	"context"
	"fmt"
	"testing"
	"time"

	_ "net/http/pprof"

	"github.com/guseggert/clustertest/cluster"
	"github.com/guseggert/clustertest/cluster/aws"
	shell "github.com/ipfs/go-ipfs-api"
	httpapi "github.com/ipfs/go-ipfs-http-client"
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

func TestKubo(t *testing.T) {
	ctx := context.Background()
	// clusterImpl, err := docker.NewCluster("ubuntu", docker.WithLogger(logger))
	// clusterImpl, err := local.NewCluster()
	clusterImpl, err := aws.NewCluster()
	require.NoError(t, err)

	bc, err := cluster.New(clusterImpl, cluster.WithLogger(logger))
	require.NoError(t, err)

	c := Cluster{BasicCluster: bc}

	defer c.Cleanup(ctx)

	versions := []string{"v0.15.0", "v0.16.0", "v0.17.0", "v0.18.0-rc1"}

	nodes, err := c.NewNodes(ctx, len(versions))
	require.NoError(t, err)

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

	var apiClients []*shell.Shell
	var httpClients []*httpapi.HttpApi
	actualVersions := map[string]bool{}
	for _, node := range nodes {
		apiClient, err := node.RPCAPIClient(ctx)
		require.NoError(t, err)
		httpClient, err := node.RPCHTTPClient(ctx)
		require.NoError(t, err)
		vers, _, err := apiClient.Version()
		require.NoError(t, err)
		actualVersions[vers] = true
		apiClients = append(apiClients, apiClient)
		httpClients = append(httpClients, httpClient)
	}

	for fromIdx, from := range nodes {
		for _, to := range nodes {
			fromAI, err := from.AddrInfo(ctx)
			require.NoError(t, err)
			toAI, err := to.AddrInfo(ctx)
			require.NoError(t, err)

			if fromAI.ID == toAI.ID {
				continue
			}

			RemoveLocalAddrs(toAI)

			err = apiClients[fromIdx].SwarmConnect(ctx, toAI.Addrs[0].String())
			require.NoError(t, err)
		}
	}

	ensureConnected := func(fromIdx int, to *Node) {
		connected := false
		fromCIs, err := httpClients[fromIdx].Swarm().Peers(ctx)
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

	time.Sleep(1 * time.Second)
	for fromIdx, from := range nodes {
		for _, to := range nodes {
			fromAI, err := from.AddrInfo(ctx)
			require.NoError(t, err)
			toAI, err := to.AddrInfo(ctx)
			require.NoError(t, err)

			if fromAI.ID == toAI.ID {
				continue
			}

			ensureConnected(fromIdx, to)

			fmt.Printf("%s is connected to %s\n", from.Node, to.Node)
		}
	}

	expectedVersions := map[string]bool{
		"0.18.0-rc1": true,
		"0.17.0":     true,
		"0.16.0":     true,
		"0.15.0":     true,
	}

	assert.Equal(t, expectedVersions, actualVersions)

	// cancel the context and observe that daemons exit
	daemonCancel()
	require.ErrorIs(t, context.Canceled, daemonGroup.Wait())
}
