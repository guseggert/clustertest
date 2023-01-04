package docker

import (
	"context"
	"fmt"
	"net"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/guseggert/clustertest/agent"
	clusteriface "github.com/guseggert/clustertest/cluster"
)

type node struct {
	ID            int
	ContainerName string
	ContainerID   string
	HostPort      int
	Env           map[string]string
	dockerClient  *client.Client
	agentClient   *agent.Client
}

func (n *node) runEnv(reqEnv map[string]string) []string {
	var env []string
	envMap := map[string]string{}
	for k, v := range n.Env {
		envMap[k] = v
	}
	for k, v := range reqEnv {
		envMap[k] = v
	}
	for k, v := range envMap {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	return env
}

func (n *node) Run(ctx context.Context, req clusteriface.RunRequest) (clusteriface.RunResultWaiter, error) {
	return n.agentClient.Run(ctx, req)
}

func (n *node) SendFile(ctx context.Context, req clusteriface.SendFileRequest) error {
	return n.agentClient.SendFile(ctx, req)
}

func (n *node) Stop(ctx context.Context) error {
	err := n.dockerClient.ContainerRemove(ctx, n.ContainerID, types.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	})
	if err != nil {
		return fmt.Errorf("killing container %q: %w", n.ContainerID, err)
	}
	return nil
}

func (n *node) Connect(ctx context.Context, req clusteriface.ConnectRequest) (net.Conn, error) {
	return n.agentClient.DialContext(ctx, req.Network, req.Addr)
}

func (n *node) String() string {
	return fmt.Sprintf("local node id=%d", n.ID)
}
