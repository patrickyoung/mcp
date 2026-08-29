package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/patrickyoung/mcp/internal/mcpserve"
)

const version = "0.2.1"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:]))
}

func run(ctx context.Context, argv []string) int {
	if len(argv) == 1 {
		switch argv[0] {
		case "version", "--version":
			fmt.Fprintln(os.Stdout, "mcpserve "+version)
			return 0
		case "help", "-h", "--help":
			usage()
			return 0
		}
	}
	fs := flag.NewFlagSet("mcpserve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	httpAddr := fs.String("http", "", "serve stateless Streamable HTTP on address")
	timeout := fs.Duration("timeout", 0, "per-filter timeout (zero means none)")
	maxInput := fs.Int64("max-input", 16<<20, "maximum request parameter bytes")
	maxOutput := fs.Int64("max-output", 64<<20, "maximum filter result bytes")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	rest := fs.Args()
	if *maxInput <= 0 || *maxOutput <= 0 {
		return fail(fmt.Errorf("byte limits must be positive"))
	}
	if len(rest) < 3 || rest[1] != "--" {
		usage()
		return 2
	}
	manifest, err := mcpserve.LoadManifest(rest[0])
	if err != nil {
		return fail(err)
	}
	server, err := mcpserve.New(manifest, mcpserve.Config{
		Dispatcher: rest[2:], Stderr: os.Stderr, Timeout: *timeout,
		MaxInput: *maxInput, MaxOutput: *maxOutput,
	})
	if err != nil {
		return fail(err)
	}
	if *httpAddr == "" {
		if err := mcpserve.RunStdio(ctx, server); err != nil && ctx.Err() == nil {
			return fail(err)
		}
		return 0
	}
	httpServer := &http.Server{
		Addr: *httpAddr, Handler: mcpserve.HTTPHandler(server),
		ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 1 << 20,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fail(err)
	}
	return 0
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "mcpserve: %v\n", err)
	return 2
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  mcpserve [-http ADDR] [-timeout D] MANIFEST -- FILTER [ARG ...]

Serve the descriptors in MANIFEST through MCP. Each non-list capability
request runs FILTER with the MCP method appended to argv, exact params JSON on
stdin, result JSON on stdout, diagnostics on stderr, and exit status as the
outcome. Without -http, MCP itself uses stdin and stdout.`)
}
