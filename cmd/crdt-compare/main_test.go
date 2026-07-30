package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCompareRunProducesBoundedReports(t *testing.T) {
	var output bytes.Buffer
	if err := run([]string{"-sizes", "1,3", "-samples", "1", "-warmups", "0", "-iterations", "1", "-revision", "test"}, &output); err != nil {
		t.Fatal(err)
	}
	var reports []report
	if err := json.Unmarshal(output.Bytes(), &reports); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 || reports[0].Runes != 1 || reports[1].Runes != 3 || reports[0].Revision != "test" {
		t.Fatalf("reports = %#v", reports)
	}
	for _, item := range reports {
		if item.UpdateBytes <= 0 || item.StateBytes <= 0 || len(item.SamplesMS) != 1 || item.MedianMS < 0 {
			t.Fatalf("invalid report = %#v", item)
		}
	}
}

func TestCompareHelpersAndInvalidArguments(t *testing.T) {
	if sizes, err := parseSizes(" 1,2 "); err != nil || !reflect.DeepEqual(sizes, []int{1, 2}) {
		t.Fatalf("parseSizes = %v, %v", sizes, err)
	}
	for _, value := range []string{"", "0", "one", "1,-2"} {
		if _, err := parseSizes(value); err == nil {
			t.Fatalf("parseSizes(%q) succeeded", value)
		}
	}
	if duration, updateBytes, stateBytes, err := runBatch("x", 1); err != nil || duration < 0 || updateBytes == 0 || stateBytes == 0 {
		t.Fatalf("runBatch = %v, %d, %d, %v", duration, updateBytes, stateBytes, err)
	}
	if item, err := measure(1, 1, 1, 1, "revision"); err != nil || item.MedianMS < 0 || item.Revision != "revision" {
		t.Fatalf("measure = %#v, %v", item, err)
	}
	for _, args := range [][]string{{"-sizes", "0"}, {"-samples", "0"}, {"-warmups", "-1"}, {"-iterations", "0"}, {"-unknown"}} {
		if err := run(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("run(%q) succeeded", args)
		}
	}
}

func TestCompareWritesRequestedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if err := run([]string{"-sizes", "1", "-samples", "1", "-warmups", "0", "-iterations", "1", "-output", path}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || len(contents) == 0 {
		t.Fatalf("report file = %q, %v", contents, err)
	}
	if _, closeWriter, err := outputWriter("-"); err != nil || closeWriter() != nil {
		t.Fatalf("stdout writer = %v", err)
	}
}
