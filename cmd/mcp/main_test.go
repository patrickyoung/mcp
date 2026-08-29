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

func TestServerSeparators(t *testing.T) {
	if got, err := afterFlagSeparator([]string{"server", "arg"}); err != nil || len(got) != 2 {
		t.Fatalf("flag separator result = %v, %v", got, err)
	}
	if _, err := afterFlagSeparator(nil); err == nil {
		t.Fatal("empty server argv accepted")
	}
	if got, err := afterSeparator([]string{"--", "server", "arg"}); err != nil || len(got) != 2 {
		t.Fatalf("literal separator result = %v, %v", got, err)
	}
	if _, err := afterSeparator([]string{"server"}); err == nil {
		t.Fatal("missing literal separator accepted")
	}
}
