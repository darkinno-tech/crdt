package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRunShowsConvergedWorkboard(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatal(err)
	}
	const want = "completed-inspections=5\nopen-tasks=[close-shift inspect-pump replace-filter]\n"
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

func TestRunReturnsWriteError(t *testing.T) {
	writeErr := errors.New("write failed")
	err := run(failingWriter{err: writeErr})
	if !errors.Is(err, writeErr) {
		t.Fatalf("run error = %v, want wrapped %v", err, writeErr)
	}
	if err == nil || !strings.Contains(err.Error(), "write collaborative board") {
		t.Fatalf("run error = %v, want contextual write error", err)
	}
}
