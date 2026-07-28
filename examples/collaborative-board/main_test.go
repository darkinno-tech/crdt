package main

import (
	"bytes"
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
