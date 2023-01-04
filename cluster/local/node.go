package local

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"

	clusteriface "github.com/guseggert/clustertest/cluster"
)

type node struct {
	ID  int
	Env map[string]string
}

type result struct {
	code int
	err  error
}

func (n *node) Run(ctx context.Context, req clusteriface.RunRequest) (clusteriface.RunResultWaiter, error) {
	cmd := exec.Command(req.Command, req.Args...)
	if len(req.Env) > 0 {
		cmd.Env = append(os.Environ(), req.Env...)
	}
	cmd.Stdin = req.Stdin
	cmd.Stdout = req.Stdout
	cmd.Stderr = req.Stderr

	err := cmd.Start()
	if err != nil {
		return nil, fmt.Errorf("running command: %w", err)
	}

	// wait on the process to finish and send the result
	resultChan := make(chan result, 1)
	procExitedChan := make(chan struct{})
	go func() {
		exitCode := -1
		var resultErr error

		err := cmd.Wait()
		close(procExitedChan)
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				resultErr = err
			}
		}
		select {
		case <-ctx.Done():
			return
		case resultChan <- result{code: exitCode, err: resultErr}:
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

	return func(ctx context.Context) (int, error) {
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case res := <-resultChan:
			return res.code, res.err
		}
	}, nil
}

func (n *node) SendFile(ctx context.Context, req clusteriface.SendFileRequest) error {
	dir := filepath.Dir(req.FilePath)
	err := os.MkdirAll(dir, 0777)
	if err != nil {
		return fmt.Errorf("making intermediate dirs: %w", err)
	}

	f, err := os.Create(req.FilePath)
	if err != nil {
		return fmt.Errorf("creating file %q: %w", req.FilePath, err)
	}

	_, err = io.Copy(f, req.Contents)

	return err
}

func (n *node) Connect(ctx context.Context, req clusteriface.ConnectRequest) (net.Conn, error) {
	return nil, nil
}

func (n *node) Stop(ctx context.Context) error {
	return nil
}

func (n *node) String() string {
	return fmt.Sprintf("local node id=%d", n.ID)
}
