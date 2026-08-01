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

type comparisonScenario string

const (
	scenarioInitial           comparisonScenario = "initial"
	scenarioOfflineConcurrent comparisonScenario = "offline-concurrent"
)

func parseScenario(value string) (comparisonScenario, error) {
	scenario := comparisonScenario(value)
	switch scenario {
	case scenarioInitial, scenarioOfflineConcurrent:
		return scenario, nil
	default:
		return "", fmt.Errorf("%q is not a supported scenario", value)
	}
}

func (scenario comparisonScenario) description() string {
	switch scenario {
	case scenarioInitial:
		return "two-replica initial plain-text sync; create, encode update, decode, apply, and verify"
	case scenarioOfflineConcurrent:
		return "three replicas; two offline writers concurrently replace one shared rune, then decode duplicate and reordered updates before verifying convergence"
	default:
		return "unknown"
	}
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
		scenarioText = flags.String("scenario", string(scenarioInitial), "comparison scenario: initial or offline-concurrent")
		sizesText    = flags.String("sizes", "4096,16384", "comma-separated UTF-8 rune counts")
		samples      = flags.Int("samples", 5, "measured samples per size")
		warmups      = flags.Int("warmups", 2, "unreported warmups per size")
		iterations   = flags.Int("iterations", 20, "operations per reported sample")
		revision     = flags.String("revision", "unknown", "source revision recorded in output")
		output       = flags.String("output", "-", "output path (creates parent directories), or - for stdout")
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *samples <= 0 || *warmups < 0 || *iterations <= 0 {
		return fmt.Errorf("samples and iterations must be positive and warmups must be non-negative")
	}
	scenario, err := parseScenario(*scenarioText)
	if err != nil {
		return err
	}
	sizes, err := parseSizes(*sizesText)
	if err != nil {
		return fmt.Errorf("parse sizes: %w", err)
	}
	reports := make([]report, 0, len(sizes))
	for _, size := range sizes {
		item, err := measure(scenario, size, *samples, *warmups, *iterations, *revision)
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

func measure(scenario comparisonScenario, runes, samples, warmups, iterations int, revision string) (report, error) {
	payload := strings.Repeat("x", runes)
	for index := 0; index < warmups; index++ {
		if _, _, _, err := runBatch(scenario, payload, iterations); err != nil {
			return report{}, err
		}
	}
	durations := make([]float64, 0, samples)
	updateBytes, stateBytes := 0, 0
	for index := 0; index < samples; index++ {
		runtime.GC()
		duration, updateSize, stateSize, err := runBatch(scenario, payload, iterations)
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
		Scenario:       scenario.description(),
		Runes:          runes,
		SamplesMS:      durations,
		MedianMS:       sorted[len(sorted)/2],
		UpdateBytes:    updateBytes,
		StateBytes:     stateBytes,
		Revision:       revision,
	}, nil
}

func runBatch(scenario comparisonScenario, payload string, iterations int) (time.Duration, int, int, error) {
	started := time.Now()
	updateBytes, stateBytes := 0, 0
	for index := 0; index < iterations; index++ {
		updateSize, stateSize, err := runOnce(scenario, payload)
		if err != nil {
			return 0, 0, 0, err
		}
		updateBytes, stateBytes = updateSize, stateSize
	}
	return time.Since(started), updateBytes, stateBytes, nil
}

func runOnce(scenario comparisonScenario, payload string) (int, int, error) {
	switch scenario {
	case scenarioInitial:
		return runInitialOnce(payload)
	case scenarioOfflineConcurrent:
		return runOfflineConcurrentOnce(payload)
	default:
		return 0, 0, fmt.Errorf("unsupported scenario %q", scenario)
	}
}

func runInitialOnce(payload string) (int, int, error) {
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

func runOfflineConcurrentOnce(payload string) (int, int, error) {
	seed, err := text.New("comparison-seed")
	if err != nil {
		return 0, 0, err
	}
	left, err := text.New("comparison-left")
	if err != nil {
		return 0, 0, err
	}
	right, err := text.New("comparison-right")
	if err != nil {
		return 0, 0, err
	}
	observer, err := text.New("comparison-observer")
	if err != nil {
		return 0, 0, err
	}
	base, err := seed.InsertRunBinaryWithLimits(0, payload, frame.DefaultLimits())
	if err != nil {
		return 0, 0, err
	}
	for _, replica := range []*text.RGA{left, right} {
		if err := applyRunFrame(replica, base); err != nil {
			return 0, 0, err
		}
	}

	offset := len(payload) / 2 // The comparison payload is ASCII, so byte and rune offsets match.
	leftUpdate, err := left.ReplaceRunBinaryWithLimits(offset, 1, "A", frame.DefaultLimits())
	if err != nil {
		return 0, 0, err
	}
	rightUpdate, err := right.ReplaceRunBinaryWithLimits(offset, 1, "B", frame.DefaultLimits())
	if err != nil {
		return 0, 0, err
	}

	for _, delivery := range []struct {
		target *text.RGA
		frame  []byte
	}{
		{right, leftUpdate}, {right, leftUpdate},
		{left, rightUpdate}, {left, rightUpdate},
		{observer, rightUpdate}, {observer, rightUpdate},
		{observer, leftUpdate}, {observer, leftUpdate},
		{observer, base}, {observer, base},
	} {
		if err := applyRunFrame(delivery.target, delivery.frame); err != nil {
			return 0, 0, err
		}
	}
	want := left.String()
	for _, replica := range []*text.RGA{right, observer} {
		if got := replica.String(); got != want || replica.PendingCount() != 0 {
			return 0, 0, fmt.Errorf("replicas did not converge: got %q, want %q, pending=%d", got, want, replica.PendingCount())
		}
	}
	state, err := left.MarshalRunBinary()
	if err != nil {
		return 0, 0, err
	}
	return len(base) + len(leftUpdate) + len(rightUpdate), len(state), nil
}

func applyRunFrame(target *text.RGA, encoded []byte) error {
	delta, err := text.UnmarshalRGARunDelta(encoded)
	if err != nil {
		return err
	}
	return target.ApplyDelta(delta)
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
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, nil, err
	}
	// The CLI's explicit --output value deliberately selects the report path.
	file, err := os.Create(path) // #nosec G304 -- user-selected CLI output path
	if err != nil {
		return nil, nil, err
	}
	return file, file.Close, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
