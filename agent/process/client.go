package process

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"

	clusteriface "github.com/guseggert/clustertest/cluster"
	"go.uber.org/zap"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

type Client struct {
	HTTPClient *http.Client
	URL        string
	Logger     *zap.SugaredLogger
}

type StartProcRequest struct {
	Command string
	Args    []string
	Env     []string
	WD      string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
}

type Process struct {
	wait func(ctx context.Context) (*clusteriface.ProcessResult, error)
}

func (p *Process) Wait(ctx context.Context) (*clusteriface.ProcessResult, error) { return p.wait(ctx) }

func (c *Client) StartProc(ctx context.Context, req StartProcRequest) (*Process, error) {
	c.Logger.Debugw("dialing WebSocket for run", "URL", c.URL)
	wsConn, _, err := websocket.Dial(ctx, c.URL, &websocket.DialOptions{
		HTTPClient:      c.HTTPClient,
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		c.Logger.Debugf("dial error: %s", err)
		return nil, fmt.Errorf("establishing WebSocket conn to run: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	runner := &clientProcRunner{
		conn:   wsConn,
		log:    c.Logger.Named("command_runner"),
		ctx:    ctx,
		cancel: cancel,
		req:    req,

		stdout: io.Discard,
		stderr: io.Discard,
		stdin:  req.Stdin,

		stdoutCh: make(chan []byte),
		stderrCh: make(chan []byte),

		resultCh: make(chan cmdResult, 1),
	}
	if req.Stdout != nil {
		runner.stdout = req.Stdout
	}
	if req.Stderr != nil {
		runner.stderr = req.Stderr
	}

	return runner.run()
}

type clientProcRunner struct {
	log    *zap.SugaredLogger
	conn   *websocket.Conn
	ctx    context.Context
	cancel func()
	req    StartProcRequest

	stderr io.Writer
	stdout io.Writer
	stdin  io.Reader

	stdoutCh chan []byte
	stderrCh chan []byte

	resultCh chan cmdResult

	wg sync.WaitGroup

	closeConnOnce sync.Once
}

func (r *clientProcRunner) shutdown() {
	r.cancel()
	r.wg.Wait()
}

func (r *clientProcRunner) run() (*Process, error) {
	r.wg.Add(2)
	go r.readStderr()
	go r.readStdout()

	err := r.writeFirstMessage()
	if err != nil {
		r.shutdown()
		return nil, fmt.Errorf("writing first message: %w", err)
	}

	r.wg.Add(2)
	go r.writeStdin()
	go r.readMessages()

	return &Process{
		wait: func(ctx context.Context) (*clusteriface.ProcessResult, error) {
			select {
			case res := <-r.resultCh:
				r.log.Debugf("got exit code %d with err: %s", res.code, res.err)
				return &clusteriface.ProcessResult{ExitCode: res.code, TimeMS: res.timeMS}, res.err
			case <-ctx.Done():
				err := ctx.Err()
				r.log.Debugf("wait context done: %s", err)
				return nil, err
			case <-r.ctx.Done():
				err := r.ctx.Err()
				r.log.Debugf("runResult context done: %s", err)
				return nil, err
			}
		},
	}, nil

}

func (r *clientProcRunner) close(code websocket.StatusCode, reason string) {
	// websocket reason can't be above 123 chars
	if len(reason) > 100 {
		reason = reason[0:100]
	}
	r.closeConnOnce.Do(func() {
		err := r.conn.Close(code, reason)
		if err != nil {
			r.log.Debugf("error closing conn: %s", err)
		}
	})
}

func (r *clientProcRunner) readMessages() {
	defer r.shutdown()
	defer r.wg.Done()

	closedStdout := false
	var closeStdoutOnce sync.Once
	closeStdout := func() {
		closeStdoutOnce.Do(func() {
			closedStdout = true
			close(r.stdoutCh)
		})
	}

	closedStderr := false
	var closeStderrOnce sync.Once
	closeStderr := func() {
		closeStderrOnce.Do(func() {
			closedStderr = true
			close(r.stderrCh)
		})
	}

	defer closeStderr()
	defer closeStdout()

	// The client always initiates the close when it decides that it's done.
	// Some important notes:
	//
	// The process wait will not return until stdout and stderr are read to completion,
	// which means that once we get an "exit" signal, no more stdout and stderr will be read.
	// This is a tradeoff to avoid having to buffer all the stdout in-memory on the server-side.
	// The downside here is that the client needs to read all stdout and stderr in order to get exit code.
	// If there's a lot of output, then that sucks. We can probably add client options
	// to tell the server how much, if any, of the output the client cares about, so the server knows how much to buffer.
	for {
		var msg procResponseMessage
		err := wsjson.Read(r.ctx, r.conn, &msg)
		if websocket.CloseStatus(err) != -1 {
			r.resultCh <- cmdResult{code: -1, err: fmt.Errorf("conn unexpectedly closed: %w", err)}
			closeStderr()
			closeStdout()
			return
		}
		if err != nil {
			r.log.Debugf("message reader got error: %s", err)
			r.resultCh <- cmdResult{err: err}
			r.close(websocket.StatusInternalError, err.Error())
			return
		}
		if len(msg.Stderr) > 0 && !closedStderr {
			r.stderrCh <- msg.Stderr
		}
		if msg.StderrDone {
			closeStderr()
		}
		if len(msg.Stdout) > 0 && !closedStdout {
			r.stdoutCh <- msg.Stdout
		}
		if msg.StdoutDone && !closedStdout {
			closeStdout()
		}
		if msg.Exited {
			r.resultCh <- cmdResult{code: msg.ExitCode, timeMS: msg.TimeMS}
			r.close(websocket.StatusNormalClosure, "")
			return
		}
	}
}

func (r *clientProcRunner) writeFirstMessage() error {
	return wsjson.Write(r.ctx, r.conn, procRequestMessage{
		Command: r.req.Command,
		Args:    r.req.Args,
		Env:     r.req.Env,
		WD:      r.req.WD,
	})
}

func (r *clientProcRunner) writeStdin() {
	defer r.wg.Done()
	writer := &wsJSONWriter{
		log:  r.log.Named("stdin_writer"),
		ctx:  r.ctx,
		conn: r.conn,
		writeMsg: func(b []byte) any {
			return procRequestMessage{Stdin: b}
		},
		closeMsg: func() any {
			return procRequestMessage{StdinDone: true}
		},
	}
	defer writer.Close()

	// caller supplied to stdin, this is fine, we just close it
	if r.stdin == nil {
		return
	}

	_, err := io.Copy(writer, r.stdin)
	r.log.Debugw("done copying stdin", "Error", err)
}

func (r *clientProcRunner) readStdout() {
	defer r.wg.Done()
	defer func() {
		if closer, ok := r.stdout.(io.Closer); ok {
			closer.Close()
		}
	}()
	for b := range r.stdoutCh {
		_, err := r.stdout.Write(b)
		if err != nil {
			r.log.Debugf("stdout reader got write error: %s", err)
			return
		}
	}
}

func (r *clientProcRunner) readStderr() {
	defer r.wg.Done()
	defer func() {
		if closer, ok := r.stderr.(io.Closer); ok {
			closer.Close()
		}
	}()
	for b := range r.stderrCh {
		_, err := r.stderr.Write(b)
		if err != nil {
			r.log.Debugf("stderr reader got write error: %s", err)
			return
		}
	}
}

type cmdResult struct {
	code   int
	timeMS int64
	err    error
}
