package mcpserve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Manifest struct {
	Name              string                  `json:"name"`
	Version           string                  `json:"version"`
	Instructions      string                  `json:"instructions,omitempty"`
	Capabilities      *mcp.ServerCapabilities `json:"capabilities,omitempty"`
	Tools             []*mcp.Tool             `json:"tools,omitempty"`
	Prompts           []*mcp.Prompt           `json:"prompts,omitempty"`
	Resources         []*mcp.Resource         `json:"resources,omitempty"`
	ResourceTemplates []*mcp.ResourceTemplate `json:"resourceTemplates,omitempty"`
	Methods           []string                `json:"methods,omitempty"`
}

type Config struct {
	Dispatcher []string
	Stderr     io.Writer
	Timeout    time.Duration
	MaxInput   int64
	MaxOutput  int64
}

func LoadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var manifest Manifest
	if err := dec.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("manifest must contain exactly one JSON object")
	}
	if manifest.Name == "" || manifest.Version == "" {
		return nil, fmt.Errorf("manifest requires name and version")
	}
	return &manifest, nil
}

func New(manifest *Manifest, cfg Config) (server *mcp.Server, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			server = nil
			err = fmt.Errorf("manifest registration: %v", recovered)
		}
	}()
	if manifest == nil {
		return nil, fmt.Errorf("nil manifest")
	}
	if len(cfg.Dispatcher) == 0 {
		return nil, fmt.Errorf("missing dispatcher command")
	}
	command, err := exec.LookPath(cfg.Dispatcher[0])
	if err != nil {
		return nil, fmt.Errorf("dispatcher %q: %w", cfg.Dispatcher[0], err)
	}
	if !filepath.IsAbs(command) {
		command, err = filepath.Abs(command)
		if err != nil {
			return nil, err
		}
	}
	cfg.Dispatcher = append([]string{command}, cfg.Dispatcher[1:]...)
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	if cfg.MaxInput <= 0 {
		cfg.MaxInput = 16 << 20
	}
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = 64 << 20
	}
	capabilities := new(mcp.ServerCapabilities)
	if manifest.Capabilities == nil {
		// The SDK's legacy default advertises logging. Modern mcpserve starts
		// with no ambient capability and derives only what the manifest names.
		capabilities = &mcp.ServerCapabilities{}
	} else {
		raw, marshalErr := json.Marshal(manifest.Capabilities)
		if marshalErr != nil {
			return nil, fmt.Errorf("server capabilities: %w", marshalErr)
		}
		if unmarshalErr := json.Unmarshal(raw, capabilities); unmarshalErr != nil {
			return nil, fmt.Errorf("server capabilities: %w", unmarshalErr)
		}
	}
	if capabilities.Tools != nil && capabilities.Tools.ListChanged ||
		capabilities.Prompts != nil && capabilities.Prompts.ListChanged ||
		capabilities.Resources != nil && (capabilities.Resources.ListChanged || capabilities.Resources.Subscribe) {
		return nil, fmt.Errorf("static manifests cannot advertise list changes or legacy resource subscriptions")
	}
	if len(manifest.Tools) != 0 && capabilities.Tools == nil {
		capabilities.Tools = &mcp.ToolCapabilities{}
	}
	if len(manifest.Prompts) != 0 && capabilities.Prompts == nil {
		capabilities.Prompts = &mcp.PromptCapabilities{}
	}
	if (len(manifest.Resources) != 0 || len(manifest.ResourceTemplates) != 0) && capabilities.Resources == nil {
		capabilities.Resources = &mcp.ResourceCapabilities{}
	}
	d := &dispatcher{cfg: cfg, allowLogging: capabilities.Logging != nil}
	hasTools := capabilities.Tools != nil || len(manifest.Tools) != 0
	hasPrompts := capabilities.Prompts != nil || len(manifest.Prompts) != 0
	hasResources := capabilities.Resources != nil || len(manifest.Resources) != 0 || len(manifest.ResourceTemplates) != 0
	hasCompletion := capabilities.Completions != nil
	_, hasTasks := capabilities.Extensions["io.modelcontextprotocol/tasks"]
	options := &mcp.ServerOptions{
		Instructions: manifest.Instructions,
		Capabilities: capabilities,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if hasCompletion {
		options.CompletionHandler = func(ctx context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
			return callInto[mcp.CompleteResult](d, ctx, req.Session, "completion/complete", req.Params)
		}
	}
	server = mcp.NewServer(&mcp.Implementation{Name: manifest.Name, Version: manifest.Version}, options)

	// Descriptors belong to the manifest; behavior belongs to an ordinary
	// executable. Standard calls stay inside the SDK's typed handlers so name
	// lookup, MRTR capability checks, and result semantics remain protocol-owned.
	type toolContract struct {
		input  *jsonschema.Resolved
		output *jsonschema.Resolved
	}
	toolContracts := make(map[string]toolContract, len(manifest.Tools))
	for _, tool := range manifest.Tools {
		input, err := resolveSchema(tool.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("tool %q input schema: %w", tool.Name, err)
		}
		var output *jsonschema.Resolved
		if tool.OutputSchema != nil {
			output, err = resolveSchema(tool.OutputSchema)
			if err != nil {
				return nil, fmt.Errorf("tool %q output schema: %w", tool.Name, err)
			}
		}
		toolContracts[tool.Name] = toolContract{input: input, output: output}
		server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if err := validateArguments(req.Params.Name, req.Params.Arguments, input); err != nil {
				return nil, err
			}
			result, err := callInto[mcp.CallToolResult](d, ctx, req.Session, "tools/call", req.Params)
			if err != nil {
				return nil, err
			}
			if output != nil {
				if err := output.Validate(result.StructuredContent); err != nil {
					return nil, protocolError(fmt.Errorf("tool %q structured result: %w", req.Params.Name, err))
				}
			}
			return result, nil
		})
	}
	for _, prompt := range manifest.Prompts {
		server.AddPrompt(prompt, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			for _, argument := range prompt.Arguments {
				if _, ok := req.Params.Arguments[argument.Name]; argument.Required && !ok {
					return nil, invalidParams("prompt %q requires argument %q", req.Params.Name, argument.Name)
				}
			}
			return callInto[mcp.GetPromptResult](d, ctx, req.Session, "prompts/get", req.Params)
		})
	}
	resourceHandler := func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return callInto[mcp.ReadResourceResult](d, ctx, req.Session, "resources/read", req.Params)
	}
	for _, resource := range manifest.Resources {
		server.AddResource(resource, resourceHandler)
	}
	for _, template := range manifest.ResourceTemplates {
		server.AddResourceTemplate(template, resourceHandler)
	}

	for _, method := range manifest.Methods {
		if method == "" || strings.HasPrefix(method, "notifications/") {
			return nil, fmt.Errorf("invalid custom request method %q", method)
		}
		if strings.HasPrefix(method, "tasks/") && !hasTasks {
			return nil, fmt.Errorf("method %q requires server extension io.modelcontextprotocol/tasks", method)
		}
		if err := mcp.AddReceivingCustomMethod(server, method,
			func(ctx context.Context, session *mcp.ServerSession, params *rawParams) (*rawResult, error) {
				if strings.HasPrefix(method, "tasks/") {
					if !rawDeclaresExtension(params.raw, "io.modelcontextprotocol/tasks") {
						return nil, missingCapability(map[string]any{"extensions": map[string]any{"io.modelcontextprotocol/tasks": map[string]any{}}})
					}
					if err := validateTaskParams(method, params.raw); err != nil {
						return nil, err
					}
				}
				result, err := d.call(ctx, session, method, params)
				if err == nil && strings.HasPrefix(method, "tasks/") {
					err = validateTaskMethodResult(method, result.raw)
				}
				return result, protocolError(err)
			}); err != nil {
			return nil, err
		}
	}

	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "tools/list", "tools/call":
				if !hasTools {
					return nil, methodNotFound(method)
				}
				if method == "tools/call" && hasTasks {
					call, ok := req.(*mcp.ServerRequest[*mcp.CallToolParamsRaw])
					if !ok {
						return nil, fmt.Errorf("tools/call has unexpected request type %T", req)
					}
					contract, ok := toolContracts[call.Params.Name]
					if !ok {
						return nil, invalidParams("unknown tool %q", call.Params.Name)
					}
					if err := validateArguments(call.Params.Name, call.Params.Arguments, contract.input); err != nil {
						return nil, err
					}
					raw, err := json.Marshal(call.Params)
					if err != nil {
						return nil, err
					}
					result, err := d.call(ctx, call.Session, method, &rawParams{raw: raw})
					if err != nil {
						return nil, protocolError(err)
					}
					if err := validateTaskAwareToolResult(result.raw, contract.output, call.ClientCapabilities()); err != nil {
						return nil, protocolError(err)
					}
					return result, nil
				}
			case "prompts/list", "prompts/get":
				if !hasPrompts {
					return nil, methodNotFound(method)
				}
			case "resources/list", "resources/templates/list", "resources/read":
				if !hasResources {
					return nil, methodNotFound(method)
				}
			case "completion/complete":
				if !hasCompletion {
					return nil, methodNotFound(method)
				}
			case "initialize":
				return nil, methodNotFound(method)
			}
			return next(ctx, method, req)
		}
	})
	return server, nil
}

