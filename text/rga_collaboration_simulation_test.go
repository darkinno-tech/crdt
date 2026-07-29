package text

import (
	"bytes"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

const (
	benchmarkRGASharedDocumentEditors = 10_000
	benchmarkRGAManyDocuments         = 20_000
	simulationBaseRunes               = 32
)

type rgaSimulationResult struct {
	documents      int
	editEvents     int
	deliveryEvents int
	visibleRunes   int
}

// TestRGACollaborationSimulationSmoke keeps the concurrency, duplicate, and
// tombstone-before-insert invariants in the ordinary test suite. The benchmark
// cases below scale the same workloads to 10K editors and 20K documents.
func TestRGACollaborationSimulationSmoke(t *testing.T) {
	shared, err := simulateSharedDocumentEdits(128, 3, rgaSimulationWorkers(128))
	if err != nil {
		t.Fatal(err)
	}
	if shared.documents != 1 || shared.editEvents != 128 || shared.visibleRunes == 0 {
		t.Fatalf("shared simulation = %+v", shared)
	}

	many, err := simulateManyDocumentEdits(256, 2, rgaSimulationWorkers(256))
	if err != nil {
		t.Fatal(err)
	}
	if many.documents != 256 || many.editEvents != 512 || many.visibleRunes == 0 {
		t.Fatalf("many-document simulation = %+v", many)
	}
}

// BenchmarkRGASimulationTenThousandEditorsOneDocument models 10K stale
// editors modifying one 32-rune document. Half insert at independently chosen
// offsets and half cut a base character. Every delta is delivered three times
// in a shuffled, contended order to two receivers, whose canonical states must
// converge. Run with -benchtime=1x.
func BenchmarkRGASimulationTenThousandEditorsOneDocument(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	var result rgaSimulationResult
	for iteration := 0; iteration < b.N; iteration++ {
		var err error
		result, err = simulateSharedDocumentEdits(
			benchmarkRGASharedDocumentEditors,
			3,
			rgaSimulationWorkers(benchmarkRGASharedDocumentEditors),
		)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(result.editEvents), "edits/op")
	b.ReportMetric(float64(result.deliveryEvents), "deliveries/op")
	b.ReportMetric(float64(result.visibleRunes), "visible-runes/op")
}

// BenchmarkRGASimulationTwentyThousandDocumentsEditing models 20K isolated
// documents being edited concurrently. Each virtual editor inserts two runes,
// cuts one immediately, and the receiver observes the cut before the insert
// with duplicate delivery. This exercises independent-document throughput and
// tombstone-before-node recovery without inventing a shared global lock. Run
// with -benchtime=1x.
func BenchmarkRGASimulationTwentyThousandDocumentsEditing(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	var result rgaSimulationResult
	for iteration := 0; iteration < b.N; iteration++ {
		var err error
		result, err = simulateManyDocumentEdits(
			benchmarkRGAManyDocuments,
			2,
			rgaSimulationWorkers(benchmarkRGAManyDocuments),
		)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(result.documents), "documents/op")
	b.ReportMetric(float64(result.editEvents), "edits/op")
	b.ReportMetric(float64(result.deliveryEvents), "deliveries/op")
	b.ReportMetric(float64(result.visibleRunes), "visible-runes/op")
}

func simulateSharedDocumentEdits(editors, duplicates, workers int) (rgaSimulationResult, error) {
	if editors <= 0 || duplicates <= 0 {
		return rgaSimulationResult{}, fmt.Errorf("editors and duplicates must be positive")
	}
	options := rgaSimulationOptions(simulationBaseRunes + editors)
	base, err := NewWithOptions("shared-seed", options)
	if err != nil {
		return rgaSimulationResult{}, err
	}
	baseDelta, err := base.Insert(0, strings.Repeat("b", simulationBaseRunes))
	if err != nil {
		return rgaSimulationResult{}, err
	}

	deltas := make([]Delta, editors)
	if err := runSimulationWorkers(editors, workers, func(index int) error {
		source, err := NewWithOptions(fmt.Sprintf("shared-editor-%05d", index), options)
		if err != nil {
			return err
		}
		if err := source.ApplyDelta(baseDelta); err != nil {
			return err
		}
		if index%2 == 0 {
			deltas[index], err = source.Insert((index*7)%(simulationBaseRunes+1), string(rune('a'+index%26)))
		} else {
			deltas[index], err = source.Delete((index*11)%simulationBaseRunes, 1)
		}
		return err
	}); err != nil {
		return rgaSimulationResult{}, fmt.Errorf("produce shared-document edits: %w", err)
	}

	left, err := NewWithOptions("shared-left", options)
	if err != nil {
		return rgaSimulationResult{}, err
	}
	right, err := NewWithOptions("shared-right", options)
	if err != nil {
		return rgaSimulationResult{}, err
	}
	if err := left.ApplyDelta(baseDelta); err != nil {
		return rgaSimulationResult{}, err
	}
	if err := right.ApplyDelta(baseDelta); err != nil {
		return rgaSimulationResult{}, err
	}

	leftOrder := shuffledSimulationIndexes(editors, 2026072901)
	rightOrder := shuffledSimulationIndexes(editors, 2026072902)
	if err := deliverSimulationDeltas(left, deltas, leftOrder, duplicates, workers); err != nil {
		return rgaSimulationResult{}, fmt.Errorf("deliver shared-document edits to left: %w", err)
	}
	if err := deliverSimulationDeltas(right, deltas, rightOrder, duplicates, workers); err != nil {
		return rgaSimulationResult{}, fmt.Errorf("deliver shared-document edits to right: %w", err)
	}
	if left.PendingCount() != 0 || right.PendingCount() != 0 {
		return rgaSimulationResult{}, fmt.Errorf("shared-document pending nodes: left=%d right=%d", left.PendingCount(), right.PendingCount())
	}
	if err := assertSimulationConverged(left, right); err != nil {
		return rgaSimulationResult{}, fmt.Errorf("shared-document convergence: %w", err)
	}

	return rgaSimulationResult{
		documents:      1,
		editEvents:     editors,
		deliveryEvents: editors * duplicates * 2,
		visibleRunes:   utf8.RuneCountInString(left.String()),
	}, nil
}

func simulateManyDocumentEdits(documents, duplicates, workers int) (rgaSimulationResult, error) {
	if documents <= 0 || duplicates <= 0 {
		return rgaSimulationResult{}, fmt.Errorf("documents and duplicates must be positive")
	}
	var visibleRunes int
	var visibleMu sync.Mutex
	if err := runSimulationWorkers(documents, workers, func(index int) error {
		options := rgaSimulationOptions(simulationBaseRunes + 2)
		seed, err := NewWithOptions(fmt.Sprintf("document-%05d-seed", index), options)
		if err != nil {
			return err
		}
		base, err := seed.Insert(0, strings.Repeat("b", simulationBaseRunes))
		if err != nil {
			return err
		}

		source, err := NewWithOptions(fmt.Sprintf("document-%05d-editor", index), options)
		if err != nil {
			return err
		}
		if err := source.ApplyDelta(base); err != nil {
			return err
		}
		offset := (index * 5) % (simulationBaseRunes + 1)
		insert, err := source.Insert(offset, "xy")
		if err != nil {
			return err
		}
		cut, err := source.Delete(offset, 1)
		if err != nil {
			return err
		}

		target, err := NewWithOptions(fmt.Sprintf("document-%05d-receiver", index), options)
		if err != nil {
			return err
		}
		if err := target.ApplyDelta(base); err != nil {
			return err
		}
		for delivery := 0; delivery < duplicates; delivery++ {
			if err := target.ApplyDelta(cut); err != nil {
				return err
			}
		}
		for delivery := 0; delivery < duplicates; delivery++ {
			if err := target.ApplyDelta(insert); err != nil {
				return err
			}
		}
		if target.PendingCount() != 0 {
			return fmt.Errorf("document %d has %d pending nodes", index, target.PendingCount())
		}
		if err := assertSimulationConverged(source, target); err != nil {
			return fmt.Errorf("document %d: %w", index, err)
		}
		visibleMu.Lock()
		visibleRunes += utf8.RuneCountInString(target.String())
		visibleMu.Unlock()
		return nil
	}); err != nil {
		return rgaSimulationResult{}, fmt.Errorf("simulate independent documents: %w", err)
	}

	return rgaSimulationResult{
		documents:      documents,
		editEvents:     documents * 2,
		deliveryEvents: documents * (1 + 2*duplicates),
		visibleRunes:   visibleRunes,
	}, nil
}

func deliverSimulationDeltas(target *RGA, deltas []Delta, order []int, duplicates, workers int) error {
	return runSimulationWorkers(len(order), workers, func(position int) error {
		delta := deltas[order[position]]
		for delivery := 0; delivery < duplicates; delivery++ {
			if err := target.ApplyDelta(delta); err != nil {
				return err
			}
		}
		return nil
	})
}

func assertSimulationConverged(left, right *RGA) error {
	if got, want := left.String(), right.String(); got != want {
		return fmt.Errorf("visible strings differ: left=%d runes right=%d runes", utf8.RuneCountInString(got), utf8.RuneCountInString(want))
	}
	leftState, err := left.MarshalBinary()
	if err != nil {
		return err
	}
	rightState, err := right.MarshalBinary()
	if err != nil {
		return err
	}
	if !bytes.Equal(leftState, rightState) {
		return fmt.Errorf("canonical states differ")
	}
	return nil
}

func rgaSimulationOptions(nodes int) Options {
	limit := nodes * 2
	return Options{
		MaxNodes:        limit,
		MaxTombstones:   limit,
		MaxPendingNodes: nodes,
		MaxPendingBytes: nodes * 128,
	}
}

func rgaSimulationWorkers(tasks int) int {
	workers := runtime.GOMAXPROCS(0) * 4
	if workers < 4 {
		workers = 4
	}
	if workers > tasks {
		workers = tasks
	}
	return workers
}

func shuffledSimulationIndexes(count int, seed int64) []int {
	indexes := make([]int, count)
	for index := range indexes {
		indexes[index] = index
	}
	random := rand.New(rand.NewSource(seed))
	random.Shuffle(len(indexes), func(left, right int) {
		indexes[left], indexes[right] = indexes[right], indexes[left]
	})
	return indexes
}

func runSimulationWorkers(tasks, workers int, task func(int) error) error {
	if tasks <= 0 || workers <= 0 {
		return fmt.Errorf("tasks and workers must be positive")
	}
	jobs := make(chan int)
	var group sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				if err := task(index); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
				}
			}
		}()
	}
	for index := 0; index < tasks; index++ {
		jobs <- index
	}
	close(jobs)
	group.Wait()
	return firstErr
}
