package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/patrickyoung/mcp/internal/admit"
	"github.com/yosida95/uritemplate/v3"
)

const (
	ProtocolVersion = "2026-07-28"
	DefaultMaxInput = int64(16 << 20)
	DefaultMaxWire  = int64(64 << 20)
)

// Lifecycle selects the MCP session establishment contract. ModernLifecycle
// is deliberately the zero value so existing callers remain strict-modern.
type Lifecycle uint8

const (
	ModernLifecycle Lifecycle = iota
	LegacyLifecycle
)

var legacyProtocolVersions = map[string]struct{}{
	"2025-11-25": {},
	"2025-06-18": {},
	"2025-03-26": {},
	"2024-11-05": {},
}

type Endpoint = admit.Endpoint

type Options struct {
	Lifecycle     Lifecycle
	Timeout       time.Duration
	MaxInput      int64
	MaxOutput     int64
	Stderr        io.Writer
	Events        io.Writer
	Listen        io.Writer
	Headers       http.Header
	Capabilities  map[string]any
	Subscriptions json.RawMessage
	RouteName     string
}

type Outcome struct {
	Raw  json.RawMessage
	Code int
}

type session struct {
	client    *mcp.Client
	conn      *mcp.ClientSession
	recorder  *recorder
	endpoint  Endpoint
	discovery json.RawMessage
}

func ResolveEndpoint(argv []string) (Endpoint, error) {
	if len(argv) == 0 {
		return Endpoint{}, fmt.Errorf("missing server command")
	}
	if len(argv) == 1 {
		u, err := url.Parse(argv[0])
		if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			return Endpoint{Type: "http", URL: u.String()}, nil
		}
	}
	command, err := exec.LookPath(argv[0])
	if err != nil {
		return Endpoint{}, fmt.Errorf("server command %q: %w", argv[0], err)
	}
	command, err = canonicalExecutable(command)
	if err != nil {
		return Endpoint{}, err
	}
	return Endpoint{Type: "stdio", Command: command, Args: append([]string(nil), argv[1:]...), Path: os.Getenv("PATH")}, nil
}

func ReadParams(r io.Reader, max int64) (json.RawMessage, error) {
	if max <= 0 {
		max = DefaultMaxInput
	}
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("input exceeds %d bytes", max)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return json.RawMessage("{}"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value map[string]any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("stdin must be one JSON object: %w", err)
	}
	if value == nil {
		return nil, fmt.Errorf("stdin must be one JSON object")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("stdin must contain exactly one JSON object")
	}
	return json.Marshal(value)
}

func Discover(ctx context.Context, endpoint Endpoint, opts Options) (Outcome, error) {
	runCtx, cancel := withTimeout(ctx, opts.Timeout)
	if cancel != nil {
		defer cancel()
	}
	s, err := connect(runCtx, endpoint, opts, false)
	if err != nil {
		return Outcome{}, err
	}
	defer s.conn.Close()
	return Outcome{Raw: append(json.RawMessage(nil), s.discovery...), Code: 0}, nil
}

func Request(ctx context.Context, endpoint Endpoint, method string, params json.RawMessage, opts Options) (Outcome, error) {
	if method == "" {
		return Outcome{}, fmt.Errorf("missing method")
	}
	runCtx, cancel := withTimeout(ctx, opts.Timeout)
	if cancel != nil {
		defer cancel()
	}
	s, err := connect(runCtx, endpoint, opts, false)
	if err != nil {
		return Outcome{}, err
	}
	defer s.conn.Close()
	s.recorder.Begin(methodMayEffect(method))
	err = callStandardOrCustom(runCtx, s.client, s.conn, method, params)
	return finish(ctx, s.recorder, err)
}

