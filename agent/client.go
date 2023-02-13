package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"

	"net"
	"net/http"
	"os"
	"path"
	"sync"
	"time"

	"github.com/guseggert/clustertest/agent/process"
	clusteriface "github.com/guseggert/clustertest/cluster"
	"github.com/hashicorp/go-retryablehttp"
	"go.uber.org/zap"
	"nhooyr.io/websocket"
)

type Client struct {
	Logger     *zap.SugaredLogger
	HTTPClient *http.Client

	host                     string
	tlsClientConfig          *tls.Config
	dialCtx                  func(ctx context.Context, network, addr string) (net.Conn, error)
	baseURL                  string
	customizeRetryableClient func(*retryablehttp.Client)
	commandClient            *process.Client

	waitInterval time.Duration

	startHeartbeatOnce sync.Once
	stopHeartbeatOnce  sync.Once
	stopHeartbeat      chan struct{}
}

type ClientOption func(c *Client)

func WithClientWaitInterval(d time.Duration) ClientOption {
	return func(c *Client) {
		c.waitInterval = d
	}
}

func WithClientLogger(l *zap.Logger) ClientOption {
	return func(c *Client) {
		c.Logger = l.Named("nodeagentclient").Sugar()
	}
}

func WithCustomizeRetryableClient(f func(r *retryablehttp.Client)) ClientOption {
	return func(c *Client) {
		c.customizeRetryableClient = f
	}
}

type logAdapter struct {
	*zap.SugaredLogger
}

func (a *logAdapter) Printf(msg string, args ...interface{}) { a.Debugf(msg, args...) }

func NewClient(log *zap.SugaredLogger, certs *Certs, ipAddr string, port int, opts ...ClientOption) (*Client, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	httpDialAddrPort := fmt.Sprintf("%s:%d", ipAddr, port)

	// Don't do DNS lookup for dialing.
	// This prevents the default dialer from modifying the host header, which we need since we are not using public CAs.
	// Resulting behavior is that the addr host is used for the host header, but it does not resolve the name.
	// Rationale is that we don't need TLS for server authn, since we control all the hosts anyway.
	// We just want authz and encryption.
	dialCtx := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", httpDialAddrPort)
	}

	tlsConfig, err := ClientTLSConfig(certs.CA.CertPEMBytes, certs.Client.CertPEMBytes, certs.Client.KeyPEMBytes)
	if err != nil {
		return nil, fmt.Errorf("building client TLS config: %w", err)
	}

	baseURL := fmt.Sprintf("https://nodeagent:%d", port)
	commandURL := baseURL + "/command"

	c := &Client{
		Logger:          log.Named("nodeagent_client"),
		host:            "nodeagent",
		baseURL:         baseURL,
		tlsClientConfig: tlsConfig,
		dialCtx:         dialCtx,
		waitInterval:    100 * time.Millisecond,
		stopHeartbeat:   make(chan struct{}),
	}

	for _, opt := range opts {
		opt(c)
	}

	retryClient := retryablehttp.NewClient()
	retryClient.HTTPClient = &http.Client{
		Transport: &http.Transport{
			DialContext:     dialCtx,
			MaxConnsPerHost: 0,
			TLSClientConfig: tlsConfig,
		},
	}
	retryClient.Backoff = func(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
		return 10 * time.Millisecond
	}
	retryClient.RetryMax = 10
	retryClient.Logger = &logAdapter{SugaredLogger: log}

	if c.customizeRetryableClient != nil {
		c.customizeRetryableClient(retryClient)
	}

	c.HTTPClient = retryClient.StandardClient()
	c.commandClient = &process.Client{
		HTTPClient: c.HTTPClient,
		URL:        commandURL,
		Logger:     log.Named("nodeagent_command_client"),
	}

	return c, nil
}

func (c *Client) prepReq(r *http.Request) {
	r.Header.Add("Content-Type", "application/json")
	r.Close = true
}

func (c *Client) SendHeartbeat(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	u := fmt.Sprintf(c.baseURL + "/heartbeat")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		panic(err)
	}

	c.prepReq(req)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP error: %w", err)
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected heartbeat status code %d", resp.StatusCode)
	}
	return nil

}

