package mcpserve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServerDispatchesCoreAndExtensionMethods(t *testing.T) {
	t.Setenv("GO_WANT_MCPSERVE_HELPER", "1")
	manifest := &Manifest{
		Name:    "filter-test",
		Version: "1.0.0",
		Tools: []*mcp.Tool{{
			Name:        "echo",
			Description: "echo through a filter",
			InputSchema: map[string]any{"type": "object"},
		}},
		Methods: []string{"acme/status"},
	}
	server, err := New(manifest, Config{Dispatcher: helperCommand(), Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	progress := make(chan *mcp.ProgressNotificationParams, 1)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			progress <- req.Params
		},
	})
	if err := mcp.AddSendingCustomMethod[*testParams, *testResult](client, "acme/status"); err != nil {
		t.Fatal(err)
	}
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 1 || listed.Tools[0].Name != "echo" {
		t.Fatalf("tools/list = %#v", listed.Tools)
	}
	callParams := &mcp.CallToolParams{
		Name: "echo", Arguments: map[string]any{"text": "hello"},
	}
	callParams.SetProgressToken("progress-1")
	called, err := clientSession.CallTool(ctx, callParams)
	if err != nil {
		t.Fatal(err)
	}
	structured, ok := called.StructuredContent.(map[string]any)
	if !ok || structured["method"] != "tools/call" || structured["name"] != "echo" {
		t.Fatalf("tools/call = %#v", called)
	}
	if _, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "not-declared"}); err == nil {
		t.Fatal("unknown tool reached dispatcher")
	}
	select {
	case got := <-progress:
		if got.ProgressToken != "progress-1" || got.Progress != 1 || got.Message != "working" {
			t.Fatalf("progress = %#v", got)
		}
	case <-ctx.Done():
		t.Fatal("progress notification not received")
	}

	custom, err := mcp.CallCustomMethod[*testParams, *testResult](ctx, clientSession, "acme/status", &testParams{Value: "now"})
	if err != nil {
		t.Fatal(err)
	}
	if custom.Status != "ready" || custom.Value != "now" {
		t.Fatalf("custom result = %#v", custom)
	}
}

