package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRunShowsWebSocketConvergence(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatal(err)
	}
	const want = "relay-value=5\nleft-value=5\nright-value=5\nfrontier-operator-a=2\nduplicate-and-out-of-order-safe=true\n"
	if got := output.String(); got != want {
		t.Fatalf("run output = %q, want %q", got, want)
	}
}

type failingWriter struct {
	err error
}

func (writer failingWriter) Write([]byte) (int, error) {
	return 0, writer.err
}

func TestRunReturnsWriterError(t *testing.T) {
	writeErr := errors.New("write failed")
	err := run(failingWriter{err: writeErr})
	if !errors.Is(err, writeErr) {
		t.Fatalf("run error = %v, want wrapped %v", err, writeErr)
	}
	if err == nil || !strings.Contains(err.Error(), "write WebSocket provider result") {
		t.Fatalf("run error = %v, want contextual write error", err)
	}
}