func (c *Client) SendFile(ctx context.Context, filePath string, contents io.Reader) error {
	urlPath := path.Join("/file", filePath)
	u := c.baseURL + urlPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, contents)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	c.prepReq(httpReq)

	httpResp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("sending file over HTTP: %w", err)
	}
	if httpResp.Body != nil {
		defer httpResp.Body.Close()
	}
	if httpResp.StatusCode != http.StatusOK {
		var body string
		b, err := io.ReadAll(httpResp.Body)
		if err != nil {
			body = fmt.Errorf("error reading body: %w", err).Error()
		} else {
			body = string(b)
		}
		return fmt.Errorf("non-200 HTTP status code %d received when sending file: %s", httpResp.StatusCode, body)
	}
	return nil
}

// ReadFile reads a file from the remote node, returning io.ErrNotExist if it is not found.
func (c *Client) ReadFile(ctx context.Context, filePath string) (io.ReadCloser, error) {
	urlPath := path.Join("/file", filePath)
	u := c.baseURL + urlPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	c.prepReq(httpReq)

	httpResp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("reading file over HTTP: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		defer httpResp.Body.Close()
		if httpResp.StatusCode == http.StatusNotFound {
			return nil, os.ErrNotExist
		}
		var body string
		b, err := io.ReadAll(httpResp.Body)
		if err != nil {
			body = fmt.Errorf("error reading body: %w", err).Error()
		} else {
			body = string(b)
		}
		return nil, fmt.Errorf("non-200 HTTP status code %d received when reading file: %s", httpResp.StatusCode, body)
	}

	return httpResp.Body, nil
}

func (c *Client) StartProc(ctx context.Context, runReq clusteriface.StartProcRequest) (clusteriface.Process, error) {
	return c.commandClient.StartProc(ctx, process.StartProcRequest{
		Command: runReq.Command,
		Args:    runReq.Args,
		Env:     runReq.Env,
		WD:      runReq.WD,
		Stdin: process.InputFD{
			Reader: runReq.Stdin,
			File:   runReq.StdinFile,
		},
		Stdout: process.OutputFD{
			Writer: runReq.Stdout,
			File:   runReq.StdoutFile,
		},
		Stderr: process.OutputFD{
			Writer: runReq.Stderr,
			File:   runReq.StderrFile,
		},
	})
}

func (c *Client) Fetch(ctx context.Context, url, path string) error {
	fetchReq := FetchRequest{
		URL:  url,
		Dest: path,
	}
	u := c.baseURL + "/fetch"
	b, err := json.Marshal(fetchReq)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return err
	}

	c.prepReq(httpReq)

	httpResp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return err
	}
	if httpResp.StatusCode != http.StatusOK {
		defer httpResp.Body.Close()
		var body string
		b, err := io.ReadAll(httpResp.Body)
		if err != nil {
			body = fmt.Errorf("error reading body: %w", err).Error()
		} else {
			body = string(b)
		}
		return fmt.Errorf("non-200 HTTP status code %d received when fetching: %s", httpResp.StatusCode, body)
	}
	return nil
}

// Dial establishes a connection to the given address, using the node as a proxy.
func (c *Client) Dial(network, addr string) (net.Conn, error) {
	return c.DialContext(context.Background(), network, addr)
}

// DialContext establishes a connection to the given address using the given network type, tunneled through a WebSocket connection with the node.
func (c *Client) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	u := c.baseURL + fmt.Sprintf("/connect/%s/%s", network, addr)

	c.Logger.Debugw("dialing WebSocket", "URL", u)
	wsConn, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c.HTTPClient})
	if err != nil {
		return nil, fmt.Errorf("dialing WebSocket conn: %w", err)
	}

	return websocket.NetConn(ctx, wsConn, websocket.MessageBinary), nil
}

func (c *Client) WaitForServer(ctx context.Context) error {
	ticker := time.NewTicker(c.waitInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			err := c.SendHeartbeat(ctx)
			if err == nil {
				c.Logger.Debug("heartbeat succeeded, done waiting for server")
				return nil
			}
			c.Logger.Debugf("got heartbeat error: %s", err)
		}
	}
}

func (n *Client) StartHeartbeat() {
	go n.startHeartbeatOnce.Do(func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-n.stopHeartbeat:
				return
			case <-ticker.C:
			}
			err := n.SendHeartbeat(context.Background())
			if err != nil {
				n.Logger.Debugf("heartbeat error: %s", err)
			}
		}
	})
}

func (n *Client) StopHeartbeat() {
	n.stopHeartbeatOnce.Do(func() { close(n.stopHeartbeat) })
}
