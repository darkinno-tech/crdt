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
	var (
		sizesText  = flag.String("sizes", "4096,16384", "comma-separated UTF-8 rune counts")
		samples    = flag.Int("samples", 5, "measured samples per size")
		warmups    = flag.Int("warmups", 2, "unreported warmups per size")
		iterations = flag.Int("iterations", 20, "operations per reported sample")
		revision   = flag.String("revision", "unknown", "source revision recorded in output")
		output     = flag.String("output", "-", "output path, or - for stdout")
	)
	flag.Parse()
	if *samples <= 0 || *warmups < 0 || *iterations <= 0 {
		fatalf("samples and iterations must be positive and warmups must be non-negative")
	}
	sizes, err := parseSizes(*sizesText)
	if err != nil {
		fatalf("parse sizes: %v", err)
	}
	reports := make([]report, 0, len(sizes))
	for _, size := range sizes {
		item, err := measure(size, *samples, *warmups, *iterations, *revision)
		if err != nil {
			fatalf("measure %d runes: %v", size, err)
		}
		reports = append(reports, item)
	}
	writer, closeWriter, err := outputWriter(*output)
	if err != nil {
		fatalf("open output: %v", err)
	}
	defer closeWriter()
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(reports); err != nil {
		fatalf("encode output: %v", err)
	}
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
		updateSize, stateSize, err := run(payload)
		if err != nil {
			return 0, 0, 0, err
		}
		updateBytes, stateBytes = updateSize, stateSize
	}
	return time.Since(started), updateBytes, stateBytes, nil
}

func run(payload string) (int, int, error) {
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