// Tool verifies the server's current descriptor in the same connection and
// only then performs the admitted call. Discovery and verification are not
// counted as the tool effect; the tools/call write is the irreversible edge.
func Tool(ctx context.Context, endpoint Endpoint, name, expected string, arguments json.RawMessage, opts Options) (Outcome, error) {
	if name == "" || expected == "" {
		return Outcome{}, fmt.Errorf("tool name and expected descriptor digest are required")
	}
	runCtx, cancel := withTimeout(ctx, opts.Timeout)
	if cancel != nil {
		defer cancel()
	}
	s, err := connect(runCtx, endpoint, opts, false)
	if err != nil {
		return Outcome{}, err
	}
	defer s.conn.Close()

	descriptor, err := findTool(runCtx, s, name)
	if err != nil {
		return Outcome{}, err
	}
	digest, err := admit.Digest("tools", endpoint, s.discovery, descriptor)
	if err != nil {
		return Outcome{}, err
	}
	if digest != expected {
		return Outcome{}, fmt.Errorf("tool %q descriptor changed: expected %s, got %s", name, expected, digest)
	}

	var args any
	dec := json.NewDecoder(bytes.NewReader(arguments))
	dec.UseNumber()
	if err := dec.Decode(&args); err != nil {
		return Outcome{}, fmt.Errorf("tool arguments: %w", err)
	}
	s.recorder.Begin(true)
	_, err = s.conn.CallTool(runCtx, &mcp.CallToolParams{Name: name, Arguments: args})
	return finish(ctx, s.recorder, err)
}

// Prompt verifies one admitted prompt descriptor, then retrieves the prompt.
// The input object is the prompt's string-valued arguments map.
func Prompt(ctx context.Context, endpoint Endpoint, name, expected string, arguments json.RawMessage, opts Options) (Outcome, error) {
	if name == "" || expected == "" {
		return Outcome{}, fmt.Errorf("prompt name and expected descriptor digest are required")
	}
	runCtx, cancel := withTimeout(ctx, opts.Timeout)
	if cancel != nil {
		defer cancel()
	}
	s, err := connect(runCtx, endpoint, opts, false)
	if err != nil {
		return Outcome{}, err
	}
	defer s.conn.Close()
	descriptor, err := findPrompt(runCtx, s, name)
	if err != nil {
		return Outcome{}, err
	}
	digest, err := admit.Digest("prompts", endpoint, s.discovery, descriptor)
	if err != nil {
		return Outcome{}, err
	}
	if digest != expected {
		return Outcome{}, fmt.Errorf("prompt %q descriptor changed: expected %s, got %s", name, expected, digest)
	}
	var args map[string]string
	if err := json.Unmarshal(arguments, &args); err != nil {
		return Outcome{}, fmt.Errorf("prompt arguments must be a JSON object of strings: %w", err)
	}
	s.recorder.Begin(false)
	_, err = s.conn.GetPrompt(runCtx, &mcp.GetPromptParams{Name: name, Arguments: args})
	return finish(ctx, s.recorder, err)
}

// ReadResource verifies one admitted resource descriptor, then reads its URI.
func ReadResource(ctx context.Context, endpoint Endpoint, uri, expected string, opts Options) (Outcome, error) {
	if uri == "" || expected == "" {
		return Outcome{}, fmt.Errorf("resource URI and expected descriptor digest are required")
	}
	runCtx, cancel := withTimeout(ctx, opts.Timeout)
	if cancel != nil {
		defer cancel()
	}
	s, err := connect(runCtx, endpoint, opts, false)
	if err != nil {
		return Outcome{}, err
	}
	defer s.conn.Close()
	descriptor, err := findResource(runCtx, s, uri)
	if err != nil {
		return Outcome{}, err
	}
	digest, err := admit.Digest("resources", endpoint, s.discovery, descriptor)
	if err != nil {
		return Outcome{}, err
	}
	if digest != expected {
		return Outcome{}, fmt.Errorf("resource %q descriptor changed: expected %s, got %s", uri, expected, digest)
	}
	s.recorder.Begin(false)
	_, err = s.conn.ReadResource(runCtx, &mcp.ReadResourceParams{URI: uri})
	return finish(ctx, s.recorder, err)
}

// ReadTemplateResource verifies an admitted URI-template descriptor, verifies
// that uri is an expansion of it, and then reads the concrete URI.
func ReadTemplateResource(ctx context.Context, endpoint Endpoint, template, uri, expected string, opts Options) (Outcome, error) {
	if template == "" || uri == "" || expected == "" {
		return Outcome{}, fmt.Errorf("resource template, URI, and expected descriptor digest are required")
	}
	runCtx, cancel := withTimeout(ctx, opts.Timeout)
	if cancel != nil {
		defer cancel()
	}
	s, err := connect(runCtx, endpoint, opts, false)
	if err != nil {
		return Outcome{}, err
	}
	defer s.conn.Close()
	descriptor, err := findTemplate(runCtx, s, template)
	if err != nil {
		return Outcome{}, err
	}
	digest, err := admit.Digest("templates", endpoint, s.discovery, descriptor)
	if err != nil {
		return Outcome{}, err
	}
	if digest != expected {
		return Outcome{}, fmt.Errorf("resource template %q descriptor changed: expected %s, got %s", template, expected, digest)
	}
	tmpl, err := uritemplate.New(template)
	if err != nil {
		return Outcome{}, fmt.Errorf("invalid admitted resource template %q: %w", template, err)
	}
	if tmpl.Match(uri) == nil {
		return Outcome{}, fmt.Errorf("resource URI %q is not an expansion of admitted template %q", uri, template)
	}
	s.recorder.Begin(false)
	_, err = s.conn.ReadResource(runCtx, &mcp.ReadResourceParams{URI: uri})
	return finish(ctx, s.recorder, err)
}

