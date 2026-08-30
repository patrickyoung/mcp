package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestVersionAndLegacyUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"version"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("version exit=%d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "mcp-legacy 0.2.1\n" {
		t.Fatalf("version output = %q", stdout.String())
	}
	stdout.Reset()
	if code := run(context.Background(), []string{"help"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("help exit=%d stderr=%q", code, stderr.String())
	}
	if strings.Contains(stdout.String(), " listen ") {
		t.Fatalf("legacy help advertises unsupported listen command: %q", stdout.String())
	}
}
