package agent

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/guseggert/clustertest/agent/process"
	"github.com/julienschmidt/httprouter"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"nhooyr.io/websocket"
)

// NodeAgent is an HTTP agent that runs on each node.
// The agent requires mTLS for both traffic encryption and authz.
type NodeAgent struct {
	logger *zap.SugaredLogger

	caCertPEM []byte
	certPEM   []byte
	keyPEM    []byte

	heartbeatFailureHandler func()
	heartbeatTimeout        time.Duration
	listenAddr              string

	httpServer    *http.Server
	commandServer *process.Server

	closed        chan struct{}
	heartbeatMut  sync.Mutex
	lastHeartbeat time.Time

	procsMut sync.Mutex
	procs    map[string]*proc
}

type Option func(n *NodeAgent)

func WithHeartbeatTimeout(d time.Duration) Option {
	return func(n *NodeAgent) {
		n.heartbeatTimeout = d
	}
}

func WithHeartbeatFailureHandler(f func()) Option {
	return func(n *NodeAgent) {
		n.heartbeatFailureHandler = f
	}
}

func WithListenAddr(s string) Option {
	return func(n *NodeAgent) {
		n.listenAddr = s
	}
}

func WithLogger(l *zap.Logger) Option {
	return func(n *NodeAgent) {
		n.logger = l.Sugar()
	}
}

func WithLogLevel(l zapcore.Level) Option {
	return func(n *NodeAgent) {
		n.logger = n.logger.WithOptions(zap.IncreaseLevel(l))
	}
}

func WithCerts(caCertPEM, certPEM, keyPEM []byte) Option {
	return func(n *NodeAgent) {
		n.caCertPEM = caCertPEM
		n.certPEM = certPEM
		n.keyPEM = keyPEM
	}
}

func HeartbeatFailureShutdown() {
	fmt.Println("heartbeat failed, shutting down")
	cmd := exec.Command("shutdown", "now")
	err := cmd.Run()
	if err != nil {
		fmt.Printf("unable to shutdown host: %s", err)
	}
}

func HeartbeatFailureExit() {
	fmt.Println("heartbeat failed, exiting")
	os.Exit(1)
}

// NewNodeAgent constructs a new host agent.
func NewNodeAgent(opts ...Option) (*NodeAgent, error) {
	logger, err := zap.NewDevelopment()
	if err != nil {
		return nil, fmt.Errorf("building logger: %w", err)
	}
	n := &NodeAgent{
		logger:           logger.Named("nodeagent").Sugar(),
		commandServer:    &process.Server{Log: logger.Named("command_server").Sugar()},
		heartbeatTimeout: 1 * time.Minute,
		listenAddr:       "0.0.0.0:8080",
		procs:            map[string]*proc{},
	}
	for _, o := range opts {
		o(n)
	}
	return n, nil
}

// startHeartbeatCheck starts a goroutine that checks for a heartbeat timeout and shuts down the node when a timeout occurs.
func (a *NodeAgent) startHeartbeatCheck() {
	go func() {
		a.heartbeatMut.Lock()
		a.lastHeartbeat = time.Now()
		a.heartbeatMut.Unlock()

		for {
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			select {
			case <-a.closed:
				return
			case <-ticker.C:
			}

			a.heartbeatMut.Lock()
			lastHeartbeat := a.lastHeartbeat
			a.heartbeatMut.Unlock()

			if lastHeartbeat.Add(a.heartbeatTimeout).Before(time.Now()) {
				if a.heartbeatFailureHandler != nil {
					a.heartbeatFailureHandler()
				}
			}
		}
	}()
}

