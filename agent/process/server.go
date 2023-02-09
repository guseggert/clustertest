package process

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

type Server struct {
	Log *zap.SugaredLogger
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		s.Log.Debugf("error accepting WebSocket conn: %s", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	s.Log.Debug("accepted WebSocket conn")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	runner := &serverProcRunner{
		log:     s.Log.Named("server_runner"),
		conn:    wsConn,
		ctx:     ctx,
		cancel:  cancel,
		stdinCh: make(chan []byte),
	}
	runner.run()
}

type serverProcRunner struct {
	log    *zap.SugaredLogger
	conn   *websocket.Conn
	ctx    context.Context
	cancel func()

	cmd *exec.Cmd

	stderrCloser io.Closer
	stdoutCloser io.Closer

	// stdinWriter is the writer for stdin, if piping stdin is enabled.
	stdinWriter io.Writer
	// stdinCloser is the closer for stdin (could be either a pipe or a file).
	stdinCloser io.Closer
	stdinCh     chan []byte

	wg sync.WaitGroup

	closeConnOnce sync.Once
}

func (r *serverProcRunner) shutdown() {
	if r.cmd.Process != nil {
		r.cmd.Process.Kill()
	}
	r.cancel()
	r.wg.Wait()
}

func (r *serverProcRunner) run() {
	// read the first message
	startTime, err := r.readFirstMessageAndStart()
	if err != nil {
		r.log.Debugf("error reading first message: %s", err)
		r.conn.Close(websocket.StatusInternalError, fmt.Sprintf("reading first message: %s", err))
		r.shutdown()
		return
	}
	r.log.Debug("process started")

	r.wg.Add(3)
	go r.readMessages()
	go r.readStdin()
	go r.waitAndWriteResult(startTime)

	r.wg.Wait()
}

func (r *serverProcRunner) close(code websocket.StatusCode, reason string) {
	r.closeConnOnce.Do(func() {
		err := r.conn.Close(code, reason)
		if err != nil {
			r.log.Debugf("error closing conn: %s", err)
		}
	})
}

func (r *serverProcRunner) readMessages() {
	defer r.shutdown()
	defer r.wg.Done()

	closedStdin := false

	for {
		var msg procRequestMessage
		err := wsjson.Read(r.ctx, r.conn, &msg)
		if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
			r.log.Debug("got normal closure from client, wrapping up")
			if !closedStdin {
				close(r.stdinCh)
			}
			return
		}
		if err != nil {
			r.log.Debugf("message reader got error: %s", err)
			if !closedStdin {
				close(r.stdinCh)
			}
			r.close(websocket.StatusInternalError, err.Error())
			return
		}
		if len(msg.Stdin.B) > 0 && !closedStdin {
			r.stdinCh <- msg.Stdin.B
		}
		if msg.Stdin.Done && !closedStdin {
			fmt.Printf("stdin done\n")
			close(r.stdinCh)
			closedStdin = true
		}
		if msg.Signal != 0 {
			var sig os.Signal
			switch msg.Signal {
			case syscall.SIGINT:
				sig = os.Interrupt
			case syscall.SIGKILL:
				sig = os.Kill
			default:
				r.log.Debugf("unknown signal type %d, ignoring", msg.Signal)
			}
			_ = r.cmd.Process.Signal(sig)
		}
	}
}

func (r *serverProcRunner) waitAndWriteResult(startTime time.Time) {
	defer r.wg.Done()

	err := r.cmd.Wait()
	timeMS := time.Since(startTime).Milliseconds()

	exitCode := r.cmd.ProcessState.ExitCode()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			r.log.Debugf("unexpected exit error: %s", err)
		}
	}

	// ensure output streams are closed and ready to read before sending the message
	// this is safe; it is guaranteed that no writes will happen after the process exits
	if err := r.stderrCloser.Close(); err != nil {
		r.log.Warnf("error closing stderr: %s", err)
	}
	if err := r.stdoutCloser.Close(); err != nil {
		r.log.Warnf("error closing stdout: %s", err)
	}

	r.log.Debugf("process %d exited with error code %d, sending message", r.cmd.Process.Pid, r.cmd.ProcessState.ExitCode())
	err = wsjson.Write(r.ctx, r.conn, procResponseMessage{
		Result: procResult{
			Exited:   true,
			ExitCode: exitCode,
			TimeMS:   timeMS,
		},
	})
	if err != nil {
		r.log.Debugf("error sending exit code: %s", err)
	}
}

