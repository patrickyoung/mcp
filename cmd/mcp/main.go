package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/patrickyoung/mcp/internal/mcpcli"
)

const version = "0.3.0"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return mcpcli.Run(ctx, mcpcli.Program{Name: "mcp", Version: version, Listen: true}, argv, stdin, stdout, stderr)
}
