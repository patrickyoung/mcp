package mcpclient

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// requireModernTransport prevents the SDK's compatibility fallback from
// putting initialize on the wire. Modern mcp may observe a legacy endpoint's
// server/discover rejection, but it never enters a stateful legacy session.
func requireModernTransport(inner mcp.Transport) mcp.Transport {
	return transportFunc(func(ctx context.Context) (mcp.Connection, error) {
		conn, err := inner.Connect(ctx)
		if err != nil {
			return nil, err
		}
		return &modernConnection{Connection: conn}, nil
	})
}

type modernConnection struct {
	mcp.Connection
}

func (c *modernConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	request, ok := message.(*jsonrpc.Request)
	if ok && request.IsCall() && request.Method == "initialize" {
		return fmt.Errorf("legacy initialize disabled; modern stateless MCP %s is required", ProtocolVersion)
	}
	return c.Connection.Write(ctx, message)
}

// forceLegacyTransport rejects the SDK's modern server/discover probe locally.
// The official SDK then performs its normal legacy initialize/initialized
// exchange. No synthetic message reaches the server or the wire recorder.
func forceLegacyTransport(inner mcp.Transport) mcp.Transport {
	return transportFunc(func(ctx context.Context) (mcp.Connection, error) {
		conn, err := inner.Connect(ctx)
		if err != nil {
			return nil, err
		}
		pumpCtx, cancel := context.WithCancel(ctx)
		legacy := &legacyConnection{
			Connection: conn,
			cancel:     cancel,
			local:      make(chan jsonrpc.Message, 1),
			remote:     make(chan legacyRead, 1),
			done:       make(chan struct{}),
		}
		go legacy.pump(pumpCtx)
		return legacy, nil
	})
}

type legacyRead struct {
	message jsonrpc.Message
	err     error
}

type legacyConnection struct {
	mcp.Connection
	cancel context.CancelFunc
	local  chan jsonrpc.Message
	remote chan legacyRead
	done   chan struct{}
	close  sync.Once
}

func (c *legacyConnection) pump(ctx context.Context) {
	for {
		message, err := c.Connection.Read(ctx)
		select {
		case c.remote <- legacyRead{message: message, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func (c *legacyConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case message := <-c.local:
		return message, nil
	case read := <-c.remote:
		return read.message, read.err
	case <-c.done:
		return nil, io.ErrClosedPipe
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *legacyConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	request, ok := message.(*jsonrpc.Request)
	if !ok || !request.IsCall() || request.Method != "server/discover" {
		return c.Connection.Write(ctx, message)
	}
	response := &jsonrpc.Response{
		ID:    request.ID,
		Error: &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "server/discover disabled by legacy compatibility mode"},
	}
	select {
	case c.local <- response:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *legacyConnection) Close() error {
	var err error
	c.close.Do(func() {
		close(c.done)
		c.cancel()
		err = c.Connection.Close()
	})
	return err
}
