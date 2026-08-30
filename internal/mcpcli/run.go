package mcpcli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/patrickyoung/mcp/internal/mcpclient"
)

type Program struct {
	Name      string
	Version   string
	Lifecycle mcpclient.Lifecycle
	Listen    bool
}

type runner struct {
	program Program
}

func Run(ctx context.Context, program Program, argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if program.Name == "" {
		program.Name = "mcp"
	}
	return (&runner{program: program}).run(ctx, argv, stdin, stdout, stderr)
}

func (r *runner) run(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		r.usage(stderr)
		return 2
	}
	switch argv[0] {
	case "-h", "--help", "help":
		r.usage(stdout)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, r.program.Name+" "+r.program.Version)
		return 0
	case "discover":
		cfg, rest, err := parseCommon("discover", argv[1:], stderr)
		if err != nil {
			return r.diagnose(stderr, err)
		}
		server, err := afterFlagSeparator(rest)
		if err != nil {
			return r.diagnose(stderr, err)
		}
		endpoint, err := mcpclient.ResolveEndpoint(server)
		if err != nil {
			return r.diagnose(stderr, err)
		}
		opts, err := cfg.options(stderr, r.program.Lifecycle)
		if err != nil {
			return r.diagnose(stderr, err)
		}
		outcome, err := mcpclient.Discover(ctx, endpoint, opts)
		return r.emit(stdout, stderr, outcome, err)
	case "request":
		cfg, rest, err := parseCommon("request", argv[1:], stderr)
		if err != nil {
			return r.diagnose(stderr, err)
		}
		if len(rest) == 0 {
			return r.diagnose(stderr, fmt.Errorf("request: missing METHOD"))
		}
		method := rest[0]
		server, err := afterSeparator(rest[1:])
		if err != nil {
			return r.diagnose(stderr, err)
		}
		params, err := mcpclient.ReadParams(stdin, cfg.maxInput)
		if err != nil {
			return r.diagnose(stderr, err)
		}
		endpoint, err := mcpclient.ResolveEndpoint(server)
		if err != nil {
			return r.diagnose(stderr, err)
		}
		opts, err := cfg.options(stderr, r.program.Lifecycle)
		if err != nil {
			return r.diagnose(stderr, err)
		}
		outcome, err := mcpclient.Request(ctx, endpoint, method, params, opts)
		return r.emit(stdout, stderr, outcome, err)
	case "tool":
		return r.runTool(ctx, argv[1:], stdin, stdout, stderr)
	case "prompt":
		return r.runPrompt(ctx, argv[1:], stdin, stdout, stderr)
	case "read":
		return r.runRead(ctx, argv[1:], stdout, stderr)
	case "template-read":
		return r.runTemplateRead(ctx, argv[1:], stdout, stderr)
	case "listen":
		if !r.program.Listen {
			fmt.Fprintf(stderr, "%s: unknown command %q\n", r.program.Name, argv[0])
			r.usage(stderr)
			return 2
		}
		cfg, rest, err := parseCommon("listen", argv[1:], stderr)
		if err != nil {
			return r.diagnose(stderr, err)
		}
		server, err := afterFlagSeparator(rest)
		if err != nil {
			return r.diagnose(stderr, err)
		}
		endpoint, err := mcpclient.ResolveEndpoint(server)
		if err != nil {
			return r.diagnose(stderr, err)
		}
		opts, err := cfg.options(stderr, r.program.Lifecycle)
		if err != nil {
			return r.diagnose(stderr, err)
		}
		subscriptions, err := mcpclient.ReadParams(stdin, cfg.maxInput)
		if err != nil {
			return r.diagnose(stderr, err)
		}
		opts.Subscriptions = subscriptions
		opts.Listen = stdout
		if err := mcpclient.Listen(ctx, endpoint, opts); err != nil {
			if errors.Is(err, context.Canceled) {
				return 130
			}
			return r.diagnose(stderr, err)
		}
		return 0
	default:
		fmt.Fprintf(stderr, "%s: unknown command %q\n", r.program.Name, argv[0])
		r.usage(stderr)
		return 2
	}
}