func validateArguments(name string, raw json.RawMessage, schema *jsonschema.Resolved) error {
	var arguments any = map[string]any{}
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return invalidParams("tool %q arguments: %v", name, err)
		}
	}
	if err := schema.Validate(arguments); err != nil {
		return invalidParams("tool %q arguments: %v", name, err)
	}
	return nil
}

func validateTaskAwareToolResult(raw json.RawMessage, output *jsonschema.Resolved, capabilities *mcp.ClientCapabilities) error {
	var head struct {
		ResultType    string          `json:"resultType"`
		TaskID        string          `json:"taskId"`
		Status        string          `json:"status"`
		CreatedAt     string          `json:"createdAt"`
		LastUpdatedAt string          `json:"lastUpdatedAt"`
		TTL           json.RawMessage `json:"ttlMs"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return err
	}
	if head.ResultType == "task" {
		if capabilities == nil || capabilities.Extensions == nil {
			return missingCapability(map[string]any{"extensions": map[string]any{"io.modelcontextprotocol/tasks": map[string]any{}}})
		}
		if _, ok := capabilities.Extensions["io.modelcontextprotocol/tasks"]; !ok {
			return missingCapability(map[string]any{"extensions": map[string]any{"io.modelcontextprotocol/tasks": map[string]any{}}})
		}
		if err := validateTaskBase(head.TaskID, head.Status, head.CreatedAt, head.LastUpdatedAt, head.TTL); err != nil {
			return fmt.Errorf("task result: %w", err)
		}
		return nil
	}
	var result mcp.CallToolResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("tool result: %w", err)
	}
	if result.NeedsInput() {
		if len(result.InputRequests) == 0 || result.RequestState == "" {
			return fmt.Errorf("input_required result requires inputRequests and requestState")
		}
		for _, input := range result.InputRequests {
			switch input.(type) {
			case *mcp.ElicitParams:
				if capabilities == nil || capabilities.Elicitation == nil {
					return missingCapability(map[string]any{"elicitation": map[string]any{}})
				}
			case *mcp.CreateMessageParams, *mcp.CreateMessageWithToolsParams:
				if capabilities == nil || capabilities.Sampling == nil {
					return missingCapability(map[string]any{"sampling": map[string]any{}})
				}
			case *mcp.ListRootsParams:
				if capabilities == nil || capabilities.RootsV2 == nil {
					return missingCapability(map[string]any{"roots": map[string]any{}})
				}
			}
		}
		return nil
	}
	if output != nil {
		if err := output.Validate(result.StructuredContent); err != nil {
			return fmt.Errorf("tool structured result: %w", err)
		}
	}
	return nil
}

func validateTaskParams(method string, raw json.RawMessage) error {
	var params struct {
		TaskID string `json:"taskId"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return invalidParams("%s params: %v", method, err)
	}
	if params.TaskID == "" {
		return invalidParams("%s requires taskId", method)
	}
	return nil
}

