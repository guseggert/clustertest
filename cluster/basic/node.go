package basic

import (
	"context"
	"fmt"
	"io"

	clusteriface "github.com/guseggert/clustertest/cluster"
	"go.uber.org/zap"
)

// Node wraps a clusteriface.Node and provides a lot of convenience functionality for working with nodes.
type Node struct {
	Node clusteriface.Node
	Ctx  context.Context
	Log  *zap.SugaredLogger
}

func (n *Node) Context(ctx context.Context) *Node {
	newN := *n
	newN.Ctx = ctx
	return &newN
}

func (n *Node) StartProc(req clusteriface.StartProcRequest) (*Process, error) {
	proc, err := n.Node.StartProc(n.Ctx, req)
	if err != nil {
		return nil, err
	}
	return &Process{Process: proc, Ctx: n.Ctx}, nil
}

func (n *Node) MustStartProc(req clusteriface.StartProcRequest) *Process {
	p, err := n.StartProc(req)
	Must(err)
	return p
}

// Run starts the given command on the node and waits for the process to exit.
func (n *Node) Run(req clusteriface.StartProcRequest) (*clusteriface.ProcessResult, error) {
	proc, err := n.Node.StartProc(n.Ctx, req)
	if err != nil {
		return nil, err
	}
	res, err := proc.Wait(n.Ctx)
	if err != nil {
		return nil, fmt.Errorf("waiting for process to exit: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("non-zero exit code %d", res.ExitCode)
	}
	return res, nil
}

func (n *Node) MustRun(req clusteriface.StartProcRequest) *clusteriface.ProcessResult {
	pr, err := n.Run(req)
	Must(err)
	return pr
}

// RootDir returns the root directory of the node.
func (n *Node) RootDir() string {
	if rootDirer, ok := n.Node.(interface{ RootDir() string }); ok {
		return rootDirer.RootDir()
	}
	return "/"
}

func (n *Node) SendFile(filePath string, contents io.Reader) error {
	return n.Node.SendFile(n.Ctx, filePath, contents)
}

func (n *Node) MustSendFile(filePath string, contents io.Reader) {
	Must(n.SendFile(filePath, contents))
}

func (n *Node) ReadFile(filePath string) (io.ReadCloser, error) {
	return n.Node.ReadFile(n.Ctx, filePath)
}

func (n *Node) MustReadFile(filePath string) io.ReadCloser {
	r, err := n.ReadFile(filePath)
	Must(err)
	return r
}