func (r *runner) runTemplateRead(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("template-read", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg commonConfig
	fs.DurationVar(&cfg.timeout, "timeout", 0, "request timeout (zero means none)")
	fs.Int64Var(&cfg.maxOutput, "max-output", mcpclient.DefaultMaxWire, "maximum server bytes")
	fs.IntVar(&cfg.eventFD, "event-fd", -1, "descriptor for progress JSONL")
	fs.IntVar(&cfg.headerFD, "header-fd", -1, "descriptor containing HTTP headers")
	fs.StringVar(&cfg.capabilitiesFile, "capabilities", "", "additional client capabilities JSON file")
	fs.StringVar(&cfg.routeName, "route-name", "", "MCP routing name for extension requests")
	expect := fs.String("expect", "", "reviewed descriptor digest")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) < 2 || *expect == "" {
		return r.diagnose(stderr, fmt.Errorf("template-read: -expect DIGEST, TEMPLATE, and URI are required"))
	}
	server, err := afterSeparator(rest[2:])
	if err != nil {
		return r.diagnose(stderr, err)
	}
	endpoint, err := mcpclient.ResolveEndpoint(server)
	if err != nil {
		return r.diagnose(stderr, err)
	}
	opts, err := cfg.options(stderr, r.program.Lifecycle)
	if err != nil {
		return r.diagnose(stderr, err)
	}
	outcome, err := mcpclient.ReadTemplateResource(ctx, endpoint, rest[0], rest[1], *expect, opts)
	return r.emit(stdout, stderr, outcome, err)
}

func (r *runner) runPrompt(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cfg, expect, name, server, err := parseAdmitted("prompt", argv, stderr)
	if err != nil {
		return r.diagnose(stderr, err)
	}
	params, err := mcpclient.ReadParams(stdin, cfg.maxInput)
	if err != nil {
		return r.diagnose(stderr, err)
	}
	endpoint, err := mcpclient.ResolveEndpoint(server)
	if err != nil {
		return r.diagnose(stderr, err)
	}
	opts, err := cfg.options(stderr, r.program.Lifecycle)
	if err != nil {
		return r.diagnose(stderr, err)
	}
	outcome, err := mcpclient.Prompt(ctx, endpoint, name, expect, params, opts)
	return r.emit(stdout, stderr, outcome, err)
}

func (r *runner) runRead(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	cfg, expect, uri, server, err := parseAdmitted("read", argv, stderr)
	if err != nil {
		return r.diagnose(stderr, err)
	}
	endpoint, err := mcpclient.ResolveEndpoint(server)
	if err != nil {
		return r.diagnose(stderr, err)
	}
	opts, err := cfg.options(stderr, r.program.Lifecycle)
	if err != nil {
		return r.diagnose(stderr, err)
	}
	outcome, err := mcpclient.ReadResource(ctx, endpoint, uri, expect, opts)
	return r.emit(stdout, stderr, outcome, err)
}

func parseAdmitted(command string, argv []string, stderr io.Writer) (commonConfig, string, string, []string, error) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg commonConfig
	fs.DurationVar(&cfg.timeout, "timeout", 0, "request timeout (zero means none)")
	fs.Int64Var(&cfg.maxInput, "max-input", mcpclient.DefaultMaxInput, "maximum stdin bytes")
	fs.Int64Var(&cfg.maxOutput, "max-output", mcpclient.DefaultMaxWire, "maximum server bytes")
	fs.IntVar(&cfg.eventFD, "event-fd", -1, "descriptor for progress JSONL")
	fs.IntVar(&cfg.headerFD, "header-fd", -1, "descriptor containing HTTP headers")
	fs.StringVar(&cfg.capabilitiesFile, "capabilities", "", "additional client capabilities JSON file")
	expect := fs.String("expect", "", "reviewed descriptor digest")
	if err := fs.Parse(argv); err != nil {
		return cfg, "", "", nil, err
	}
	rest := fs.Args()
	if len(rest) == 0 || *expect == "" {
		return cfg, "", "", nil, fmt.Errorf("%s: -expect DIGEST and NAME/URI are required", command)
	}
	server, err := afterSeparator(rest[1:])
	if err != nil {
		return cfg, "", "", nil, err
	}
	return cfg, *expect, rest[0], server, nil
}

