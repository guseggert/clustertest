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
func NewNodeAgent(caCertPEM, certPEM, keyPEM []byte, opts ...Option) (*NodeAgent, error) {
	logger, err := zap.NewDevelopment()
	if err != nil {
		return nil, fmt.Errorf("building logger: %w", err)
	}
	n := &NodeAgent{
		logger:           logger.Named("nodeagent").Sugar(),
		commandServer:    &process.Server{Log: logger.Named("command_server").Sugar()},
		caCertPEM:        caCertPEM,
		certPEM:          certPEM,
		keyPEM:           keyPEM,
		heartbeatTimeout: 1 * time.Minute,
		listenAddr:       "0.0.0.0:8080",
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
	tcpListener, err := net.Listen("tcp", a.listenAddr)
	if err != nil {
		return fmt.Errorf("listening TCP: %w", err)
	}

	tlsConfig, err := ServerTLSConfig(a.caCertPEM, a.certPEM, a.keyPEM)
	if err != nil {
		return fmt.Errorf("building server TLS config: %w", err)
	}

	tlsListener := tls.NewListener(tcpListener, tlsConfig)

	router := httprouter.New()
	router.GET("/heartbeat", a.heartbeat)
	router.GET("/command", a.commandWS)
	router.POST("/command", a.command)
	router.POST("/file/*path", a.postFile)
	router.GET("/file/*path", a.readFile)
	router.GET("/connect/:network/:addr", a.connect)

	server := http.Server{Handler: router}
	a.httpServer = &server

	err = server.Serve(tlsListener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err
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
