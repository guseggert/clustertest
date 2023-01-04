package cluster

import (
	"context"
	"io"
	"net"
)

type RunResultWaiter func(context.Context) (int, error)

type RunRequest struct {
	Command string
	Args    []string
	Env     []string
	WD      string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
}

type ConnectRequest struct {
	Network string
	Addr    string
}

type SendFileRequest struct {
	FilePath string
	Contents io.Reader
}

// Node is generally a host or container, and is a member of a cluster.
// The implementation defines how to coordinate the node.
type Node interface {
	Run(ctx context.Context, req RunRequest) (RunResultWaiter, error)
	SendFile(ctx context.Context, req SendFileRequest) error
	Stop(ctx context.Context) error
	Connect(ctx context.Context, req ConnectRequest) (net.Conn, error)
}

type Nodes []Node