func (r *runner) runTool(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tool", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", 0, "request timeout (zero means none)")
	maxInput := fs.Int64("max-input", mcpclient.DefaultMaxInput, "maximum stdin bytes")
	maxOutput := fs.Int64("max-output", mcpclient.DefaultMaxWire, "maximum server bytes")
	eventFD := fs.Int("event-fd", -1, "descriptor for progress JSONL")
	headerFD := fs.Int("header-fd", -1, "descriptor containing HTTP headers")
	capabilitiesFile := fs.String("capabilities", "", "additional client capabilities JSON file")
	routeName := fs.String("route-name", "", "MCP routing name for extension requests")
	expect := fs.String("expect", "", "reviewed descriptor digest")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return r.diagnose(stderr, fmt.Errorf("tool: missing NAME"))
	}
	name := rest[0]
	server, err := afterSeparator(rest[1:])
	if err != nil {
		return r.diagnose(stderr, err)
	}
	params, err := mcpclient.ReadParams(stdin, *maxInput)
	if err != nil {
		return r.diagnose(stderr, err)
	}
	endpoint, err := mcpclient.ResolveEndpoint(server)
	if err != nil {
		return r.diagnose(stderr, err)
	}
	cfg := commonConfig{timeout: *timeout, maxInput: *maxInput, maxOutput: *maxOutput, eventFD: *eventFD, headerFD: *headerFD, capabilitiesFile: *capabilitiesFile, routeName: *routeName}
	opts, err := cfg.options(stderr, r.program.Lifecycle)
	if err != nil {
		return r.diagnose(stderr, err)
	}
	outcome, err := mcpclient.Tool(ctx, endpoint, name, *expect, params, opts)
	return r.emit(stdout, stderr, outcome, err)
}

type commonConfig struct {
	timeout          time.Duration
	maxInput         int64
	maxOutput        int64
	eventFD          int
	headerFD         int
	capabilitiesFile string
	routeName        string
}

func parseCommon(name string, argv []string, stderr io.Writer) (commonConfig, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg commonConfig
	fs.DurationVar(&cfg.timeout, "timeout", 0, "request timeout (zero means none)")
	fs.Int64Var(&cfg.maxInput, "max-input", mcpclient.DefaultMaxInput, "maximum stdin bytes")
	fs.Int64Var(&cfg.maxOutput, "max-output", mcpclient.DefaultMaxWire, "maximum server bytes")
	fs.IntVar(&cfg.eventFD, "event-fd", -1, "descriptor for progress JSONL")
	fs.IntVar(&cfg.headerFD, "header-fd", -1, "descriptor containing HTTP headers")
	fs.StringVar(&cfg.capabilitiesFile, "capabilities", "", "additional client capabilities JSON file")
	fs.StringVar(&cfg.routeName, "route-name", "", "MCP routing name for extension requests")
	if err := fs.Parse(argv); err != nil {
		return commonConfig{}, nil, err
	}
	if cfg.maxInput <= 0 || cfg.maxOutput <= 0 {
		return commonConfig{}, nil, fmt.Errorf("byte limits must be positive")
	}
	if cfg.eventFD >= 0 && cfg.eventFD < 3 {
		return commonConfig{}, nil, fmt.Errorf("event descriptor must be 3 or greater")
	}
	if cfg.headerFD >= 0 && cfg.headerFD < 3 {
		return commonConfig{}, nil, fmt.Errorf("header descriptor must be 3 or greater")
	}
	return cfg, fs.Args(), nil
}

