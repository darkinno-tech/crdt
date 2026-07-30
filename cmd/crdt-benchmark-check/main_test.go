package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const benchmarkSamples = `goos: linux
BenchmarkFast-1  1000  100.0 ns/op  20 B/op  2 allocs/op
BenchmarkFast-1  1000  110.0 ns/op  20 B/op  2 allocs/op
BenchmarkFast-1  1000  120.0 ns/op  20 B/op  2 allocs/op
BenchmarkFast-1  1000  130.0 ns/op  20 B/op  2 allocs/op
BenchmarkFast-1  1000  140.0 ns/op  20 B/op  2 allocs/op
BenchmarkRelay/receivers_4-1  1000  200.0 ns/op  1.00 MB/s  40 B/op  4 allocs/op
BenchmarkRelay/receivers_4-1  1000  210.0 ns/op  1.00 MB/s  40 B/op  4 allocs/op
BenchmarkRelay/receivers_4-1  1000  220.0 ns/op  1.00 MB/s  40 B/op  4 allocs/op
BenchmarkRelay/receivers_4-1  1000  230.0 ns/op  1.00 MB/s  40 B/op  4 allocs/op
BenchmarkRelay/receivers_4-1  1000  240.0 ns/op  1.00 MB/s  40 B/op  4 allocs/op
`