func connect(parent context.Context, endpoint Endpoint, opts Options, listening bool) (*session, error) {
	if opts.Lifecycle != ModernLifecycle && opts.Lifecycle != LegacyLifecycle {
		return nil, fmt.Errorf("unknown MCP lifecycle mode %d", opts.Lifecycle)
	}
	ctx := parent
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	maxOutput := opts.MaxOutput
	if maxOutput <= 0 {
		maxOutput = DefaultMaxWire
	}
	base, err := endpointTransport(endpoint, opts, stderr, maxOutput)
	if err != nil {
		return nil, err
	}
	rec := new(recorder)

	events := &jsonlWriter{w: opts.Events}
	listen := &jsonlWriter{w: opts.Listen}
	rec.extensionNotifications = events
	if listening {
		rec.extensionNotifications = listen
	}
	capabilities, err := clientCapabilities(opts.Capabilities, opts.Lifecycle)
	if err != nil {
		return nil, err
	}
	clientOpts := &mcp.ClientOptions{
		Capabilities:   capabilities,
		MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			events.write("notifications/progress", req.Params)
		},
	}
	if listening {
		clientOpts.ElicitationCompleteHandler = func(_ context.Context, req *mcp.ElicitationCompleteNotificationRequest) {
			listen.write("notifications/elicitation/complete", req.Params)
		}
		clientOpts.ToolListChangedHandler = func(_ context.Context, req *mcp.ToolListChangedRequest) {
			listen.write("notifications/tools/list_changed", req.Params)
		}
		clientOpts.PromptListChangedHandler = func(_ context.Context, req *mcp.PromptListChangedRequest) {
			listen.write("notifications/prompts/list_changed", req.Params)
		}
		clientOpts.ResourceListChangedHandler = func(_ context.Context, req *mcp.ResourceListChangedRequest) {
			listen.write("notifications/resources/list_changed", req.Params)
		}
		clientOpts.ResourceUpdatedHandler = func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			listen.write("notifications/resources/updated", req.Params)
		}
		clientOpts.LoggingMessageHandler = func(_ context.Context, req *mcp.LoggingMessageRequest) {
			listen.write("notifications/message", req.Params)
		}
	}
	clientName := "unix-mcp"
	if opts.Lifecycle == LegacyLifecycle {
		clientName = "unix-mcp-legacy"
	}
	c := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: "0.3.0"}, clientOpts)
	transport := rec.transport(base)
	if opts.Lifecycle == LegacyLifecycle {
		transport = forceLegacyTransport(transport)
	} else {
		transport = requireModernTransport(transport)
	}
	cs, err := c.Connect(ctx, transport, nil)
	if err != nil {
		return nil, classifyBeforeEffect(parent, err)
	}
	init := cs.InitializeResult()
	if opts.Lifecycle == LegacyLifecycle {
		if init == nil {
			_ = cs.Close()
			return nil, fmt.Errorf("legacy initialize produced no negotiated protocol version")
		}
		if _, ok := legacyProtocolVersions[init.ProtocolVersion]; !ok {
			_ = cs.Close()
			return nil, fmt.Errorf("server negotiated %s; a supported legacy MCP version is required", init.ProtocolVersion)
		}
		response, ok := rec.Last("initialize")
		if !ok || len(response.Result) == 0 || len(response.Error) != 0 {
			_ = cs.Close()
			return nil, fmt.Errorf("initialize produced no trustworthy result")
		}
		return &session{client: c, conn: cs, recorder: rec, endpoint: endpoint, discovery: response.Result}, nil
	}
	if init == nil || init.ProtocolVersion != ProtocolVersion {
		_ = cs.Close()
		version := "unknown"
		if init != nil {
			version = init.ProtocolVersion
		}
		return nil, fmt.Errorf("server negotiated %s; modern stateless MCP %s is required", version, ProtocolVersion)
	}
	response, ok := rec.Last("server/discover")
	if !ok || len(response.Result) == 0 || len(response.Error) != 0 {
		_ = cs.Close()
		return nil, fmt.Errorf("server/discover produced no trustworthy result")
	}
	return &session{client: c, conn: cs, recorder: rec, endpoint: endpoint, discovery: response.Result}, nil
}

