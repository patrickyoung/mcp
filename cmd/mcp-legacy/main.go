package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/patrickyoung/mcp/internal/mcpcli"
	"github.com/patrickyoung/mcp/internal/mcpclient"
)

const version = "0.2.1"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return mcpcli.Run(ctx, mcpcli.Program{
		Name:      "mcp-legacy",
		Version:   version,
		Lifecycle: mcpclient.LegacyLifecycle,
	}, argv, stdin, stdout, stderr)
}