func TestParse(t *testing.T) {
	parsed, err := parse(strings.NewReader(benchmarkSamples + "BenchmarkNoCPUSuffix  1000  100.0 ns/op  20 B/op  2 allocs/op\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(parsed["BenchmarkFast"]); got != 5 {
		t.Fatalf("BenchmarkFast samples = %d, want 5", got)
	}
	if got := parsed["BenchmarkRelay/receivers_4"][2]; got.nsPerOp != 220 || got.bytesPerOp != 40 || got.allocsPerOp != 4 {
		t.Fatalf("relay sample = %#v", got)
	}
	if got := parsed["BenchmarkNoCPUSuffix"][0].nsPerOp; got != 100 {
		t.Fatalf("no-CPU benchmark ns/op = %.2f, want 100", got)
	}
}

func TestParseRejectsNoSamples(t *testing.T) {
	if _, err := parse(strings.NewReader("PASS\n")); err == nil {
		t.Fatal("parse succeeded without benchmarks")
	}
	if _, err := parse(failingReader{}); err == nil {
		t.Fatal("parse succeeded after a read error")
	}
}

func TestMedian(t *testing.T) {
	got := median([]sample{{30, 1, 4}, {10, 4, 1}, {20, 3, 2}, {40, 2, 3}})
	if want := (sample{25, 2.5, 2.5}); got != want {
		t.Fatalf("median = %#v, want %#v", got, want)
	}
}

func TestCompare(t *testing.T) {
	base, err := parse(strings.NewReader(benchmarkSamples))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := parse(strings.NewReader(strings.Replace(benchmarkSamples, "100.0 ns/op", "110.0 ns/op", 1)))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := compare(base, candidate, []string{"BenchmarkFast", "BenchmarkRelay/receivers_4"}, 5, 0.25, 0.05, 0.05, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "BenchmarkRelay/receivers_4") {
		t.Fatalf("comparison output = %q", output.String())
	}
}

func TestCompareRejectsRegressionAndUnexpectedBenchmark(t *testing.T) {
	base, err := parse(strings.NewReader(benchmarkSamples))
	if err != nil {
		t.Fatal(err)
	}
	regressed := cloneResults(base)
	regressed["BenchmarkFast"] = []sample{{200, 20, 2}, {210, 20, 2}, {220, 20, 2}, {230, 20, 2}, {240, 20, 2}}
	if err := compare(base, regressed, nil, 5, 0.1, 0.05, 0.05, ioDiscard{}); err == nil {
		t.Fatal("compare accepted time regression")
	}
	regressed["BenchmarkUnexpected"] = regressed["BenchmarkFast"]
	if err := compare(base, regressed, nil, 5, 0.5, 0.05, 0.05, ioDiscard{}); err == nil {
		t.Fatal("compare accepted unexpected benchmark")
	}
}

func TestCompareRejectsIncompleteSamplesAndMetricRegressions(t *testing.T) {
	base, err := parse(strings.NewReader(benchmarkSamples))
	if err != nil {
		t.Fatal(err)
	}
	if err := compare(base, base, nil, 6, 0.25, 0.05, 0.05, ioDiscard{}); err == nil {
		t.Fatal("compare accepted incomplete samples")
	}
	bytesRegressed := cloneResults(base)
	bytesRegressed["BenchmarkFast"] = []sample{{100, 30, 2}, {110, 30, 2}, {120, 30, 2}, {130, 30, 2}, {140, 30, 2}}
	if err := compare(base, bytesRegressed, nil, 5, 0.25, 0.05, 0.05, ioDiscard{}); err == nil {
		t.Fatal("compare accepted allocation-byte regression")
	}
	allocsRegressed := cloneResults(base)
	allocsRegressed["BenchmarkFast"] = []sample{{100, 20, 3}, {110, 20, 3}, {120, 20, 3}, {130, 20, 3}, {140, 20, 3}}
	if err := compare(base, allocsRegressed, nil, 5, 0.25, 0.05, 0.05, ioDiscard{}); err == nil {
		t.Fatal("compare accepted allocation-count regression")
	}
}

func TestCompareRejectsMissingAndZeroBaselineMetrics(t *testing.T) {
	base, err := parse(strings.NewReader(benchmarkSamples))
	if err != nil {
		t.Fatal(err)
	}
	missing := cloneResults(base)
	delete(missing, "BenchmarkFast")
	if err := compare(base, missing, []string{"BenchmarkFast"}, 5, 0.25, 0.05, 0.05, ioDiscard{}); err == nil {
		t.Fatal("compare accepted missing benchmark")
	}
	if err := compare(base, missing, nil, 5, 0.25, 0.05, 0.05, ioDiscard{}); err == nil {
		t.Fatal("compare accepted a candidate missing a baseline benchmark")
	}
	if err := compare(missing, base, []string{"BenchmarkFast"}, 5, 0.25, 0.05, 0.05, ioDiscard{}); err == nil {
		t.Fatal("compare accepted a baseline missing a required benchmark")
	}
	if err := withinLimit("BenchmarkZero", "allocs/op", 0, 1, 0); err == nil {
		t.Fatal("withinLimit accepted an increase from zero")
	}
	if err := withinLimit("BenchmarkZero", "allocs/op", 0, 0, 0); err != nil {
		t.Fatal(err)
	}
}

func TestRun(t *testing.T) {
	directory := t.TempDir()
	basePath := filepath.Join(directory, "base.txt")
	candidatePath := filepath.Join(directory, "candidate.txt")
	if err := os.WriteFile(basePath, []byte(benchmarkSamples), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidatePath, []byte(benchmarkSamples), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if status := run([]string{"-base", basePath, "-candidate", candidatePath, "-require", "BenchmarkFast"}, &stdout, &stderr); status != 0 {
		t.Fatalf("run status = %d, stderr = %s", status, stderr.String())
	}
	if status := run([]string{"-base", basePath, "-candidate", candidatePath, "-minimum-samples", "0"}, &stdout, &stderr); status != 2 {
		t.Fatalf("invalid run status = %d", status)
	}
	if status := run([]string{"-base", basePath}, &stdout, &stderr); status != 2 {
		t.Fatalf("missing-argument status = %d", status)
	}
	if status := run([]string{"-base", basePath, "-candidate", filepath.Join(directory, "missing.txt")}, &stdout, &stderr); status != 2 {
		t.Fatalf("missing-file status = %d", status)
	}
	if status := run([]string{"-base", basePath, "-candidate", candidatePath, "-max-time-regression", "-1"}, &stdout, &stderr); status != 2 {
		t.Fatalf("invalid-limit status = %d", status)
	}
	if status := run([]string{"-unknown"}, &stdout, &stderr); status != 2 {
		t.Fatalf("unknown-flag status = %d", status)
	}
	var values stringList
	if err := values.Set("BenchmarkFast"); err != nil || values.String() != "[BenchmarkFast]" {
		t.Fatalf("string list = %q, %v", values.String(), err)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(data []byte) (int, error) { return len(data), nil }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func cloneResults(source results) results {
	cloned := make(results, len(source))
	for name, samples := range source {
		cloned[name] = append([]sample(nil), samples...)
	}
	return cloned
}
