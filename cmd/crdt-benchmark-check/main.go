// Command crdt-benchmark-check compares controlled Go benchmark samples.
//
// It is intentionally small and dependency-free so CI can compare a candidate
// with its parent on the same runner instead of relying on machine-specific
// absolute timings.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
)

var benchmarkLine = regexp.MustCompile(`^(Benchmark\S+?)(?:-\d+)?\s+\d+\s+([0-9]+(?:\.[0-9]+)?)\s+ns/op(?:\s+[0-9]+(?:\.[0-9]+)?\s+\S+)?\s+([0-9]+(?:\.[0-9]+)?)\s+B/op\s+([0-9]+(?:\.[0-9]+)?)\s+allocs/op$`)

type sample struct {
	nsPerOp     float64
	bytesPerOp  float64
	allocsPerOp float64
}

type results map[string][]sample

type stringList []string

func (values *stringList) String() string { return fmt.Sprint([]string(*values)) }

func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("crdt-benchmark-check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	basePath := flags.String("base", "", "path to baseline benchmark output")
	candidatePath := flags.String("candidate", "", "path to candidate benchmark output")
	maxTimeRegression := flags.Float64("max-time-regression", 0.25, "maximum relative ns/op increase")
	maxBytesRegression := flags.Float64("max-bytes-regression", 0.05, "maximum relative B/op increase")
	maxAllocsRegression := flags.Float64("max-allocs-regression", 0.05, "maximum relative allocs/op increase")
	minimumSamples := flags.Int("minimum-samples", 5, "minimum samples required for every benchmark")
	var required stringList
	flags.Var(&required, "require", "benchmark name required in both results; repeatable")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *basePath == "" || *candidatePath == "" {
		_, _ = fmt.Fprintln(stderr, "both -base and -candidate are required")
		return 2
	}
	if *minimumSamples < 1 || *maxTimeRegression < 0 || *maxBytesRegression < 0 || *maxAllocsRegression < 0 {
		_, _ = fmt.Fprintln(stderr, "sample count and regression limits must be non-negative; sample count must be positive")
		return 2
	}

	base, err := parseFile(*basePath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "read baseline: %v\n", err)
		return 2
	}
	candidate, err := parseFile(*candidatePath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "read candidate: %v\n", err)
		return 2
	}
	if err := compare(base, candidate, required, *minimumSamples, *maxTimeRegression, *maxBytesRegression, *maxAllocsRegression, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "benchmark regression: %v\n", err)
		return 1
	}
	return 0
}

func parseFile(path string) (results, error) {
	// The command compares local CI artifacts selected by its caller.
	file, err := os.Open(path) // #nosec G304 -- the caller explicitly supplies both artifact paths.
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return parse(file)
}

func parse(input io.Reader) (results, error) {
	parsed := make(results)
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		matches := benchmarkLine.FindStringSubmatch(scanner.Text())
		if matches == nil {
			continue
		}
		nsPerOp, err := strconv.ParseFloat(matches[2], 64)
		if err != nil {
			return nil, fmt.Errorf("parse %s ns/op: %w", matches[1], err)
		}
		bytesPerOp, err := strconv.ParseFloat(matches[3], 64)
		if err != nil {
			return nil, fmt.Errorf("parse %s B/op: %w", matches[1], err)
		}
		allocsPerOp, err := strconv.ParseFloat(matches[4], 64)
		if err != nil {
			return nil, fmt.Errorf("parse %s allocs/op: %w", matches[1], err)
		}
		parsed[matches[1]] = append(parsed[matches[1]], sample{nsPerOp, bytesPerOp, allocsPerOp})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(parsed) == 0 {
		return nil, errors.New("no Go benchmark samples found")
	}
	return parsed, nil
}

func compare(base, candidate results, required []string, minimumSamples int, maxTime, maxBytes, maxAllocs float64, output io.Writer) error {
	for _, name := range required {
		if _, ok := base[name]; !ok {
			return fmt.Errorf("baseline is missing required %s", name)
		}
		if _, ok := candidate[name]; !ok {
			return fmt.Errorf("candidate is missing required %s", name)
		}
	}
	for name := range base {
		if _, ok := candidate[name]; !ok {
			return fmt.Errorf("candidate is missing baseline benchmark %s", name)
		}
	}
	for name := range candidate {
		if _, ok := base[name]; !ok {
			return fmt.Errorf("candidate has unexpected benchmark %s", name)
		}
	}

	names := make([]string, 0, len(base))
	for name := range base {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		baseline := base[name]
		candidateSamples := candidate[name]
		if len(baseline) < minimumSamples || len(candidateSamples) < minimumSamples {
			return fmt.Errorf("%s has %d baseline and %d candidate samples; need at least %d", name, len(baseline), len(candidateSamples), minimumSamples)
		}
		baseMedian := median(baseline)
		candidateMedian := median(candidateSamples)
		if err := withinLimit(name, "ns/op", baseMedian.nsPerOp, candidateMedian.nsPerOp, maxTime); err != nil {
			return err
		}
		if err := withinLimit(name, "B/op", baseMedian.bytesPerOp, candidateMedian.bytesPerOp, maxBytes); err != nil {
			return err
		}
		if err := withinLimit(name, "allocs/op", baseMedian.allocsPerOp, candidateMedian.allocsPerOp, maxAllocs); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(output, "%s: ns/op %.2f -> %.2f, B/op %.2f -> %.2f, allocs/op %.2f -> %.2f\n", name, baseMedian.nsPerOp, candidateMedian.nsPerOp, baseMedian.bytesPerOp, candidateMedian.bytesPerOp, baseMedian.allocsPerOp, candidateMedian.allocsPerOp); err != nil {
			return fmt.Errorf("write comparison: %w", err)
		}
	}
	return nil
}

func median(samples []sample) sample {
	return sample{
		nsPerOp:     medianMetric(samples, func(value sample) float64 { return value.nsPerOp }),
		bytesPerOp:  medianMetric(samples, func(value sample) float64 { return value.bytesPerOp }),
		allocsPerOp: medianMetric(samples, func(value sample) float64 { return value.allocsPerOp }),
	}
}

func medianMetric(samples []sample, metric func(sample) float64) float64 {
	values := make([]float64, len(samples))
	for index, value := range samples {
		values[index] = metric(value)
	}
	sort.Float64s(values)
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return (values[middle-1] + values[middle]) / 2
}

func withinLimit(name, metric string, baseline, candidate, maximumIncrease float64) error {
	if baseline == 0 {
		if candidate == 0 {
			return nil
		}
		return fmt.Errorf("%s %s increased from 0 to %.2f", name, metric, candidate)
	}
	if candidate > baseline*(1+maximumIncrease) {
		return fmt.Errorf("%s %s %.2f exceeds baseline %.2f by more than %.0f%%", name, metric, candidate, baseline, maximumIncrease*100)
	}
	return nil
}
