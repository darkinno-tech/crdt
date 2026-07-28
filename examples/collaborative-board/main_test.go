package main

import (
	"bytes"
	"errors"
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

func TestRunPropagatesWriterFailure(t *testing.T) {
	if err := run(failingWriter{}); !errors.Is(err, errWrite) {
		t.Fatalf("run() error = %v, want %v", err, errWrite)
	}
}

var errWrite = errors.New("write failed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }
