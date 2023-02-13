package agent

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"testing"

	"github.com/guseggert/clustertest/cluster"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

var (
	log *zap.SugaredLogger
)

func init() {
	l, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}

	log = l.Sugar()
}

func TestNegativeAuthz(t *testing.T) {
	// ensure that unauthorized clients are rejected
	serverCerts, err := GenerateCerts()
	require.NoError(t, err)
	agent, err := NewNodeAgent(
		serverCerts.CA.CertPEMBytes,
		serverCerts.Server.CertPEMBytes,
		serverCerts.Server.KeyPEMBytes,
		WithListenAddr("127.0.0.1:9998"),
	)
	require.NoError(t, err)

	go agent.Run()
	defer func() {
		require.NoError(t, agent.Stop())
	}()

	// generate some client certs with the same CA but with keys actually signed by some other CA
	// which should fail server-side validation
	clientCerts, err := GenerateCerts()
	require.NoError(t, err)
	clientCerts.CA = serverCerts.CA
	client, err := NewClient(log, clientCerts, "127.0.0.1", 9998, WithCustomizeRetryableClient(func(r *retryablehttp.Client) {
		r.RetryMax = 0
	}))
	require.NoError(t, err)

	err = client.SendHeartbeat(context.Background())
	require.ErrorContains(t, err, "remote error: tls: bad certificate")
}

func TestPostFile(t *testing.T) {
	cert, err := GenerateCerts()
	require.NoError(t, err)
	agent, err := NewNodeAgent(
		cert.CA.CertPEMBytes,
		cert.Server.CertPEMBytes,
		cert.Server.KeyPEMBytes,
		WithListenAddr("127.0.0.1:9998"),
	)
	require.NoError(t, err)

	go agent.Run()
	defer func() {
		require.NoError(t, agent.Stop())
	}()

	client, err := NewClient(log, cert, "127.0.0.1", 9998)
	require.NoError(t, err)

	err = client.WaitForServer(context.Background())
	require.NoError(t, err)

	err = client.SendFile(context.Background(), "/tmp/hello", bytes.NewBuffer([]byte("hello")))
	require.NoError(t, err)
}

func TestConnect(t *testing.T) {
	ctx := context.Background()

	cert, err := GenerateCerts()
	require.NoError(t, err)

	agent, err := NewNodeAgent(
		cert.CA.CertPEMBytes,
		cert.Server.CertPEMBytes,
		cert.Server.KeyPEMBytes,
		WithListenAddr("127.0.0.1:9998"),
	)
	require.NoError(t, err)

	go agent.Run()
	defer func() {
		require.NoError(t, agent.Stop())
	}()

	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	t.Cleanup(s.Close)

	u, err := url.Parse(s.URL)
	require.NoError(t, err)
	addrPort, err := netip.ParseAddrPort(u.Host)
	require.NoError(t, err)

	client, err := NewClient(log, cert, "127.0.0.1", 9998)
	require.NoError(t, err)

	err = client.WaitForServer(ctx)
	require.NoError(t, err)

	conn, err := client.Dial("tcp", addrPort.String())
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet, "/", nil)
	require.NoError(t, err)

	err = req.Write(conn)
	require.NoError(t, err)

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, "hello", string(b))
}

