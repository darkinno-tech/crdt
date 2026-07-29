package main

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestRun(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatal(err)
	}
	const want = "assignee=west\nnote=inspect pump\nasset-tree-nodes=1\n"
	if output.String() != want {
		t.Fatalf("run() output = %q, want %q", output.String(), want)
	}
}

func TestRunReturnsWriteFailure(t *testing.T) {
	if err := run(failingWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("run() error = %v, want wrapped %v", err, io.ErrClosedPipe)
	}
}