func validateTaskMethodResult(method string, raw json.RawMessage) error {
	var result struct {
		ResultType    string                     `json:"resultType"`
		TaskID        string                     `json:"taskId"`
		Status        string                     `json:"status"`
		CreatedAt     string                     `json:"createdAt"`
		LastUpdatedAt string                     `json:"lastUpdatedAt"`
		TTL           json.RawMessage            `json:"ttlMs"`
		InputRequests map[string]json.RawMessage `json:"inputRequests"`
		Result        json.RawMessage            `json:"result"`
		Error         json.RawMessage            `json:"error"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("%s result: %w", method, err)
	}
	if result.ResultType != "complete" {
		return fmt.Errorf("%s resultType must be complete", method)
	}
	if method != "tasks/get" {
		return nil
	}
	if err := validateTaskBase(result.TaskID, result.Status, result.CreatedAt, result.LastUpdatedAt, result.TTL); err != nil {
		return fmt.Errorf("tasks/get result: %w", err)
	}
	switch result.Status {
	case "input_required":
		if len(result.InputRequests) == 0 {
			return fmt.Errorf("tasks/get input_required result requires inputRequests")
		}
	case "completed":
		if len(result.Result) == 0 {
			return fmt.Errorf("tasks/get completed result requires result")
		}
	case "failed":
		if len(result.Error) == 0 {
			return fmt.Errorf("tasks/get failed result requires error")
		}
	}
	return nil
}

func validateTaskBase(taskID, status, createdAt, lastUpdatedAt string, ttl json.RawMessage) error {
	if taskID == "" || !validTaskStatus(status) {
		return fmt.Errorf("requires taskId and valid status")
	}
	if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
		return fmt.Errorf("createdAt must be RFC 3339")
	}
	if _, err := time.Parse(time.RFC3339, lastUpdatedAt); err != nil {
		return fmt.Errorf("lastUpdatedAt must be RFC 3339")
	}
	if len(ttl) == 0 {
		return fmt.Errorf("requires ttlMs")
	}
	if string(ttl) != "null" {
		var milliseconds int64
		if err := json.Unmarshal(ttl, &milliseconds); err != nil || milliseconds < 0 {
			return fmt.Errorf("ttlMs must be a non-negative integer or null")
		}
	}
	return nil
}

func validTaskStatus(status string) bool {
	switch status {
	case "working", "input_required", "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func rawDeclaresExtension(raw json.RawMessage, name string) bool {
	var params struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return false
	}
	var capabilities struct {
		Extensions map[string]json.RawMessage `json:"extensions"`
	}
	if json.Unmarshal(params.Meta[mcp.MetaKeyClientCapabilities], &capabilities) != nil {
		return false
	}
	_, ok := capabilities.Extensions[name]
	return ok
}

func missingCapability(required map[string]any) error {
	data, _ := json.Marshal(map[string]any{"requiredCapabilities": required})
	return &jsonrpc.Error{Code: -32021, Message: "missing required client capability", Data: data}
}

func resolveSchema(value any) (*jsonschema.Resolved, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	return schema.Resolve(nil)
}

func callInto[T any](d *dispatcher, ctx context.Context, session *mcp.ServerSession, method string, params any) (*T, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	result, err := d.call(ctx, session, method, &rawParams{raw: raw})
	if err != nil {
		return nil, protocolError(err)
	}
	var typed T
	if err := json.Unmarshal(result.raw, &typed); err != nil {
		return nil, protocolError(fmt.Errorf("dispatcher %s result: %w", method, err))
	}
	return &typed, nil
}

func invalidParams(format string, args ...any) error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf(format, args...)}
}

func methodNotFound(method string) error {
	return &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: fmt.Sprintf("method not found: %q", method)}
}

func protocolError(err error) error {
	if err == nil {
		return nil
	}
	var wire *jsonrpc.Error
	if errors.As(err, &wire) {
		return err
	}
	return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: err.Error()}
}

func RunStdio(ctx context.Context, server *mcp.Server) error {
	return server.Run(ctx, &mcp.StdioTransport{})
}

func HTTPHandler(server *mcp.Server) http.Handler {
	base := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.Header.Get("Mcp-Method"), "tasks/") && r.Body != nil {
			body, err := io.ReadAll(io.LimitReader(r.Body, (16<<20)+1))
			_ = r.Body.Close()
			if err == nil && len(body) <= 16<<20 {
				r.Body = io.NopCloser(bytes.NewReader(body))
				var call struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Params struct {
						TaskID string `json:"taskId"`
					} `json:"params"`
				}
				if json.Unmarshal(body, &call) == nil && strings.HasPrefix(call.Method, "tasks/") && r.Header.Get("Mcp-Name") != call.Params.TaskID {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(struct {
						JSONRPC string          `json:"jsonrpc"`
						ID      json.RawMessage `json:"id"`
						Error   *jsonrpc.Error  `json:"error"`
					}{"2.0", call.ID, &jsonrpc.Error{Code: -32020, Message: fmt.Sprintf("Mcp-Name header %q does not match taskId %q", r.Header.Get("Mcp-Name"), call.Params.TaskID)}})
					return
				}
			}
		}
		base.ServeHTTP(w, r)
	})
}

type dispatcher struct {
	cfg          Config
	allowLogging bool
}

func (d *dispatcher) call(parent context.Context, session *mcp.ServerSession, method string, params *rawParams) (*rawResult, error) {
	ctx := parent
	var cancel context.CancelFunc
	if d.cfg.Timeout > 0 {
		ctx, cancel = context.WithTimeout(parent, d.cfg.Timeout)
		defer cancel()
	}
	raw, err := params.MarshalJSON()
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > d.cfg.MaxInput {
		return nil, fmt.Errorf("request params exceed %d bytes", d.cfg.MaxInput)
	}
	argv := append(append([]string(nil), d.cfg.Dispatcher[1:]...), method)
	cmd := exec.CommandContext(ctx, d.cfg.Dispatcher[0], argv...)
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Stderr = d.cfg.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	events, eventWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("dispatcher event pipe: %w", err)
	}
	cmd.ExtraFiles = []*os.File{eventWriter}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killGroup(cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = time.Second
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		_ = events.Close()
		_ = eventWriter.Close()
		return nil, fmt.Errorf("dispatcher stdout: %w", err)
	}
	cmd.Stdout = stdoutWriter
	if err := cmd.Start(); err != nil {
		_ = events.Close()
		_ = eventWriter.Close()
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		return nil, fmt.Errorf("dispatcher %s: %w", method, err)
	}
	_ = eventWriter.Close()
	_ = stdoutWriter.Close()
	eventDone := make(chan error, 1)
	var requestMeta struct {
		Meta mcp.Meta `json:"_meta"`
	}
	_ = json.Unmarshal(raw, &requestMeta)
	progressToken := requestMeta.Meta["progressToken"]
	go func() {
		eventDone <- forwardEvents(ctx, session, events, progressToken, d.allowLogging)
		_ = events.Close()
	}()
	output := make(chan struct {
		body []byte
		err  error
	}, 1)
	go func() {
		body, readErr := io.ReadAll(io.LimitReader(stdout, d.cfg.MaxOutput+1))
		_ = stdout.Close()
		output <- struct {
			body []byte
			err  error
		}{body, readErr}
	}()
	waitErr := cmd.Wait()
	// The invocation owns the whole process group. A grandchild retaining an
	// inherited descriptor must not extend the request or survive it.
	_ = killGroup(cmd.Process.Pid, syscall.SIGKILL)
	read := <-output
	eventErr := <-eventDone
	if eventErr != nil {
		return nil, fmt.Errorf("dispatcher %s events: %w", method, eventErr)
	}
	if read.err != nil {
		return nil, fmt.Errorf("dispatcher %s output: %w", method, read.err)
	}
	if int64(len(read.body)) > d.cfg.MaxOutput {
		return nil, fmt.Errorf("dispatcher output exceeds %d bytes", d.cfg.MaxOutput)
	}
	body := bytes.TrimSpace(read.body)
	if len(body) == 0 {
		return nil, fmt.Errorf("dispatcher %s produced no JSON result", method)
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == 1 {
			var wire jsonrpc.Error
			if json.Unmarshal(body, &wire) != nil || wire.Code == 0 || wire.Message == "" {
				return nil, fmt.Errorf("dispatcher %s exit 1 requires a JSON-RPC error object", method)
			}
			return nil, &wire
		}
		return nil, fmt.Errorf("dispatcher %s: %w", method, waitErr)
	}
	if !json.Valid(body) || body[0] != '{' {
		return nil, fmt.Errorf("dispatcher %s stdout must be one JSON object", method)
	}
	body, err = withDefaultResultType(body)
	if err != nil {
		return nil, fmt.Errorf("dispatcher %s result: %w", method, err)
	}
	return &rawResult{raw: append(json.RawMessage(nil), body...)}, nil
}

func withDefaultResultType(body []byte) ([]byte, error) {
	var result map[string]json.RawMessage
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if _, ok := result["resultType"]; !ok {
		result["resultType"] = json.RawMessage(`"complete"`)
	}
	return json.Marshal(result)
}

func forwardEvents(ctx context.Context, session *mcp.ServerSession, r io.Reader, progressToken any, allowLogging bool) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for line := 1; scanner.Scan(); line++ {
		var event struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		switch event.Method {
		case "notifications/progress":
			var params mcp.ProgressNotificationParams
			if err := json.Unmarshal(event.Params, &params); err != nil {
				return fmt.Errorf("line %d progress: %w", line, err)
			}
			if progressToken == nil || !reflect.DeepEqual(params.ProgressToken, progressToken) {
				return fmt.Errorf("line %d progress token was not requested", line)
			}
			if err := session.NotifyProgress(ctx, &params); err != nil {
				return fmt.Errorf("line %d progress: %w", line, err)
			}
		case "notifications/message":
			if !allowLogging {
				return fmt.Errorf("line %d logging capability is not declared", line)
			}
			var params mcp.LoggingMessageParams
			if err := json.Unmarshal(event.Params, &params); err != nil {
				return fmt.Errorf("line %d message: %w", line, err)
			}
			if err := session.Log(ctx, &params); err != nil {
				return fmt.Errorf("line %d message: %w", line, err)
			}
		default:
			return fmt.Errorf("line %d: unsupported notification method %q", line, event.Method)
		}
	}
	return scanner.Err()
}

type rawParams struct {
	mcp.ParamsBase
	raw json.RawMessage
}

func (p *rawParams) UnmarshalJSON(data []byte) error {
	p.raw = append(p.raw[:0], data...)
	var meta struct {
		Meta mcp.Meta `json:"_meta"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return err
	}
	p.Meta = meta.Meta
	return nil
}

func (p *rawParams) MarshalJSON() ([]byte, error) {
	var body map[string]any
	if len(p.raw) != 0 {
		if err := json.Unmarshal(p.raw, &body); err != nil {
			return nil, err
		}
	}
	if body == nil {
		body = make(map[string]any)
	}
	if p.Meta != nil {
		body["_meta"] = p.Meta
	}
	return json.Marshal(body)
}

type rawResult struct {
	mcp.ResultBase
	raw json.RawMessage
}

func (r *rawResult) MarshalJSON() ([]byte, error) {
	var body map[string]any
	if len(r.raw) != 0 {
		if err := json.Unmarshal(r.raw, &body); err != nil {
			return nil, err
		}
	}
	if body == nil {
		body = make(map[string]any)
	}
	if r.Meta != nil {
		body["_meta"] = r.Meta
	}
	return json.Marshal(body)
}

func killGroup(pid int, signal syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
