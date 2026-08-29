package main

import "testing"

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