func Listen(ctx context.Context, endpoint Endpoint, opts Options) error {
	if opts.Lifecycle == LegacyLifecycle {
		return fmt.Errorf("listen is unavailable in legacy compatibility mode")
	}
	runCtx, cancel := withTimeout(ctx, opts.Timeout)
	if cancel != nil {
		defer cancel()
	}
	s, err := connect(runCtx, endpoint, opts, true)
	if err != nil {
		return err
	}
	defer s.conn.Close()
	listenErr := startSubscriptions(runCtx, s, opts.Subscriptions)
	if listenErr != nil {
		return listenErr
	}
	done := make(chan error, 1)
	go func() { done <- s.conn.Wait() }()
	select {
	case <-runCtx.Done():
		return runCtx.Err()
	case err := <-done:
		if err == nil {
			return io.ErrUnexpectedEOF
		}
		return err
	}
}

func clientCapabilities(extra map[string]any, lifecycle Lifecycle) (*mcp.ClientCapabilities, error) {
	// A Unix caller can preserve task handles, inspect polymorphic results, and
	// issue every task lifecycle request, so modern task support is truthful by
	// default. Legacy extensions and deprecated client capabilities are explicit.
	caps := &mcp.ClientCapabilities{}
	if lifecycle == ModernLifecycle {
		caps.AddExtension("io.modelcontextprotocol/tasks", nil)
	}
	if extra == nil {
		return caps, nil
	}
	raw, err := json.Marshal(extra)
	if err != nil {
		return nil, fmt.Errorf("client capabilities: %w", err)
	}
	if err := json.Unmarshal(raw, caps); err != nil {
		return nil, fmt.Errorf("client capabilities: %w", err)
	}
	if lifecycle == ModernLifecycle {
		if caps.Extensions == nil {
			caps.Extensions = make(map[string]any)
		}
		if _, ok := caps.Extensions["io.modelcontextprotocol/tasks"]; !ok {
			caps.AddExtension("io.modelcontextprotocol/tasks", nil)
		}
	}
	return caps, nil
}

const unixListenMethod = "io.patrickyoung.unix/subscriptions-listen"

func startSubscriptions(ctx context.Context, s *session, subscriptions json.RawMessage) error {
	// v1.7.0 implements subscriptions/listen but does not yet export the
	// ClientSession method. A sending middleware changes only the local alias;
	// the official SDK still supplies metadata, owns the call, dispatches
	// notifications, and puts subscriptions/listen on the wire.
	if err := mcp.AddSendingCustomMethod[*rawParams, *rawResult](s.client, unixListenMethod); err != nil {
		return err
	}
	s.client.AddSendingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != unixListenMethod {
				return next(ctx, method, req)
			}
			_, err := next(ctx, "subscriptions/listen", req)
			return &rawResult{}, err
		}
	})
	params := subscriptions
	if len(bytes.TrimSpace(params)) == 0 || string(bytes.TrimSpace(params)) == "{}" {
		params = json.RawMessage(`{"notifications":{"toolsListChanged":true,"promptsListChanged":true,"resourcesListChanged":true}}`)
	}
	_, err := mcp.CallCustomMethod[*rawParams, *rawResult](ctx, s.conn, unixListenMethod, &rawParams{raw: params})
	return err
}

