package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type wireResponse struct {
	Result json.RawMessage
	Error  json.RawMessage
}

type recorder struct {
	mu sync.Mutex

	conn mcp.Connection

	methods map[string]string
	last    map[string]wireResponse

	begin                  bool
	effect                 bool
	targetID               string
	target                 wireResponse
	targetReady            bool
	sent                   bool
	duplicate              chan struct{}
	dupOnce                sync.Once
	extensionNotifications *jsonlWriter
}

func (r *recorder) transport(inner mcp.Transport) mcp.Transport {
	return transportFunc(func(ctx context.Context) (mcp.Connection, error) {
		conn, err := inner.Connect(ctx)
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.conn = conn
		r.methods = make(map[string]string)
		r.last = make(map[string]wireResponse)
		r.mu.Unlock()
		return &recordingConnection{Connection: conn, recorder: r}, nil
	})
}

func (r *recorder) Begin(effect bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.begin = true
	r.effect = effect
	r.targetID = ""
	r.target = wireResponse{}
	r.targetReady = false
	r.sent = false
	r.duplicate = make(chan struct{})
	r.dupOnce = sync.Once{}
}

func (r *recorder) Sent() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent
}

func (r *recorder) DuplicateWithin(wait time.Duration) bool {
	r.mu.Lock()
	ch := r.duplicate
	r.mu.Unlock()
	if ch == nil {
		return false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

func (r *recorder) Target() (wireResponse, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.target, r.targetReady
}

func (r *recorder) Last(method string) (wireResponse, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.last[method]
	return v, ok
}

type transportFunc func(context.Context) (mcp.Connection, error)

func (f transportFunc) Connect(ctx context.Context) (mcp.Connection, error) {
	return f(ctx)
}

type recordingConnection struct {
	mcp.Connection
	recorder *recorder
}

func (c *recordingConnection) Write(ctx context.Context, msg jsonrpc.Message) error {
	if err := c.Connection.Write(ctx, msg); err != nil {
		return err
	}
	req, ok := msg.(*jsonrpc.Request)
	if !ok || !req.IsCall() {
		return nil
	}
	key := idKey(req.ID)
	c.recorder.mu.Lock()
	c.recorder.methods[key] = req.Method
	if c.recorder.begin && c.recorder.targetID == "" {
		c.recorder.targetID = key
		c.recorder.sent = c.recorder.effect
		c.recorder.begin = false
	}
	c.recorder.mu.Unlock()
	return nil
}

func (c *recordingConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		msg, err := c.Connection.Read(ctx)
		if err != nil {
			return nil, err
		}
		if req, ok := msg.(*jsonrpc.Request); ok && !req.IsCall() && interceptNotification(req.Method) {
			c.recorder.extensionNotifications.writeRaw(req.Method, req.Params)
			continue
		}
		res, ok := msg.(*jsonrpc.Response)
		if !ok {
			return msg, nil
		}
		wr := wireResponse{Result: append(json.RawMessage(nil), res.Result...)}
		if res.Error != nil {
			wr.Error, _ = json.Marshal(res.Error)
		}
		key := idKey(res.ID)
		c.recorder.mu.Lock()
		if method := c.recorder.methods[key]; method != "" {
			c.recorder.last[method] = wr
		}
		if key == c.recorder.targetID {
			if c.recorder.targetReady {
				c.recorder.dupOnce.Do(func() { close(c.recorder.duplicate) })
			}
			c.recorder.target = wr
			c.recorder.targetReady = true
		}
		c.recorder.mu.Unlock()
		return msg, nil
	}
}

func interceptNotification(method string) bool {
	if method == "notifications/subscriptions/acknowledged" || method == "notifications/tasks" {
		return true
	}
	switch method {
	case "notifications/cancelled", "notifications/progress", "notifications/tools/list_changed",
		"notifications/prompts/list_changed", "notifications/resources/list_changed",
		"notifications/resources/updated", "notifications/message",
		"notifications/elicitation/complete", "notifications/roots/list_changed":
		return false
	}
	return strings.HasPrefix(method, "notifications/")
}

func idKey(id jsonrpc.ID) string {
	return fmt.Sprintf("%T:%v", id.Raw(), id.Raw())
}