func (c commonConfig) options(stderr io.Writer, lifecycle mcpclient.Lifecycle) (mcpclient.Options, error) {
	opts := mcpclient.Options{Lifecycle: lifecycle, Timeout: c.timeout, MaxInput: c.maxInput, MaxOutput: c.maxOutput, Stderr: stderr}
	opts.RouteName = c.routeName
	if c.eventFD >= 3 {
		opts.Events = os.NewFile(uintptr(c.eventFD), "mcp-events-fd-"+strconv.Itoa(c.eventFD))
	}
	if c.headerFD >= 3 {
		f := os.NewFile(uintptr(c.headerFD), "mcp-headers-fd-"+strconv.Itoa(c.headerFD))
		headers, err := mcpclient.ReadHeaders(f, 1<<20)
		if err != nil {
			return opts, err
		}
		opts.Headers = headers
	}
	if c.capabilitiesFile != "" {
		raw, err := os.ReadFile(c.capabilitiesFile)
		if err != nil {
			return opts, fmt.Errorf("reading client capabilities: %w", err)
		}
		if err := json.Unmarshal(raw, &opts.Capabilities); err != nil {
			return opts, fmt.Errorf("client capabilities must be one JSON object: %w", err)
		}
	}
	return opts, nil
}

func afterSeparator(args []string) ([]string, error) {
	if len(args) == 0 || args[0] != "--" || len(args) == 1 {
		return nil, fmt.Errorf("expected -- SERVER [ARG ...]")
	}
	return args[1:], nil
}

// flag.FlagSet consumes a leading -- when it parses all options before the
// server argv, as discover and listen do. Commands with a METHOD/NAME before
// the separator use afterSeparator instead because parsing stops at that word.
func afterFlagSeparator(args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("expected -- SERVER [ARG ...]")
	}
	return args, nil
}

func (r *runner) emit(stdout, stderr io.Writer, outcome mcpclient.Outcome, err error) int {
	if err != nil {
		return r.diagnose(stderr, err)
	}
	if len(outcome.Raw) != 0 {
		if _, err := stdout.Write(outcome.Raw); err != nil {
			fmt.Fprintf(stderr, "%s: writing result: %v\n", r.program.Name, err)
			return 2
		}
		if len(outcome.Raw) == 0 || outcome.Raw[len(outcome.Raw)-1] != '\n' {
			_, _ = io.WriteString(stdout, "\n")
		}
	}
	return outcome.Code
}

func (r *runner) diagnose(stderr io.Writer, err error) int {
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "%s: %v\n", r.program.Name, err)
	var exitErr *mcpclient.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	return 2
}

func (r *runner) usage(w io.Writer) {
	name := r.program.Name
	fmt.Fprintln(w, "usage:")
	fmt.Fprintf(w, "  %s discover [-timeout D] -- SERVER [ARG ...]\n", name)
	fmt.Fprintf(w, "  %s request [-timeout D] METHOD -- SERVER [ARG ...]\n", name)
	fmt.Fprintf(w, "  %s tool -expect DIGEST NAME -- SERVER [ARG ...]\n", name)
	fmt.Fprintf(w, "  %s prompt -expect DIGEST NAME -- SERVER [ARG ...]\n", name)
	fmt.Fprintf(w, "  %s read -expect DIGEST URI -- SERVER [ARG ...]\n", name)
	fmt.Fprintf(w, "  %s template-read -expect DIGEST TEMPLATE URI -- SERVER [ARG ...]\n", name)
	if r.program.Listen {
		fmt.Fprintf(w, "  %s listen [-timeout D] -- SERVER [ARG ...]\n", name)
	}
	fmt.Fprintln(w, `
stdin is empty or one JSON object. stdout is the exact MCP result. Server
stderr remains stderr. Requests are never retried. Exit 75 means valid but
unfinished; exit 125 means the request may have taken effect without a
trustworthy terminal result.`)
}
