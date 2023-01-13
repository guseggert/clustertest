package cluster

import (
	"context"
	"io"
	"net"
)

type Process interface {
	// Wait waits for the process to exit and returns its exit code.
	Wait(context.Context) (int, error)
}

type StartProcRequest struct {
	Command string
	Args    []string
	// Env is the environment variables of the process, in the form "k=v".
	// If unspecified, the environment variables to use are implementation-defined.
	Env []string
	// WD is the working directory of the process.
	// If unspecified, this is implementation-defined.
	WD string
	// Stdin is a reader which, when specified, is sent to the process's stdin.
	Stdin io.Reader
	// Stdout is a writer which, when specified, receives the stdout of the process.
	Stdout io.Writer
	// Stderr is a writer which, when specified, receives the stderr of the process.
	Stderr io.Writer
}

// Node is generally a host or container, and is a member of a cluster.
// The implementation defines how to coordinate the node.
type Node interface {
	StartProc(ctx context.Context, req StartProcRequest) (Process, error)
	SendFile(ctx context.Context, filePath string, Contents io.Reader) error
	ReadFile(ctx context.Context, path string) (io.ReadCloser, error)
	Stop(ctx context.Context) error
	Dial(ctx context.Context, network, address string) (net.Conn, error)
	String() string
}

// An optional node interface for making HTTP requests on the node.
type Fetcher interface {
	// Fetch fetches content from a URL with an HTTP GET and stores the content in the given path on the node.
	Fetch(ctx context.Context, url, path string) error
}

type Nodes []Node
