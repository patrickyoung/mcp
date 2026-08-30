package mcpclient

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLegacyConnectionCloseUnblocksRead(t *testing.T) {
	inner := &blockingConnection{closed: make(chan struct{})}
	transport := forceLegacyTransport(transportFunc(func(context.Context) (mcp.Connection, error) {
		return inner, nil
	}))
	conn, err := transport.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() {
		_, err := conn.Read(context.Background())
		read <- err
	}()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("Read returned no error after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Read")
	}
}

func TestModernConnectionDoesNotSendInitialize(t *testing.T) {
	inner := &captureConnection{blockingConnection: blockingConnection{closed: make(chan struct{})}}
	transport := requireModernTransport(transportFunc(func(context.Context) (mcp.Connection, error) {
		return inner, nil
	}))
	conn, err := transport.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	id1, err := jsonrpc.MakeID(float64(1))
	if err != nil {
		t.Fatal(err)
	}
	id2, err := jsonrpc.MakeID(float64(2))
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(context.Background(), &jsonrpc.Request{ID: id1, Method: "server/discover"}); err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(context.Background(), &jsonrpc.Request{ID: id2, Method: "initialize"}); err == nil {
		t.Fatal("initialize was accepted in modern mode")
	}
	inner.mu.Lock()
	defer inner.mu.Unlock()
	if len(inner.methods) != 1 || inner.methods[0] != "server/discover" {
		t.Fatalf("methods sent to server = %v", inner.methods)
	}
}

type blockingConnection struct {
	closed chan struct{}
	once   sync.Once
}

type captureConnection struct {
	blockingConnection
	mu      sync.Mutex
	methods []string
}

func (c *captureConnection) Write(_ context.Context, message jsonrpc.Message) error {
	if request, ok := message.(*jsonrpc.Request); ok {
		c.mu.Lock()
		c.methods = append(c.methods, request.Method)
		c.mu.Unlock()
	}
	return nil
}

func (c *blockingConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case <-c.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (*blockingConnection) Write(context.Context, jsonrpc.Message) error { return nil }
func (*blockingConnection) SessionID() string                            { return "" }
func (c *blockingConnection) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
