package mcpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/patrickyoung/mcp/internal/admit"
)

func TestRequestPreservesResultAndClassifiesOutcomes(t *testing.T) {
	tests := []struct {
		mode string
		code int
		find string
	}{
		{"ok", 0, `"unknown":{"kept":true}`},
		{"tool-error", 1, `"isError":true`},
		{"input-required", 75, `"resultType":"input_required"`},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			endpoint := helperEndpoint(t, tt.mode)
			out, err := Request(context.Background(), endpoint, "tools/call", json.RawMessage(`{"name":"echo","arguments":{"x":1}}`), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if out.Code != tt.code {
				t.Fatalf("code = %d, want %d; raw=%s", out.Code, tt.code, out.Raw)
			}
			if !strings.Contains(string(out.Raw), tt.find) {
				t.Fatalf("result %s does not contain %s", out.Raw, tt.find)
			}
		})
	}
}

func TestPostSendFailuresAreUnknown(t *testing.T) {
	for _, mode := range []string{"drop", "wrong-id", "malformed", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			endpoint := helperEndpoint(t, mode)
			_, err := Request(context.Background(), endpoint, "tools/call", json.RawMessage(`{"name":"echo"}`), Options{})
			var exitErr *ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != 125 {
				t.Fatalf("error = %v, want exit 125", err)
			}
		})
	}
}

func TestOutputLimitAfterSendIsUnknown(t *testing.T) {
	endpoint := helperEndpoint(t, "large")
	_, err := Request(context.Background(), endpoint, "tools/call", json.RawMessage(`{"name":"echo"}`), Options{MaxOutput: 4096})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 125 {
		t.Fatalf("error = %v, want exit 125", err)
	}
}