func TestCommand(t *testing.T) {
	ctx := context.Background()

	cert, err := GenerateCerts()
	require.NoError(t, err)

	agent, err := NewNodeAgent(
		cert.CA.CertPEMBytes,
		cert.Server.CertPEMBytes,
		cert.Server.KeyPEMBytes,
		WithListenAddr("127.0.0.1:9998"),
	)
	require.NoError(t, err)

	go agent.Run()
	defer func() {
		require.NoError(t, agent.Stop())
	}()

	client, err := NewClient(log, cert, "127.0.0.1", 9998)
	require.NoError(t, err)

	err = client.WaitForServer(ctx)
	require.NoError(t, err)

	cases := []struct {
		name                  string
		cmd                   string
		args                  []string
		stdin                 string
		stdinFileContents     string
		expStdout             string
		expStderr             string
		expStdoutFileContents string
		expStderrFileContents string
	}{
		{
			name:      "happy case",
			cmd:       "echo",
			args:      []string{"hello"},
			expStdout: "hello\n",
		},
		{
			name: "happy case, no stdout reader",
			cmd:  "echo",
			args: []string{"hello"},
		},
		{
			name:      "happy case with stdout and stderr readers",
			cmd:       "sh",
			args:      []string{"-c", "printf foo; printf bar 1>&2"},
			expStdout: "foo",
			expStderr: "bar",
		},
		{
			name: "happy case with no stderr and stdout readers",
			cmd:  "sh",
			args: []string{"-c", "printf foo; printf bar 1>&2"},
		},
		{
			name:      "stdin to stdout",
			cmd:       "sh",
			args:      []string{"-c", "read line; echo $line bar"},
			stdin:     "foo",
			expStdout: "foo bar\n",
		},
		{
			name:              "stdin from file",
			cmd:               "cat",
			stdinFileContents: "foo",
			expStdout:         "foo",
		},
		{
			name:                  "stdout to file",
			cmd:                   "echo",
			args:                  []string{"foo"},
			expStdoutFileContents: "foo\n",
		},
		{
			name:                  "stderr to file",
			cmd:                   "sh",
			args:                  []string{"-c", "printf foo 1>&2"},
			expStderrFileContents: "foo",
		},
		{
			name:                  "stdin from file, stdout and stderr to file",
			cmd:                   "sh",
			args:                  []string{"-c", "xargs printf; printf bar 1>&2"},
			stdinFileContents:     "foo",
			expStdoutFileContents: "foo",
			expStderrFileContents: "bar",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := cluster.StartProcRequest{
				Command: c.cmd,
				Args:    c.args,
			}

			var stdoutBuf bytes.Buffer
			if c.expStdout != "" {
				req.Stdout = &noopWriteCloser{Writer: &stdoutBuf}
			}
			if c.expStdoutFileContents != "" {
				f, err := os.CreateTemp("", "")
				require.NoError(t, err)
				require.NoError(t, f.Close())
				req.StdoutFile = f.Name()
			}
			if c.expStderrFileContents != "" {
				f, err := os.CreateTemp("", "")
				require.NoError(t, err)
				require.NoError(t, f.Close())
				req.StderrFile = f.Name()
			}

			var stderrBuf bytes.Buffer
			if c.expStderr != "" {
				req.Stderr = &noopWriteCloser{Writer: &stderrBuf}
			}

			if c.stdin != "" {
				req.Stdin = bytes.NewReader([]byte(c.stdin))
			}
			if c.stdinFileContents != "" {
				f, err := os.CreateTemp("", "")
				require.NoError(t, err)
				_, err = io.Copy(f, bytes.NewReader([]byte(c.stdinFileContents)))
				require.NoError(t, err)
				t.Logf("writing stdin contents to %s", f.Name())
				req.StdinFile = f.Name()
				require.NoError(t, f.Close())
			}

			proc, err := client.StartProc(ctx, req)
			require.NoError(t, err)

			res, err := proc.Wait(ctx)
			require.NoError(t, err)

			assert.Equal(t, 0, res.ExitCode)

			if c.expStdout != "" {
				assert.Equal(t, c.expStdout, stdoutBuf.String())
			}
			if c.expStdoutFileContents != "" {
				b, err := os.ReadFile(req.StdoutFile)
				require.NoError(t, err)
				assert.Equal(t, c.expStdoutFileContents, string(b))
			}
			if c.expStderrFileContents != "" {
				b, err := os.ReadFile(req.StderrFile)
				require.NoError(t, err)
				assert.Equal(t, c.expStderrFileContents, string(b))
			}
			if c.expStderr != "" {
				assert.Equal(t, c.expStderr, stderrBuf.String())
			}
		})
	}
}

type noopWriteCloser struct{ io.Writer }

func (c *noopWriteCloser) Close() error { return nil }
