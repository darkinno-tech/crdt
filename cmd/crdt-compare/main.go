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
	Protocol       string    `json:"protocol"`
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
type comparisonProtocol string

const (
	scenarioInitial           comparisonScenario = "initial"
	scenarioOfflineConcurrent comparisonScenario = "offline-concurrent"
	protocolRunV2             comparisonProtocol = "run-v2"
	protocolPackedV3          comparisonProtocol = "packed-v3"
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

func parseProtocol(value string) (comparisonProtocol, error) {
	protocol := comparisonProtocol(value)
	switch protocol {
	case protocolRunV2, protocolPackedV3:
		return protocol, nil
	default:
		return "", fmt.Errorf("%q is not a supported protocol", value)
	}
}

func (protocol comparisonProtocol) implementation() string {
	switch protocol {
	case protocolRunV2:
		return "DarkInno RGA run-v2"
	case protocolPackedV3:
		return "DarkInno RGA packed-v3"
	default:
		return "unknown"
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
		protocolText = flags.String("protocol", string(protocolRunV2), "RGA protocol: run-v2 or packed-v3")
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
	protocol, err := parseProtocol(*protocolText)
	if err != nil {
		return err
	}
	sizes, err := parseSizes(*sizesText)
	if err != nil {
		return fmt.Errorf("parse sizes: %w", err)
	}
	reports := make([]report, 0, len(sizes))
	for _, size := range sizes {
		item, err := measureProtocol(scenario, protocol, size, *samples, *warmups, *iterations, *revision)
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
	return measureProtocol(scenario, protocolRunV2, runes, samples, warmups, iterations, revision)
}

func measureProtocol(scenario comparisonScenario, protocol comparisonProtocol, runes, samples, warmups, iterations int, revision string) (report, error) {
	payload := strings.Repeat("x", runes)
	for index := 0; index < warmups; index++ {
		if _, _, _, err := runBatchProtocol(scenario, protocol, payload, iterations); err != nil {
			return report{}, err
		}
	}
	durations := make([]float64, 0, samples)
	updateBytes, stateBytes := 0, 0
	for index := 0; index < samples; index++ {
		runtime.GC()
		duration, updateSize, stateSize, err := runBatchProtocol(scenario, protocol, payload, iterations)
		if err != nil {
			return report{}, err
		}
		durations = append(durations, float64(duration)/float64(time.Millisecond*time.Duration(iterations)))
		updateBytes, stateBytes = updateSize, stateSize
	}
	sorted := append([]float64(nil), durations...)
	sort.Float64s(sorted)
	return report{
		Implementation: protocol.implementation(),
		Protocol:       string(protocol),
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
	return runBatchProtocol(scenario, protocolRunV2, payload, iterations)
}

func runBatchProtocol(scenario comparisonScenario, protocol comparisonProtocol, payload string, iterations int) (time.Duration, int, int, error) {
	started := time.Now()
	updateBytes, stateBytes := 0, 0
	for index := 0; index < iterations; index++ {
		updateSize, stateSize, err := runOnceProtocol(scenario, protocol, payload)
		if err != nil {
			return 0, 0, 0, err
		}
		updateBytes, stateBytes = updateSize, stateSize
	}
	return time.Since(started), updateBytes, stateBytes, nil
}

func runOnceProtocol(scenario comparisonScenario, protocol comparisonProtocol, payload string) (int, int, error) {
	switch scenario {
	case scenarioInitial:
		return runInitialOnceProtocol(protocol, payload)
	case scenarioOfflineConcurrent:
		return runOfflineConcurrentOnceProtocol(protocol, payload)
	default:
		return 0, 0, fmt.Errorf("unsupported scenario %q", scenario)
	}
}

func runInitialOnceProtocol(protocol comparisonProtocol, payload string) (int, int, error) {
	source, err := text.New("comparison-source")
	if err != nil {
		return 0, 0, err
	}
	target, err := text.New("comparison-target")
	if err != nil {
		return 0, 0, err
	}
	encoded, err := insertComparisonFrame(source, protocol, 0, payload)
	if err != nil {
		return 0, 0, err
	}
	if err := applyComparisonFrame(target, protocol, encoded); err != nil {
		return 0, 0, err
	}
	if target.String() != payload {
		return 0, 0, fmt.Errorf("target did not converge")
	}
	state, err := marshalComparisonState(source, protocol)
	if err != nil {
		return 0, 0, err
	}
	return len(encoded), len(state), nil
}

func runOfflineConcurrentOnceProtocol(protocol comparisonProtocol, payload string) (int, int, error) {
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
	base, err := insertComparisonFrame(seed, protocol, 0, payload)
	if err != nil {
		return 0, 0, err
	}
	for _, replica := range []*text.RGA{left, right} {
		if err := applyComparisonFrame(replica, protocol, base); err != nil {
			return 0, 0, err
		}
	}

	offset := len(payload) / 2 // The comparison payload is ASCII, so byte and rune offsets match.
	leftUpdate, err := replaceComparisonFrame(left, protocol, offset, 1, "A")
	if err != nil {
		return 0, 0, err
	}
	rightUpdate, err := replaceComparisonFrame(right, protocol, offset, 1, "B")
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
		if err := applyComparisonFrame(delivery.target, protocol, delivery.frame); err != nil {
			return 0, 0, err
		}
	}
	want := left.String()
	for _, replica := range []*text.RGA{right, observer} {
		if got := replica.String(); got != want || replica.PendingCount() != 0 {
			return 0, 0, fmt.Errorf("replicas did not converge: got %q, want %q, pending=%d", got, want, replica.PendingCount())
		}
	}
	state, err := marshalComparisonState(left, protocol)
	if err != nil {
		return 0, 0, err
	}
	return len(base) + len(leftUpdate) + len(rightUpdate), len(state), nil
}

func insertComparisonFrame(target *text.RGA, protocol comparisonProtocol, offset int, value string) ([]byte, error) {
	switch protocol {
	case protocolRunV2:
		return target.InsertRunBinaryWithLimits(offset, value, frame.DefaultLimits())
	case protocolPackedV3:
		return target.InsertPackedBinaryWithLimits(offset, value, frame.DefaultLimits())
	default:
		return nil, fmt.Errorf("unsupported protocol %q", protocol)
	}
}

func replaceComparisonFrame(target *text.RGA, protocol comparisonProtocol, offset, count int, value string) ([]byte, error) {
	switch protocol {
	case protocolRunV2:
		return target.ReplaceRunBinaryWithLimits(offset, count, value, frame.DefaultLimits())
	case protocolPackedV3:
		return target.ReplacePackedBinaryWithLimits(offset, count, value, frame.DefaultLimits())
	default:
		return nil, fmt.Errorf("unsupported protocol %q", protocol)
	}
}

func marshalComparisonState(target *text.RGA, protocol comparisonProtocol) ([]byte, error) {
	switch protocol {
	case protocolRunV2:
		return target.MarshalRunBinary()
	case protocolPackedV3:
		return target.MarshalPackedBinary()
	default:
		return nil, fmt.Errorf("unsupported protocol %q", protocol)
	}
}

func applyComparisonFrame(target *text.RGA, protocol comparisonProtocol, encoded []byte) error {
	var (
		delta text.Delta
		err   error
	)
	switch protocol {
	case protocolRunV2:
		delta, err = text.UnmarshalRGARunDelta(encoded)
	case protocolPackedV3:
		delta, err = text.UnmarshalRGAPackedDelta(encoded)
	default:
		return fmt.Errorf("unsupported protocol %q", protocol)
	}
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
