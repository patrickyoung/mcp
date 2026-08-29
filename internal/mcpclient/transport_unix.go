//go:build unix

package mcpclient

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type commandTransport struct {
	endpoint  Endpoint
	stderr    io.Writer
	maxOutput int64
	grace     time.Duration
}

func (t *commandTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	cmd := exec.Command(t.endpoint.Command, t.endpoint.Args...)
	cmd.Stderr = t.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if t.endpoint.Path != "" {
		cmd.Env = replaceEnv(os.Environ(), "PATH", t.endpoint.Path)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	reader := io.ReadCloser(stdout)
	if t.maxOutput > 0 {
		reader = &boundedReadCloser{ReadCloser: stdout, remaining: t.maxOutput}
	}
	conn, err := (&mcp.IOTransport{Reader: reader, Writer: stdin}).Connect(ctx)
	if err != nil {
		_ = killGroup(cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		return nil, err
	}
	return &processConnection{Connection: conn, cmd: cmd, grace: t.grace}, nil
}

func replaceEnv(environ []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}

type boundedReadCloser struct {
	io.ReadCloser
	remaining int64
	exceeded  bool
}

func (r *boundedReadCloser) Read(p []byte) (int, error) {
	if r.exceeded {
		return 0, fmt.Errorf("MCP output exceeds configured byte limit")
	}
	if int64(len(p)) > r.remaining+1 {
		p = p[:r.remaining+1]
	}
	n, err := r.ReadCloser.Read(p)
	r.remaining -= int64(n)
	if r.remaining < 0 {
		r.exceeded = true
		return n, fmt.Errorf("MCP output exceeds configured byte limit")
	}
	return n, err
}

type processConnection struct {
	mcp.Connection
	cmd   *exec.Cmd
	grace time.Duration
	once  sync.Once
	err   error
}

func (c *processConnection) Close() error {
	c.once.Do(func() {
		closeErr := c.Connection.Close()
		waited := make(chan error, 1)
		go func() { waited <- c.cmd.Wait() }()
		grace := c.grace
		if grace <= 0 {
			grace = time.Second
		}

		select {
		case waitErr := <-waited:
			// The direct server may exit while descendants keep inherited
			// descriptors or continue working. Process lifetime belongs to the
			// whole group, not only the process we can Wait on.
			_ = killGroup(c.cmd.Process.Pid, syscall.SIGTERM)
			c.err = joinCloseErrors(closeErr, waitErr)
			return
		case <-time.After(grace):
		}
		_ = killGroup(c.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case waitErr := <-waited:
			c.err = joinCloseErrors(closeErr, waitErr)
			return
		case <-time.After(grace):
		}
		_ = killGroup(c.cmd.Process.Pid, syscall.SIGKILL)
		waitErr := <-waited
		c.err = joinCloseErrors(closeErr, waitErr)
	})
	return c.err
}

func killGroup(pid int, signal syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, signal); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}

func joinCloseErrors(a, b error) error {
	if a != nil {
		return a
	}
	if b != nil {
		if _, ok := b.(*exec.ExitError); ok {
			return nil
		}
		return b
	}
	return nil
}

// Compile-time check that wrappers retain the SDK transport contract.
var _ mcp.Connection = (*processConnection)(nil)
var _ jsonrpc.Message
