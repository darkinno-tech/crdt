// Command crdt-compare produces the DarkInno side of the reproducible
// cross-library text-sync comparison. It intentionally reports bytes and
// language-specific elapsed time separately: Go and Node have different
// runtimes, allocators, and APIs, so only the workload contract is shared.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/text"
)

type report struct {
	Implementation string    `json:"implementation"`
	Runtime        string    `json:"runtime"`
	Scenario       string    `json:"scenario"`
	Runes          int       `json:"runes"`
	SamplesMS      []float64 `json:"samples_ms"`
	MedianMS       float64   `json:"median_ms"`
	UpdateBytes    int       `json:"update_bytes"`
	StateBytes     int       `json:"state_bytes"`
	Revision       string    `json:"revision"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fatalf("%v", err)
	}
}

func run(args []string, writer io.Writer) (err error) {
	flags := flag.NewFlagSet("crdt-compare", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var (
		sizesText  = flags.String("sizes", "4096,16384", "comma-separated UTF-8 rune counts")
		samples    = flags.Int("samples", 5, "measured samples per size")
		warmups    = flags.Int("warmups", 2, "unreported warmups per size")
		iterations = flags.Int("iterations", 20, "operations per reported sample")
		revision   = flags.String("revision", "unknown", "source revision recorded in output")
		output     = flags.String("output", "-", "output path (creates parent directories), or - for stdout")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *samples <= 0 || *warmups < 0 || *iterations <= 0 {
		return fmt.Errorf("samples and iterations must be positive and warmups must be non-negative")
	}
	sizes, err := parseSizes(*sizesText)
	if err != nil {
		return fmt.Errorf("parse sizes: %w", err)
	}
	reports := make([]report, 0, len(sizes))
	for _, size := range sizes {
		item, err := measure(size, *samples, *warmups, *iterations, *revision)
		if err != nil {
			return fmt.Errorf("measure %d runes: %w", size, err)
		}
		reports = append(reports, item)
	}
	destination := writer
	closeWriter := func() error { return nil }
	if *output != "-" {
		destination, closeWriter, err = outputWriter(*output)
		if err != nil {
			return fmt.Errorf("open output: %w", err)
		}
	}
	defer func() {
		if closeErr := closeWriter(); err == nil && closeErr != nil {
			err = fmt.Errorf("close output: %w", closeErr)
		}
	}()
	encoder := json.NewEncoder(destination)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(reports); err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	return nil
}

func measure(runes, samples, warmups, iterations int, revision string) (report, error) {
	payload := strings.Repeat("x", runes)
	for index := 0; index < warmups; index++ {
		if _, _, _, err := runBatch(payload, iterations); err != nil {
			return report{}, err
		}
	}
	durations := make([]float64, 0, samples)
	updateBytes, stateBytes := 0, 0
	for index := 0; index < samples; index++ {
		runtime.GC()
		duration, updateSize, stateSize, err := runBatch(payload, iterations)
		if err != nil {
			return report{}, err
		}
		durations = append(durations, float64(duration)/float64(time.Millisecond*time.Duration(iterations)))
		updateBytes, stateBytes = updateSize, stateSize
	}
	sorted := append([]float64(nil), durations...)
	sort.Float64s(sorted)
	return report{
		Implementation: "DarkInno RGA run-v2",
		Runtime:        runtime.Version(),
		Scenario:       "two-replica initial plain-text sync; create, encode update, decode, apply, and verify",
		Runes:          runes,
		SamplesMS:      durations,
		MedianMS:       sorted[len(sorted)/2],
		UpdateBytes:    updateBytes,
		StateBytes:     stateBytes,
		Revision:       revision,
	}, nil
}

func runBatch(payload string, iterations int) (time.Duration, int, int, error) {
	started := time.Now()
	updateBytes, stateBytes := 0, 0
	for index := 0; index < iterations; index++ {
		updateSize, stateSize, err := runOnce(payload)
		if err != nil {
			return 0, 0, 0, err
		}
		updateBytes, stateBytes = updateSize, stateSize
	}
	return time.Since(started), updateBytes, stateBytes, nil
}

func runOnce(payload string) (int, int, error) {
	source, err := text.New("comparison-source")
	if err != nil {
		return 0, 0, err
	}
	target, err := text.New("comparison-target")
	if err != nil {
		return 0, 0, err
	}
	encoded, err := source.InsertRunBinaryWithLimits(0, payload, frame.DefaultLimits())
	if err != nil {
		return 0, 0, err
	}
	delta, err := text.UnmarshalRGARunDelta(encoded)
	if err != nil {
		return 0, 0, err
	}
	if err := target.ApplyDelta(delta); err != nil {
		return 0, 0, err
	}
	if target.String() != payload {
		return 0, 0, fmt.Errorf("target did not converge")
	}
	state, err := source.MarshalRunBinary()
	if err != nil {
		return 0, 0, err
	}
	return len(encoded), len(state), nil
}

func parseSizes(value string) ([]int, error) {
	parts := strings.Split(value, ",")
	sizes := make([]int, 0, len(parts))
	for _, part := range parts {
		size, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || size <= 0 {
			return nil, fmt.Errorf("%q is not a positive integer", part)
		}
		sizes = append(sizes, size)
	}
	if len(sizes) == 0 {
		return nil, fmt.Errorf("no sizes")
	}
	return sizes, nil
}

func outputWriter(path string) (io.Writer, func() error, error) {
	if path == "-" {
		return os.Stdout, func() error { return nil }, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, err
	}
	file, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	return file, file.Close, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
