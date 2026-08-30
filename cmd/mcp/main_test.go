package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestVersionContract(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"version"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("version exit=%d stderr=%q", code, stderr.String())
	}
	if stdout.String() != "mcp 0.2.1\n" {
		t.Fatalf("version output = %q", stdout.String())
	}
}
