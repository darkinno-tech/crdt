package main

import (
	"bytes"
	"testing"
)

func TestRun(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got, want := output.String(), "recovered=true cursor=41 outbox_bytes=24\n"; got != want {
		t.Fatalf("run() output = %q, want %q", got, want)
	}
}
