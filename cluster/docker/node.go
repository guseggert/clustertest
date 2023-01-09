package docker

import (
	"context"
	"fmt"
	"io"
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

func (n *node) StartProc(ctx context.Context, req clusteriface.StartProcRequest) (clusteriface.Process, error) {
	return n.agentClient.StartProc(ctx, req)
}

func (n *node) SendFile(ctx context.Context, filePath string, contents io.Reader) error {
	return n.agentClient.SendFile(ctx, filePath, contents)
}

func (n *node) ReadFile(ctx context.Context, path string) (io.ReadCloser, error) {
	return n.agentClient.ReadFile(ctx, path)
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

func (n *node) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return n.agentClient.DialContext(ctx, network, addr)
}

func (n *node) String() string {
	return fmt.Sprintf("local node id=%d", n.ID)
}