func (r *serverProcRunner) readFirstMessageAndStart() (time.Time, error) {
	var req procRequestMessage
	err := wsjson.Read(r.ctx, r.conn, &req)
	if err != nil {
		return time.Time{}, err
	}
	r.log.Debugw("got first message", "Message", req)

	cmd := exec.Command(req.Req.Command, req.Req.Args...)
	cmd.Dir = req.Req.WD
	if len(req.Req.Env) > 0 {
		cmd.Env = append(os.Environ(), req.Req.Env...)
	}

	if req.Req.Stdin.File != "" {
		fmt.Printf("opening %s\n", req.Req.Stdin.File)
		f, err := os.Open(req.Req.Stdin.File)
		if err != nil {
			return time.Time{}, fmt.Errorf("opening stdin file %q: %w", req.Req.Stdin.File, err)
		}
		r.stdinCloser = f
		r.stdinWriter = io.Discard // we don't write directly to stdin in this case, it happens internally in the stdlib
		cmd.Stdin = f
	} else if req.Req.Stdin.Discard {
		r.stdinWriter = io.Discard
		r.stdinCloser = &noopWriteCloser{io.Discard}
	} else {
		stdinR, stdinW := io.Pipe()
		r.stdinWriter = stdinW
		r.stdinCloser = stdinW
		cmd.Stdin = stdinR
	}

	if req.Req.Stdout.File != "" {
		f, err := os.Create(req.Req.Stdout.File)
		if err != nil {
			return time.Time{}, fmt.Errorf("opening stdout file %q: %w", req.Req.Stdout.File, err)
		}
		r.stdoutCloser = f
		cmd.Stdout = f
	} else if req.Req.Stdout.Discard {
		r.stdoutCloser = &noopWriteCloser{io.Discard}
		cmd.Stdout = io.Discard
	} else {
		w := &wsJSONWriter{
			log:  r.log.Named("stdout_writer"),
			ctx:  r.ctx,
			conn: r.conn,
			writeMsg: func(b []byte) any {
				return procResponseMessage{Stdout: fdPayload{B: b}}
			},
		}
		r.stdoutCloser = w
		cmd.Stdout = w
	}

	if req.Req.Stderr.File != "" {
		f, err := os.Create(req.Req.Stderr.File)
		if err != nil {
			return time.Time{}, fmt.Errorf("opening stderr file %q: %w", req.Req.Stderr.File, err)
		}
		r.stderrCloser = f
		cmd.Stderr = f
	} else if req.Req.Stderr.Discard {
		r.stderrCloser = &noopWriteCloser{io.Discard}
		cmd.Stderr = io.Discard
	} else {
		w := &wsJSONWriter{
			log:  r.log.Named("stderr_writer"),
			ctx:  r.ctx,
			conn: r.conn,
			writeMsg: func(b []byte) any {
				return procResponseMessage{Stderr: fdPayload{B: b}}
			},
		}
		r.stderrCloser = w
		cmd.Stderr = w
	}

	r.cmd = cmd

	return time.Now(), cmd.Start()
}

func (r *serverProcRunner) readStdin() {
	defer r.wg.Done()
	if r.stdinWriter == nil {
		return
	}

	defer r.stdinCloser.Close()
	for b := range r.stdinCh {
		_, err := r.stdinWriter.Write(b)
		if err != nil {
			r.log.Debugf("stdin reader got write error: %s", err)
			return
		}
	}
}
