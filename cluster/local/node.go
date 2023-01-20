package local

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	clusteriface "github.com/guseggert/clustertest/cluster"
)

type Node struct {
	ID  int
	Env map[string]string
	Dir string
}

type result struct {
	code   int
	timeMS int64
	err    error
}

type proc struct {
	wait func(context.Context) (*clusteriface.ProcessResult, error)
}

func (p *proc) Wait(ctx context.Context) (*clusteriface.ProcessResult, error) { return p.wait(ctx) }

func (n *Node) StartProc(ctx context.Context, req clusteriface.StartProcRequest) (clusteriface.Process, error) {
	cmd := exec.Command(req.Command, req.Args...)
	if len(req.Env) > 0 {
		cmd.Env = append(os.Environ(), req.Env...)
	}
	cmd.Stdin = req.Stdin
	cmd.Stdout = req.Stdout
	cmd.Stderr = req.Stderr
	cmd.Dir = req.WD

	start := time.Now()
	err := cmd.Start()
	if err != nil {
		return nil, fmt.Errorf("running command: %w", err)
	}

	// wait on the process to finish and send the result
	resultChan := make(chan result, 1)
	procExitedChan := make(chan struct{})
	go func() {
		exitCode := 0
		var resultErr error

		err := cmd.Wait()
		timeMS := time.Since(start).Milliseconds()
		close(procExitedChan)
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				resultErr = err
				exitCode = -1
			}
		}
		select {
		case <-ctx.Done():
			return
		case resultChan <- result{code: exitCode, timeMS: timeMS, err: resultErr}:
		}

	}()

	// kill the process if the context is canceled
	go func() {
		select {
		case <-ctx.Done():
			cmd.Process.Kill()
		case <-procExitedChan:
		}
	}()

	return &proc{
		wait: func(ctx context.Context) (*clusteriface.ProcessResult, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case res := <-resultChan:
				return &clusteriface.ProcessResult{ExitCode: res.code, TimeMS: res.timeMS}, res.err
			}
		},
	}, nil
}

func (n *Node) SendFile(ctx context.Context, filePath string, contents io.Reader) error {
	dir := filepath.Dir(filePath)
	err := os.MkdirAll(dir, 0777)
	if err != nil {
		return fmt.Errorf("making intermediate dirs: %w", err)
	}

	f, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("creating file %q: %w", filePath, err)
	}
	defer f.Close()

	_, err = io.Copy(f, contents)

	return err
}

func (n *Node) ReadFile(ctx context.Context, path string) (io.ReadCloser, error) {
	return os.Open(path)
}

func (n *Node) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return net.Dial(network, addr)
}

func (n *Node) Stop(ctx context.Context) error {
	return nil
}

func (n *Node) String() string {
	return fmt.Sprintf("local node id=%d", n.ID)
}

func (n *Node) RootDir() string {
	return n.Dir
}
