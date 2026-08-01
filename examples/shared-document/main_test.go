package main

import (
	"bytes"
	"testing"
)

func TestRun(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatal(err)
	}
	const want = "title=Release plan\ntask=release-notes\ndone=false\nupdates=5\n"
	if got := output.String(); got != want {
		t.Fatalf("run output = %q, want %q", got, want)
	}
}
