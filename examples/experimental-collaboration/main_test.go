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
	const want = "assignee=west\nnote=inspect pump\nasset-tree-nodes=1\n"
	if output.String() != want {
		t.Fatalf("run() output = %q, want %q", output.String(), want)
	}
}