func (a *NodeAgent) runHTTPServer() error {
	var listener net.Listener

	tcpListener, err := net.Listen("tcp", a.listenAddr)
	if err != nil {
		return fmt.Errorf("listening TCP: %w", err)
	}
	listener = tcpListener

	if a.caCertPEM != nil && a.certPEM != nil && a.keyPEM != nil {
		tlsConfig, err := ServerTLSConfig(a.caCertPEM, a.certPEM, a.keyPEM)
		if err != nil {
			listener.Close()
			return fmt.Errorf("building server TLS config: %w", err)
		}
		listener = tls.NewListener(tcpListener, tlsConfig)
	} else if a.caCertPEM != nil || a.certPEM != nil || a.keyPEM != nil {
		listener.Close()
		return errors.New("refusing to start with insecure configuration, either specify all TLS config or none")
	}

	router := httprouter.New()
	router.GET("/heartbeat", a.heartbeat)
	router.POST("/proc/:name", a.procStart)
	router.POST("/proc", a.command)
	router.POST("/proc/:name/signal", a.procSignal)
	router.POST("/proc/:name/stdin", a.procStdin)
	router.GET("/proc/:name/stdout", a.procStdout)
	router.GET("/proc/:name/stderr", a.procStderr)
	router.GET("/proc/:name/wait", a.procWait)
	router.DELETE("/proc/:name", a.procKill)
	router.GET("/command", a.commandWS)
	router.POST("/command", a.command)
	router.POST("/file/*path", a.postFile)
	router.GET("/file/*path", a.readFile)
	router.GET("/connect/:network/:addr", a.connect)
	router.POST("/fetch", a.fetch)

	handler := a.logHandler(router)

	server := http.Server{Handler: handler}
	a.httpServer = &server

	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		a.logger.Info("server closed gracefully")
		return nil
	}
	a.logger.Infof("server closed abnormally: %s", err)

	return err
}

