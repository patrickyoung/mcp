package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/patrickyoung/mcp/internal/box"
	"github.com/patrickyoung/mcp/internal/mcpclient"
)

const version = "0.3.0"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		usage(stderr)
		return 2
	}
	switch argv[0] {
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, "mcpbox "+version)
		return 0
	case "make":
		fs := flag.NewFlagSet("make", flag.ContinueOnError)
		fs.SetOutput(stderr)
		mcpPath := fs.String("mcp", "", "path to the mcp filter")
		headers := fs.String("headers", "", "HTTP header file used only during discovery")
		if err := fs.Parse(argv[1:]); err != nil {
			return 2
		}
		rest := fs.Args()
		if len(rest) < 3 || rest[1] != "--" {
			return fail(stderr, fmt.Errorf("make: expected DIR -- SERVER [ARG ...]"))
		}
		endpoint, err := mcpclient.ResolveEndpoint(rest[2:])
		if err != nil {
			return fail(stderr, err)
		}
		if err := box.Make(ctx, rest[0], endpoint, box.Config{MCP: *mcpPath, Stderr: stderr, Headers: *headers}); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stderr, "mcpbox: wrote unadmitted capability folder %s\n", rest[0])
		return 0
	case "show":
		if len(argv) != 2 {
			return fail(stderr, fmt.Errorf("show: expected DIR"))
		}
		if err := box.Show(stdout, argv[1]); err != nil {
			return fail(stderr, err)
		}
		return 0
	case "tools", "actions", "prompts", "resources", "templates":
		if len(argv) != 2 {
			return fail(stderr, fmt.Errorf("%s: expected DIR", argv[0]))
		}
		kind := argv[0]
		if kind == "actions" {
			kind = "tools"
		}
		if err := box.List(stdout, argv[1], kind); err != nil {
			return fail(stderr, err)
		}
		return 0
	case "admit":
		if len(argv) < 4 {
			return fail(stderr, fmt.Errorf("admit: expected DIR KIND NAME [...]"))
		}
		if err := box.Admit(argv[1], argv[2], argv[3:], box.Config{}); err != nil {
			return fail(stderr, err)
		}
		return 0
	case "revoke":
		if len(argv) < 4 {
			return fail(stderr, fmt.Errorf("revoke: expected DIR KIND NAME [...]"))
		}
		if err := box.Revoke(argv[1], argv[2], argv[3:], box.Config{}); err != nil {
			return fail(stderr, err)
		}
		return 0
	case "diff":
		if len(argv) != 3 {
			return fail(stderr, fmt.Errorf("diff: expected OLD NEW"))
		}
		code, err := box.Diff(ctx, argv[1], argv[2], stdout, stderr)
		if err != nil {
			return fail(stderr, err)
		}
		return code
	default:
		fmt.Fprintf(stderr, "mcpbox: unknown command %q\n", argv[0])
		usage(stderr)
		return 2
	}
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "mcpbox: %v\n", err)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `usage:
  mcpbox make [-mcp PATH] [-headers FILE] DIR -- SERVER [ARG ...]
  mcpbox show DIR
  mcpbox diff OLD NEW
  mcpbox tools|actions|prompts|resources|templates DIR
  mcpbox admit DIR KIND NAME [...]
  mcpbox revoke DIR KIND NAME [...]

make discovers into a new folder atomically and grants nothing. Listing a
kind prints name, descriptor digest, and synopsis as TSV. admit DIR actions
creates Action-compatible connectors for effectful tools; ordinary tool
admission bypasses that action gate. Changed descriptors stop at runtime before
a capability request is sent.`)
}
