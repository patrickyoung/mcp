package mcpclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func endpointTransport(endpoint Endpoint, opts Options, stderr io.Writer, maxOutput int64) (mcp.Transport, error) {
	switch endpoint.Type {
	case "stdio":
		if endpoint.Command == "" {
			return nil, fmt.Errorf("stdio endpoint has no command")
		}
		return &commandTransport{endpoint: endpoint, stderr: stderr, maxOutput: maxOutput, grace: defaultCloseGrace}, nil
	case "http":
		if endpoint.URL == "" {
			return nil, fmt.Errorf("HTTP endpoint has no URL")
		}
		rt := http.RoundTripper(http.DefaultTransport)
		rt = &headerRoundTripper{next: rt, headers: opts.Headers, maxOutput: maxOutput, routeName: opts.RouteName}
		client := &http.Client{
			Transport: rt,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				// Following 301/302/307/308 can transmit an effect to a second
				// endpoint. Endpoint changes are operator decisions.
				return http.ErrUseLastResponse
			},
		}
		return &mcp.StreamableClientTransport{
			Endpoint:             endpoint.URL,
			HTTPClient:           client,
			MaxRetries:           -1,
			DisableStandaloneSSE: true,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported endpoint type %q", endpoint.Type)
	}
}

const defaultCloseGrace = 1_000_000_000 // one second as a duration

type headerRoundTripper struct {
	next      http.RoundTripper
	headers   http.Header
	maxOutput int64
	routeName string
}

func (r *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	for name, values := range r.headers {
		for _, value := range values {
			clone.Header.Add(name, value)
		}
	}
	if clone.Header.Get("Mcp-Name") == "" && r.routeName != "" {
		clone.Header.Set("Mcp-Name", r.routeName)
	}
	// Tasks is an extension, so the SDK cannot infer its routing name. Preserve
	// the body while supplying the header required by the extension spec.
	if clone.Body != nil && clone.Header.Get("Mcp-Name") == "" {
		body, err := io.ReadAll(clone.Body)
		if err != nil {
			return nil, err
		}
		_ = clone.Body.Close()
		clone.Body = io.NopCloser(bytes.NewReader(body))
		clone.ContentLength = int64(len(body))
		var call struct {
			Method string `json:"method"`
			Params struct {
				TaskID string `json:"taskId"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &call) == nil && strings.HasPrefix(call.Method, "tasks/") && call.Params.TaskID != "" {
			clone.Header.Set("Mcp-Name", call.Params.TaskID)
		}
	}
	resp, err := r.next.RoundTrip(clone)
	if err != nil {
		return nil, err
	}
	if resp.Body != nil && r.maxOutput > 0 {
		resp.Body = &boundedReadCloser{ReadCloser: resp.Body, remaining: r.maxOutput}
	}
	return resp, nil
}

func readHeaders(r io.Reader, max int64) (http.Header, error) {
	if r == nil {
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > max {
		return nil, fmt.Errorf("HTTP headers exceed %d bytes", max)
	}
	headers := make(http.Header)
	for lineNo, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("invalid HTTP header on line %d", lineNo+1)
		}
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		lowerName := strings.ToLower(name)
		if lowerName == "host" || lowerName == "content-length" || strings.HasPrefix(lowerName, "mcp-") {
			return nil, fmt.Errorf("HTTP header %q is protocol-owned", name)
		}
		headers.Add(name, strings.TrimSpace(value))
	}
	return headers, nil
}

// ReadHeaders exposes the bounded line-oriented header parser to the command.
func ReadHeaders(r io.Reader, max int64) (http.Header, error) { return readHeaders(r, max) }

var _ mcp.Transport = (*mcp.StreamableClientTransport)(nil)
var _ http.RoundTripper = (*headerRoundTripper)(nil)