func TestToolChecksDescriptorBeforeCall(t *testing.T) {
	endpoint := helperEndpoint(t, "ok")
	disc, err := Discover(context.Background(), endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := []byte(`{"name":"echo","description":"echo input","inputSchema":{"type":"object"}}`)
	digest, err := admit.Digest("tools", endpoint, disc.Raw, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Tool(context.Background(), endpoint, "echo", digest, json.RawMessage(`{"hello":"world"}`), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Code != 0 || !strings.Contains(string(out.Raw), `"hello":"world"`) {
		t.Fatalf("outcome = %#v", out)
	}

	_, err = Tool(context.Background(), endpoint, "echo", strings.Repeat("0", 64), json.RawMessage(`{}`), Options{})
	var exitErr *ExitError
	if err == nil || errors.As(err, &exitErr) {
		t.Fatalf("mismatch error = %v", err)
	}
}

func TestPromptAndResourceCheckDescriptors(t *testing.T) {
	endpoint := helperEndpoint(t, "ok")
	disc, err := Discover(context.Background(), endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	promptRaw := []byte(`{"name":"review","description":"review a change","arguments":[{"name":"tone"}]}`)
	promptDigest, err := admit.Digest("prompts", endpoint, disc.Raw, promptRaw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Prompt(context.Background(), endpoint, "review", promptDigest, json.RawMessage(`{"tone":"brief"}`), Options{})
	if err != nil || out.Code != 0 || !strings.Contains(string(out.Raw), `"messages"`) {
		t.Fatalf("prompt = %#v, %v", out, err)
	}

	resourceRaw := []byte(`{"uri":"doc://guide","name":"guide","description":"the guide"}`)
	resourceDigest, err := admit.Digest("resources", endpoint, disc.Raw, resourceRaw)
	if err != nil {
		t.Fatal(err)
	}
	out, err = ReadResource(context.Background(), endpoint, "doc://guide", resourceDigest, Options{})
	if err != nil || out.Code != 0 || !strings.Contains(string(out.Raw), `"contents"`) {
		t.Fatalf("resource = %#v, %v", out, err)
	}
}

func TestExplicitContinuationAndTaskExtension(t *testing.T) {
	endpoint := helperEndpoint(t, "input-required")
	first, err := Request(context.Background(), endpoint, "tools/call", json.RawMessage(`{"name":"echo","arguments":{}}`), Options{})
	if err != nil || first.Code != 75 {
		t.Fatalf("first = %#v, %v", first, err)
	}
	continued := json.RawMessage(`{
		"name":"echo",
		"arguments":{},
		"requestState":"opaque",
		"inputResponses":{"q":{"action":"accept","content":{"answer":"yes"}}}
	}`)
	second, err := Request(context.Background(), endpoint, "tools/call", continued, Options{})
	if err != nil || second.Code != 0 || !strings.Contains(string(second.Raw), `"continued":true`) {
		t.Fatalf("continued = %#v, %v", second, err)
	}

	task, err := Request(context.Background(), helperEndpoint(t, "ok"), "io.modelcontextprotocol/tasks/get", json.RawMessage(`{"taskId":"t-1"}`), Options{})
	if err != nil || task.Code != 75 || !strings.Contains(string(task.Raw), `"status":"working"`) {
		t.Fatalf("task = %#v, %v", task, err)
	}
}

func TestListenEmitsJSONLAndDoesNotReconnect(t *testing.T) {
	var stream bytes.Buffer
	err := Listen(context.Background(), helperEndpoint(t, "listen"), Options{Timeout: 100 * time.Millisecond, Listen: &stream})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Listen error = %v", err)
	}
	if got := stream.String(); !strings.Contains(got, `"method":"notifications/tools/list_changed"`) {
		t.Fatalf("stream = %q", got)
	}
}

func TestTimeoutKillsServerProcessGroup(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	endpoint := helperEndpoint(t, "hang", pidfile)
	_, err := Request(context.Background(), endpoint, "tools/call", json.RawMessage(`{"name":"echo"}`), Options{Timeout: 100 * time.Millisecond})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 125 {
		t.Fatalf("error = %v, want exit 125", err)
	}
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server child process %d survived process-group cleanup", pid)
}

func TestReadParams(t *testing.T) {
	if got, err := ReadParams(strings.NewReader("  \n"), 100); err != nil || string(got) != "{}" {
		t.Fatalf("empty: %s, %v", got, err)
	}
	if _, err := ReadParams(strings.NewReader("[]"), 100); err == nil {
		t.Fatal("array input accepted")
	}
	if _, err := ReadParams(strings.NewReader("{} {}"), 100); err == nil {
		t.Fatal("two values accepted")
	}
	if _, err := ReadParams(strings.NewReader(`{"long":"value"}`), 4); err == nil {
		t.Fatal("oversize input accepted")
	}
}

func helperEndpoint(t *testing.T, mode string, args ...string) Endpoint {
	t.Helper()
	testBinary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	command, err := exec.LookPath("env")
	if err != nil {
		t.Fatal(err)
	}
	command, err = filepath.Abs(command)
	if err != nil {
		t.Fatal(err)
	}
	argv := []string{"MCP_HELPER_PROCESS=1", testBinary, "-test.run=TestMCPHelperProcess", "--", mode}
	argv = append(argv, args...)
	return Endpoint{Type: "stdio", Command: command, Args: argv, Path: os.Getenv("PATH")}
}

func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("MCP_HELPER_PROCESS") != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		os.Exit(90)
	}
	mode := os.Args[separator+1]
	extra := os.Args[separator+2:]
	serveHelper(mode, extra)
	os.Exit(0)
}

func serveHelper(mode string, extra []string) {
	// The transport deliberately passes the parent's environment unchanged.
	// Mark only the subprocess branch after exec through a test-binary flag.
	_ = os.Setenv("MCP_HELPER_PROCESS", "1")
	dec := json.NewDecoder(bufio.NewReader(os.Stdin))
	enc := json.NewEncoder(os.Stdout)
	for {
		var req struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := dec.Decode(&req); err != nil {
			if err != io.EOF {
				fmt.Fprintln(os.Stderr, err)
			}
			return
		}
		switch req.Method {
		case "server/discover":
			writeResponse(enc, req.ID, map[string]any{
				"resultType":        "complete",
				"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "test-server", "version": "1"}},
				"ttlMs":             0,
				"cacheScope":        "public",
				"supportedVersions": []string{ProtocolVersion},
				"capabilities": map[string]any{
					"tools": map[string]any{}, "prompts": map[string]any{}, "resources": map[string]any{},
				},
			})
		case "tools/list":
			writeResponse(enc, req.ID, map[string]any{
				"resultType": "complete",
				"tools": []any{map[string]any{
					"name": "echo", "description": "echo input", "inputSchema": map[string]any{"type": "object"},
				}},
			})
		case "prompts/list":
			writeResponse(enc, req.ID, map[string]any{"resultType": "complete", "prompts": []any{map[string]any{
				"name": "review", "description": "review a change", "arguments": []any{map[string]any{"name": "tone"}},
			}}})
		case "prompts/get":
			writeResponse(enc, req.ID, map[string]any{"messages": []any{map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": "review"}}}})
		case "resources/list":
			writeResponse(enc, req.ID, map[string]any{"resultType": "complete", "resources": []any{map[string]any{
				"uri": "doc://guide", "name": "guide", "description": "the guide",
			}}})
		case "resources/read":
			writeResponse(enc, req.ID, map[string]any{"contents": []any{map[string]any{"uri": "doc://guide", "text": "hello"}}})
		case "tools/call":
			switch mode {
			case "drop":
				return
			case "wrong-id":
				writeResponse(enc, float64(999), map[string]any{"content": []any{}})
				return
			case "malformed":
				fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0",bad`)
				return
			case "duplicate":
				writeResponse(enc, req.ID, map[string]any{"content": []any{}})
				writeResponse(enc, req.ID, map[string]any{"content": []any{}})
				return
			case "large":
				writeResponse(enc, req.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("x", 10000)}}})
			case "tool-error":
				writeResponse(enc, req.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": "no"}}, "isError": true})
			case "input-required":
				var continuation struct {
					InputResponses map[string]any `json:"inputResponses"`
				}
				_ = json.Unmarshal(req.Params, &continuation)
				if len(continuation.InputResponses) != 0 {
					writeResponse(enc, req.ID, map[string]any{"content": []any{}, "continued": true})
				} else {
					writeResponse(enc, req.ID, map[string]any{"resultType": "input_required", "inputRequests": map[string]any{"q": map[string]any{"method": "elicitation/create", "params": map[string]any{}}}, "requestState": "opaque", "content": []any{}})
				}
			case "hang":
				if len(extra) != 1 {
					return
				}
				child := exec.Command("sleep", "60")
				if child.Start() != nil {
					return
				}
				_ = os.WriteFile(extra[0], []byte(strconv.Itoa(child.Process.Pid)), 0o600)
				select {}
			default:
				var params struct {
					Arguments any `json:"arguments"`
				}
				_ = json.Unmarshal(req.Params, &params)
				writeResponse(enc, req.ID, map[string]any{
					"content":           []any{map[string]any{"type": "text", "text": "ok"}},
					"structuredContent": params.Arguments,
					"unknown":           map[string]any{"kept": true},
				})
			}
		case "io.modelcontextprotocol/tasks/get":
			writeResponse(enc, req.ID, map[string]any{"taskId": "t-1", "status": "working"})
		case "subscriptions/listen":
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/subscriptions/acknowledged", "params": map[string]any{}})
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/tools/list_changed", "params": map[string]any{}})
		default:
			writeError(enc, req.ID, -32601, "method not found")
		}
	}
}

func writeResponse(enc *json.Encoder, id, result any) {
	_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeError(enc *json.Encoder, id any, code int, message string) {
	_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}
