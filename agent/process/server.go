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

	stderr io.ReadCloser
	stdout io.ReadCloser

	stdin   io.WriteCloser
	stdinCh chan []byte

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
		if len(msg.Stdin) > 0 && !closedStdin {
			r.stdinCh <- msg.Stdin
		}
		if msg.StdinDone && !closedStdin {
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

	r.log.Debugf("process %d exited with error code %d, sending message", r.cmd.Process.Pid, r.cmd.ProcessState.ExitCode())
	err = wsjson.Write(r.ctx, r.conn, procResponseMessage{
		Exited:   true,
		ExitCode: exitCode,
		TimeMS:   timeMS,
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

	cmd := exec.Command(req.Command, req.Args...)
	cmd.Dir = req.WD
	if len(req.Env) > 0 {
		cmd.Env = append(os.Environ(), req.Env...)
	}

	cmd.Stderr = &wsJSONWriter{
		log:  r.log.Named("stderr_writer"),
		ctx:  r.ctx,
		conn: r.conn,
		writeMsg: func(b []byte) any {
			return procResponseMessage{Stderr: b}
		},
	}

	cmd.Stdout = &wsJSONWriter{
		log:  r.log.Named("stdout_writer"),
		ctx:  r.ctx,
		conn: r.conn,
		writeMsg: func(b []byte) any {
			return procResponseMessage{Stdout: b}
		},
	}

	stdinR, stdinW := io.Pipe()
	cmd.Stdin = stdinR
	r.stdin = stdinW

	r.cmd = cmd

	return time.Now(), cmd.Start()
}

func (r *serverProcRunner) readStdin() {
	defer r.wg.Done()
	defer r.stdin.Close()
	for b := range r.stdinCh {
		_, err := r.stdin.Write(b)
		if err != nil {
			r.log.Debugf("stdin reader got write error: %s", err)
			return
		}
	}
}