func findTool(ctx context.Context, s *session, name string) (json.RawMessage, error) {
	cursor := ""
	for {
		s.recorder.Begin(false)
		_, err := s.conn.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		outcome, finishErr := finish(ctx, s.recorder, err)
		if finishErr != nil {
			return nil, finishErr
		}
		if outcome.Code != 0 {
			return nil, fmt.Errorf("listing tools returned exit %d", outcome.Code)
		}
		var page struct {
			Tools      []json.RawMessage `json:"tools"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(outcome.Raw, &page); err != nil {
			return nil, fmt.Errorf("tools/list result: %w", err)
		}
		for _, raw := range page.Tools {
			var head struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(raw, &head) == nil && head.Name == name {
				return raw, nil
			}
		}
		if page.NextCursor == "" {
			return nil, fmt.Errorf("tool %q is no longer advertised", name)
		}
		cursor = page.NextCursor
	}
}

func findPrompt(ctx context.Context, s *session, name string) (json.RawMessage, error) {
	cursor := ""
	for {
		s.recorder.Begin(false)
		_, err := s.conn.ListPrompts(ctx, &mcp.ListPromptsParams{Cursor: cursor})
		outcome, finishErr := finish(ctx, s.recorder, err)
		if finishErr != nil {
			return nil, finishErr
		}
		var page struct {
			Prompts    []json.RawMessage `json:"prompts"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(outcome.Raw, &page); err != nil {
			return nil, fmt.Errorf("prompts/list result: %w", err)
		}
		for _, raw := range page.Prompts {
			var head struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(raw, &head) == nil && head.Name == name {
				return raw, nil
			}
		}
		if page.NextCursor == "" {
			return nil, fmt.Errorf("prompt %q is no longer advertised", name)
		}
		cursor = page.NextCursor
	}
}

func findResource(ctx context.Context, s *session, uri string) (json.RawMessage, error) {
	cursor := ""
	for {
		s.recorder.Begin(false)
		_, err := s.conn.ListResources(ctx, &mcp.ListResourcesParams{Cursor: cursor})
		outcome, finishErr := finish(ctx, s.recorder, err)
		if finishErr != nil {
			return nil, finishErr
		}
		var page struct {
			Resources  []json.RawMessage `json:"resources"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(outcome.Raw, &page); err != nil {
			return nil, fmt.Errorf("resources/list result: %w", err)
		}
		for _, raw := range page.Resources {
			var head struct {
				URI string `json:"uri"`
			}
			if json.Unmarshal(raw, &head) == nil && head.URI == uri {
				return raw, nil
			}
		}
		if page.NextCursor == "" {
			return nil, fmt.Errorf("resource %q is no longer advertised", uri)
		}
		cursor = page.NextCursor
	}
}

func findTemplate(ctx context.Context, s *session, uriTemplate string) (json.RawMessage, error) {
	cursor := ""
	for {
		s.recorder.Begin(false)
		_, err := s.conn.ListResourceTemplates(ctx, &mcp.ListResourceTemplatesParams{Cursor: cursor})
		outcome, finishErr := finish(ctx, s.recorder, err)
		if finishErr != nil {
			return nil, finishErr
		}
		var page struct {
			Templates  []json.RawMessage `json:"resourceTemplates"`
			NextCursor string            `json:"nextCursor"`
		}
		if err := json.Unmarshal(outcome.Raw, &page); err != nil {
			return nil, fmt.Errorf("resources/templates/list result: %w", err)
		}
		for _, raw := range page.Templates {
			var head struct {
				URITemplate string `json:"uriTemplate"`
			}
			if json.Unmarshal(raw, &head) == nil && head.URITemplate == uriTemplate {
				return raw, nil
			}
		}
		if page.NextCursor == "" {
			return nil, fmt.Errorf("resource template %q is no longer advertised", uriTemplate)
		}
		cursor = page.NextCursor
	}
}

func finish(parent context.Context, rec *recorder, callErr error) (Outcome, error) {
	response, ready := rec.Target()
	if callErr != nil {
		if ready && len(response.Error) != 0 {
			if rec.DuplicateWithin(2 * time.Millisecond) {
				return Outcome{}, &ExitError{Code: 125, Err: fmt.Errorf("duplicate terminal response")}
			}
			return Outcome{Raw: response.Error, Code: 1}, nil
		}
		if rec.Sent() {
			return Outcome{}, &ExitError{Code: 125, Err: callErr}
		}
		if errors.Is(parent.Err(), context.Canceled) {
			return Outcome{}, &ExitError{Code: 130, Err: parent.Err()}
		}
		return Outcome{}, callErr
	}
	if !ready || len(response.Result) == 0 {
		code := 2
		if rec.Sent() {
			code = 125
		}
		return Outcome{}, &ExitError{Code: code, Err: fmt.Errorf("no trustworthy terminal response")}
	}
	if rec.DuplicateWithin(2 * time.Millisecond) {
		return Outcome{}, &ExitError{Code: 125, Err: fmt.Errorf("duplicate terminal response")}
	}
	return Outcome{Raw: response.Result, Code: resultCode(response.Result)}, nil
}

func resultCode(raw []byte) int {
	var result struct {
		IsError    bool   `json:"isError"`
		ResultType string `json:"resultType"`
		Status     string `json:"status"`
		Task       *struct {
			Status string `json:"status"`
		} `json:"task"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return 0
	}
	if result.IsError {
		return 1
	}
	if result.Task != nil {
		switch result.Task.Status {
		case "working", "input_required":
			return 75
		case "failed", "cancelled":
			return 1
		case "completed":
			return 0
		}
	}
	switch result.Status {
	case "working", "input_required":
		return 75
	case "failed", "cancelled":
		return 1
	case "completed":
		return 0
	}
	if result.ResultType == "input_required" || result.ResultType == "task" {
		return 75
	}
	return 0
}

func methodMayEffect(method string) bool {
	switch method {
	case "tools/list", "prompts/list", "prompts/get", "resources/list",
		"resources/templates/list", "resources/read", "completion/complete", "tasks/get":
		return false
	default:
		// Unknown extension requests are conservatively effectful. Extension
		// authors may put an observation behind mcp request, but a lost response
		// must never invite an automatic replay of an unclassified method.
		return true
	}
}

type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

func callStandardOrCustom(ctx context.Context, c *mcp.Client, cs *mcp.ClientSession, method string, raw json.RawMessage) error {
	switch method {
	case "tools/list":
		var p mcp.ListToolsParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		_, err := cs.ListTools(ctx, &p)
		return err
	case "tools/call":
		var p mcp.CallToolParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		_, err := cs.CallTool(ctx, &p)
		return err
	case "prompts/list":
		var p mcp.ListPromptsParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		_, err := cs.ListPrompts(ctx, &p)
		return err
	case "prompts/get":
		var p mcp.GetPromptParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		_, err := cs.GetPrompt(ctx, &p)
		return err
	case "resources/list":
		var p mcp.ListResourcesParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		_, err := cs.ListResources(ctx, &p)
		return err
	case "resources/templates/list":
		var p mcp.ListResourceTemplatesParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		_, err := cs.ListResourceTemplates(ctx, &p)
		return err
	case "resources/read":
		var p mcp.ReadResourceParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		_, err := cs.ReadResource(ctx, &p)
		return err
	case "completion/complete":
		var p mcp.CompleteParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		_, err := cs.Complete(ctx, &p)
		return err
	case "server/discover":
		return fmt.Errorf("%s is lifecycle machinery; use mcp discover", method)
	case "initialize", "ping", "logging/setLevel", "resources/subscribe", "resources/unsubscribe":
		return fmt.Errorf("%s was removed from modern stateless MCP", method)
	}

	p := &rawParams{raw: append(json.RawMessage(nil), raw...)}
	if err := mcp.AddSendingCustomMethod[*rawParams, *rawResult](c, method); err != nil {
		return err
	}
	_, err := mcp.CallCustomMethod[*rawParams, *rawResult](ctx, cs, method, p)
	return err
}

type rawParams struct {
	mcp.ParamsBase
	raw json.RawMessage
}

func (p *rawParams) MarshalJSON() ([]byte, error) {
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(p.raw))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil {
		return nil, err
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

func (r *rawResult) UnmarshalJSON(data []byte) error {
	r.raw = append(r.raw[:0], data...)
	var body struct {
		Meta mcp.Meta `json:"_meta"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return err
	}
	r.Meta = body.Meta
	return nil
}

func withTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return parent, nil
	}
	return context.WithTimeout(parent, timeout)
}

func classifyBeforeEffect(parent context.Context, err error) error {
	if errors.Is(parent.Err(), context.Canceled) {
		return &ExitError{Code: 130, Err: err}
	}
	return err
}

func canonicalExecutable(path string) (string, error) {
	resolved, err := exec.LookPath(path)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(resolved, "/") {
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return "", err
		}
	}
	if final, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = final
	}
	return filepath.Clean(resolved), nil
}

type jsonlWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *jsonlWriter) write(method string, params any) {
	if w.w == nil {
		return
	}
	record := struct {
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{method, params}
	raw, err := json.Marshal(record)
	if err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.w.Write(append(raw, '\n'))
}

func (w *jsonlWriter) writeRaw(method string, params json.RawMessage) {
	if w == nil || w.w == nil {
		return
	}
	record := struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params,omitempty"`
	}{method, append(json.RawMessage(nil), params...)}
	raw, err := json.Marshal(record)
	if err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.w.Write(append(raw, '\n'))
}