func TestTasksExtensionPreservesPolymorphicResults(t *testing.T) {
	t.Setenv("GO_WANT_MCPSERVE_HELPER", "1")
	capabilities := &mcp.ServerCapabilities{}
	capabilities.AddExtension("io.modelcontextprotocol/tasks", nil)
	server, err := New(&Manifest{
		Name: "tasks-filter", Version: "1.0.0", Capabilities: capabilities,
		Tools:   []*mcp.Tool{{Name: "async", InputSchema: map[string]any{"type": "object"}}},
		Methods: []string{"tasks/get", "tasks/update", "tasks/cancel"},
	}, Config{Dispatcher: helperCommand(), Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(HTTPHandler(server))
	defer httpServer.Close()

	params := map[string]any{"name": "async", "arguments": map[string]any{}}
	withTasks := requestMeta(map[string]any{"extensions": map[string]any{"io.modelcontextprotocol/tasks": map[string]any{}}})
	params["_meta"] = withTasks
	status, result := rawHTTPCall(t, httpServer.URL, "tools/call", "async", params)
	if status != http.StatusOK || result["resultType"] != "task" || result["taskId"] != "task-1" {
		t.Fatalf("task creation: HTTP %d %#v", status, result)
	}

	getParams := map[string]any{"taskId": "task-1", "_meta": withTasks}
	status, result = rawHTTPCall(t, httpServer.URL, "tasks/get", "task-1", getParams)
	if status != http.StatusOK || result["status"] != "completed" || result["result"] == nil {
		t.Fatalf("tasks/get: HTTP %d %#v", status, result)
	}
	status, _ = rawHTTPCall(t, httpServer.URL, "tasks/get", "wrong-task", getParams)
	if status != http.StatusBadRequest {
		t.Fatalf("tasks/get accepted mismatched Mcp-Name: HTTP %d", status)
	}

	withoutTasks := map[string]any{"name": "async", "arguments": map[string]any{}, "_meta": requestMeta(map[string]any{})}
	status, result = rawHTTPCall(t, httpServer.URL, "tools/call", "async", withoutTasks)
	if status != http.StatusBadRequest || result["errorCode"] != float64(-32021) {
		t.Fatalf("missing task capability: HTTP %d %#v", status, result)
	}
}

func TestStatelessHTTPDispatch(t *testing.T) {
	t.Setenv("GO_WANT_MCPSERVE_HELPER", "1")
	server, err := New(&Manifest{
		Name: "http-filter", Version: "1.0.0",
		Tools: []*mcp.Tool{{Name: "echo", InputSchema: map[string]any{"type": "object"}}},
	}, Config{Dispatcher: helperCommand(), Stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(HTTPHandler(server))
	defer httpServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "http-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: httpServer.URL, MaxRetries: -1, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok || structured["method"] != "tools/call" {
		t.Fatalf("HTTP tools/call = %#v", result)
	}
}

func TestDispatcherOutcomesAndBounds(t *testing.T) {
	t.Setenv("GO_WANT_MCPSERVE_HELPER", "1")
	params := &rawParams{raw: json.RawMessage(`{}`)}

	t.Setenv("MCPSERVE_HELPER_MODE", "peer-error")
	d := dispatcher{cfg: Config{Dispatcher: helperCommand(), Stderr: io.Discard, MaxInput: 1024, MaxOutput: 1024}}
	_, err := d.call(context.Background(), nil, "acme/error", params)
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32001 || rpcErr.Message != "refused" {
		t.Fatalf("peer error = %#v", err)
	}

	t.Setenv("MCPSERVE_HELPER_MODE", "large")
	_, err = d.call(context.Background(), nil, "acme/large", params)
	if err == nil || !strings.Contains(err.Error(), "output exceeds") {
		t.Fatalf("large output error = %v", err)
	}

	t.Setenv("MCPSERVE_HELPER_MODE", "sleep")
	d.cfg.Timeout = 20 * time.Millisecond
	_, err = d.call(context.Background(), nil, "acme/sleep", params)
	if err == nil {
		t.Fatal("timeout succeeded")
	}
}

func TestLoadManifestRequiresOneObject(t *testing.T) {
	path := t.TempDir() + "/manifest.json"
	if err := os.WriteFile(path, []byte(`{"name":"x","version":"1"} {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(path); err == nil {
		t.Fatal("accepted trailing JSON")
	}
}

func TestInvalidManifestReturnsError(t *testing.T) {
	if _, err := New(&Manifest{
		Name: "bad", Version: "1", Tools: []*mcp.Tool{{Name: "missing-schema"}},
	}, Config{Dispatcher: helperCommand()}); err == nil {
		t.Fatal("invalid tool descriptor did not return an error")
	}
}

type testParams struct {
	mcp.ParamsBase
	Value string `json:"value"`
}

type testResult struct {
	mcp.ResultBase
	Status string `json:"status"`
	Value  string `json:"value"`
}

func helperCommand() []string {
	return []string{os.Args[0], "-test.run=^TestDispatcherHelperProcess$", "--"}
}

func requestMeta(capabilities map[string]any) map[string]any {
	return map[string]any{
		mcp.MetaKeyProtocolVersion:    "2026-07-28",
		mcp.MetaKeyClientCapabilities: capabilities,
		mcp.MetaKeyClientInfo:         map[string]any{"name": "raw-test", "version": "1"},
	}
}

func rawHTTPCall(t *testing.T, endpoint, method, name string, params map[string]any) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", method)
	req.Header.Set("Mcp-Name", name)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(bytes.TrimSpace(payload), []byte("event:")) {
		for _, line := range bytes.Split(payload, []byte{'\n'}) {
			if bytes.HasPrefix(line, []byte("data:")) {
				payload = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			}
		}
	}
	var wire struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Code int64 `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Error != nil {
		return response.StatusCode, map[string]any{"errorCode": float64(wire.Error.Code), "raw": string(payload)}
	}
	return response.StatusCode, wire.Result
}

func TestDispatcherHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_MCPSERVE_HELPER") != "1" {
		return
	}
	method := os.Args[len(os.Args)-1]
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	switch os.Getenv("MCPSERVE_HELPER_MODE") {
	case "peer-error":
		fmt.Print(`{"code":-32001,"message":"refused","data":{"why":"policy"}}`)
		os.Exit(1)
	case "large":
		fmt.Printf(`{"value":"%s"}`, strings.Repeat("x", 2048))
		return
	case "sleep":
		time.Sleep(time.Second)
		fmt.Print(`{}`)
		return
	}
	var params map[string]any
	if json.Unmarshal(body, &params) != nil {
		os.Exit(2)
	}
	switch method {
	case "tools/call":
		name, _ := params["name"].(string)
		if name == "async" {
			fmt.Print(`{"resultType":"task","taskId":"task-1","status":"working","createdAt":"2026-08-29T12:00:00Z","lastUpdatedAt":"2026-08-29T12:00:00Z","ttlMs":60000,"pollIntervalMs":1000}`)
			os.Exit(0)
		}
		meta, _ := params["_meta"].(map[string]any)
		if token := meta["progressToken"]; token != nil {
			if events := os.NewFile(3, "events"); events != nil {
				_ = json.NewEncoder(events).Encode(map[string]any{
					"method": "notifications/progress",
					"params": map[string]any{"progressToken": token, "progress": 1, "total": 1, "message": "working"},
				})
				_ = events.Close()
			}
		}
		result := map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": "ok"}},
			"structuredContent": map[string]any{"method": method, "name": name},
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
	case "acme/status":
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"status": "ready", "value": params["value"]})
	case "tasks/get":
		fmt.Print(`{"resultType":"complete","taskId":"task-1","status":"completed","createdAt":"2026-08-29T12:00:00Z","lastUpdatedAt":"2026-08-29T12:01:00Z","ttlMs":60000,"result":{"content":[{"type":"text","text":"done"}]}}`)
	case "tasks/update", "tasks/cancel":
		fmt.Print(`{"resultType":"complete"}`)
	default:
		fmt.Print(`{"code":-32601,"message":"not found"}`)
		os.Exit(1)
	}
	os.Exit(0)
}
