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
	const want = "inventory=[filter-17 pump-42]\nconcurrent-statuses=[inspection-required maintenance]\nrecovered-status=assigned\n"
	if output.String() != want {
		t.Fatalf("run() output = %q, want %q", output.String(), want)
	}
}