func (a *NodeAgent) logHandler(h http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		a.logger.Debugw(
			"received request",
			"Time", time.Now(),
			"URL", r.URL,
			"Method", r.Method,
		)

		h.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

// Run runs the node agent and returns once the node agent has stopped.
func (a *NodeAgent) Run() error {
	a.startHeartbeatCheck()
	return a.runHTTPServer()
}

type ConnectRequest struct {
	Addr    string
	Network string
}

type FetchRequest struct {
	URL  string
	Dest string
}

type StartProcRequest struct {
	Command string
	Args    []string
	Env     []string
	WD      string

	// Indicates if the process should wait for stdin to arrive at the stdin endpoint.
	WillWriteStdin bool
	// StdinFile is the file to read into stdin. If this is specified, the value of WillWriteStdin is ignored.
	StdinFile string
	// Stdin is the string to send to stdin. If this is specified, the values of StdinFile and WillWriteStdin are ignored.
	Stdin string

	// Stdout indicates if the caller wants to access stdout via the stdout endpoint.
	// When this is specified, the process will block on reading stdout.
	WillReadStdout bool
	// StdoutFile is the file to write stdout to. If this is specified, the value of the Stdout field is ignored.
	StdoutFile string

	// Stderr indicates if the caller wants to access stdout via the stdout endpoint.
	// When this is specified, the process will block on reading stderr.
	WillReadStderr bool
	// StderrFile is the file to write stdout to. If this is specified, the value of the Stderr field is ignored.
	StderrFile string
}

type StartProcResponse struct {
	// PID is the PID of the process.
	PID int
}

type proc struct {
	err error

	cmd    *exec.Cmd
	waitCh chan struct{}

	stdinHasWriter atomic.Bool
	stdinW         io.Writer
	stdinR         io.ReadCloser

	stderrMW *multiWriter
	stderrF  io.WriteCloser
	stdoutMW *multiWriter
	stdoutF  io.WriteCloser

	waiters []chan int
}

func (a *NodeAgent) procStart(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	name := params.ByName("name")
	a.procsMut.Lock()
	defer a.procsMut.Unlock()

	if _, ok := a.procs[name]; ok {
		http.Error(w, fmt.Sprintf("process already exists with name %q", name), http.StatusBadRequest)
		return
	}

	var req StartProcRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cmd := exec.Command(req.Command, req.Args...)
	cmd.Env = req.Env
	cmd.Dir = req.WD

	proc := &proc{
		cmd:    cmd,
		waitCh: make(chan struct{}),
	}

	if req.Stdin != "" {
		cmd.Stdin = bytes.NewReader([]byte(req.Stdin))
	} else if req.StdinFile != "" {
		f, err := os.Open(req.StdinFile)
		if err != nil {
			http.Error(w, fmt.Sprintf("opening stdin file: %s", err.Error()), http.StatusBadRequest)
			return
		}
		proc.stdinR = f
	} else if req.WillWriteStdin {
		proc.stdinR, proc.stdinW = io.Pipe()
		cmd.Stdin = proc.stdinR
	}

	if req.StdoutFile != "" {
		f, err := os.Create(req.StdoutFile)
		if err != nil {
			http.Error(w, fmt.Sprintf("creating stdout file: %s", err.Error()), http.StatusBadRequest)
			return
		}
		proc.stdoutF = f
	} else if req.WillReadStdout {
		proc.stdoutMW = &multiWriter{}
		cmd.Stdout = proc.stdoutMW
	}

	if req.StderrFile != "" {
		f, err := os.Create(req.StderrFile)
		if err != nil {
			http.Error(w, fmt.Sprintf("creating stderr file: %s", err.Error()), http.StatusBadRequest)
			return
		}
		proc.stderrF = f
	} else if req.WillReadStderr {
		proc.stderrMW = &multiWriter{}
		cmd.Stderr = proc.stderrMW
	}

	err = cmd.Start()
	if err != nil {
		http.Error(w, fmt.Sprintf("starting process: %s", err.Error()), http.StatusBadRequest)
		return
	}

	// clean up after the process exits
	// multiwriters don't need to be closed, each goroutine can just wait on the proc to exit before returning
	go func() {
		cmd.Wait()
		close(proc.waitCh)
		if proc.stderrF != nil {
			proc.stderrF.Close()
		}
		if proc.stdoutF != nil {
			proc.stdoutF.Close()
		}
		if proc.stdinR != nil {
			proc.stdinR.Close()
		}
	}()

	a.procs[name] = proc

	resp := StartProcResponse{PID: cmd.Process.Pid}

	b, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, err = w.Write(b)
}

type SignalProcRequest struct {
	Type string
}

func (a *NodeAgent) procSignal(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	name := params.ByName("name")
	a.procsMut.Lock()
	defer a.procsMut.Unlock()
	proc, ok := a.procs[name]
	if !ok {
		http.Error(w, fmt.Sprintf("no process exists with name %q", name), http.StatusBadRequest)
		return
	}

	var req SignalProcRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var sig os.Signal
	switch req.Type {
	case "kill":
		sig = os.Kill
	case "interrupt":
		sig = os.Interrupt
	default:
		http.Error(w, fmt.Sprintf("unsupported signal type %q", req.Type), http.StatusBadRequest)
		return
	}

	err = proc.cmd.Process.Signal(sig)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}
func (a *NodeAgent) procStdout(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	name := params.ByName("name")
	a.procsMut.Lock()
	proc, ok := a.procs[name]
	if !ok {
		a.procsMut.Unlock()
		http.Error(w, fmt.Sprintf("no process exists with name %q", name), http.StatusBadRequest)
		return
	}
	if proc.stdoutMW == nil {
		a.procsMut.Unlock()
		http.Error(w, fmt.Sprintf("process %q doesn't support reading stdout", name), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	proc.stdoutMW.Add(w)
	a.procsMut.Unlock()

	<-proc.waitCh
}

func (a *NodeAgent) procStderr(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	name := params.ByName("name")
	a.procsMut.Lock()
	proc, ok := a.procs[name]
	if !ok {
		a.procsMut.Unlock()
		http.Error(w, fmt.Sprintf("no process exists with name %q", name), http.StatusBadRequest)
		return
	}
	if proc.stderrMW == nil {
		a.procsMut.Unlock()
		http.Error(w, fmt.Sprintf("process %q doesn't support reading stderr", name), http.StatusBadRequest)
		return
	}
	proc.stderrMW.Add(w)
	a.procsMut.Unlock()

	<-proc.waitCh
}
func (a *NodeAgent) procStdin(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	name := params.ByName("name")
	a.procsMut.Lock()
	proc, ok := a.procs[name]
	a.procsMut.Unlock()
	if !ok {
		http.Error(w, fmt.Sprintf("no process exists with name %q", name), http.StatusBadRequest)
		return
	}
	if proc.stdinW == nil {
		http.Error(w, fmt.Sprintf("process %q doesn't support writing stdin", name), http.StatusBadRequest)
	}

	if proc.stdinHasWriter.CompareAndSwap(false, true) {
		http.Error(w, fmt.Sprintf("process %q already has a stdin writer", name), http.StatusBadRequest)
	}

	_, err := io.Copy(proc.stdinW, r.Body)
	if err != nil {
		// at this point we're in an unrecoverable state, so whack the process
		proc.cmd.Process.Kill()
		proc.err = fmt.Errorf("reading stdin: %w", err)
		return
	}
}

type WaitProcResponse struct {
	Code int
}

func (a *NodeAgent) procWait(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	name := params.ByName("name")
	a.procsMut.Lock()
	proc, ok := a.procs[name]
	a.procsMut.Unlock()
	if !ok {
		http.Error(w, fmt.Sprintf("no process exists with name %q", name), http.StatusBadRequest)
		return
	}
	<-proc.waitCh
	b, err := json.Marshal(WaitProcResponse{Code: proc.cmd.ProcessState.ExitCode()})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Write(b)
}

func (a *NodeAgent) procKill(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	name := params.ByName("name")
	a.procsMut.Lock()
	defer a.procsMut.Unlock()
	proc, ok := a.procs[name]
	if !ok {
		a.procsMut.Unlock()
		http.Error(w, fmt.Sprintf("no process exists with name %q", name), http.StatusBadRequest)
		return
	}
	_ = proc.cmd.Process.Kill()
	delete(a.procs, name)
}

func (a *NodeAgent) fetch(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	var req FetchRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.Create(req.Dest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, req.URL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	_, err = io.Copy(f, resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// connect proxies traffic to a destination through the agent, via a WebSocket connection
func (a *NodeAgent) connect(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	network := params.ByName("network")
	addr := params.ByName("addr")

	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		a.logger.Debugf("connect WebSocket accept error: %s", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	remoteConn := websocket.NetConn(r.Context(), wsConn, websocket.MessageBinary)

	localConn, err := net.DialTimeout(network, addr, 5*time.Second)
	if err != nil {
		a.logger.Debugf("connect dial error: %s", err)
		remoteConn.Close()
		return
	}

	go func() {
		defer remoteConn.Close()
		defer localConn.Close()
		_, err := io.Copy(localConn, remoteConn)
		if err != nil {
			a.logger.Debugf("connect copy to local error: %s", err)
		}
	}()
	_, err = io.Copy(remoteConn, localConn)
	if err != nil {
		a.logger.Debugf("connect copy to remote error: %s", err)
	}
}

func (a *NodeAgent) postFile(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	path := params.ByName("path")

	dir := filepath.Dir(path)
	err := os.MkdirAll(dir, 0777)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	f, err := os.Create(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	_, err = io.Copy(f, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
func (a *NodeAgent) readFile(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	path := params.ByName("path")

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "no such file or directory", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = io.Copy(w, f)
	if err != nil {
		a.logger.Debugf("error sending file response: %s", err)
	}
}

func (a *NodeAgent) heartbeat(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	a.heartbeatMut.Lock()
	lastHeartbeat := a.lastHeartbeat
	a.lastHeartbeat = time.Now()
	a.heartbeatMut.Unlock()
	response := struct {
		LastHeartbeat string
	}{
		LastHeartbeat: lastHeartbeat.UTC().Format(time.RFC3339),
	}
	b, err := json.Marshal(response)
	if err != nil {
		a.logger.Debugf("error marshaling heartbeat response: %s", err)
	}
	w.Header().Add("Content-Type", "application/json")
	w.Write(b)
}

type PostCommandRequest struct {
	Command    string
	Args       []string
	Stdin      string
	Env        []string
	WorkingDir string
}

type PostCommandResponse struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

func (a *NodeAgent) commandWS(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	a.commandServer.ServeHTTP(w, r)
}

// command is a simple command runner which takes a stdin buffer and sends all of stdout and stderr in the response.
// This is much easier to curl and write simple clients against, but doesn't support streaming input & output.
func (a *NodeAgent) command(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	var req PostCommandRequest
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(&req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Command == "" {
		http.Error(w, "request contained no command", http.StatusBadRequest)
		return
	}

	cmd := exec.Command(req.Command, req.Args...)
	if req.WorkingDir != "" {
		cmd.Dir = req.WorkingDir
	}
	cmd.Env = append(cmd.Env, req.Env...)
	stderr := &bytes.Buffer{}
	stdout := &bytes.Buffer{}
	cmd.Stderr = stderr
	cmd.Stdout = stdout

	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	}

	err = cmd.Start()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// If the request is aborted, kill the process.
	// In the normal case, this is a no-op as the process will already be finished when the context is done.
	go func() {
		<-r.Context().Done()
		cmd.Process.Kill()
	}()

	cmd.Wait()

	resp := PostCommandResponse{
		ExitCode: cmd.ProcessState.ExitCode(),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}
	b, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write(b)
}

func (a *NodeAgent) Stop() error {
	return a.httpServer.Close()
}
