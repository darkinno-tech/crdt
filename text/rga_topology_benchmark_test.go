package text

import (
	"fmt"
	"strings"
	"testing"
)

const benchmarkRGALongTextRunes = 100_000

// BenchmarkRGAInsertLongText measures the local editor path for a full pasted
// document. Its timer includes tag allocation, resolved-batch planning, and
// index construction, but excludes constructing a fresh replica. Go's
// allocation report intentionally still includes each iteration's full budget.
func BenchmarkRGAInsertLongText(b *testing.B) {
	value := strings.Repeat("a", benchmarkRGALongTextRunes)
	b.SetBytes(int64(len(value)))
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		b.StopTimer()
		target, err := NewWithOptions("writer", benchmarkRGAOptions(benchmarkRGALongTextRunes))
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := target.Insert(0, value); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRGAApplyDeltaTopologies measures an empty replica applying a
// complete delta. The linear case represents one editor's pasted run; the
// wide case represents many concurrent inserts at one document position.
func BenchmarkRGAApplyDeltaTopologies(b *testing.B) {
	for _, workload := range []struct {
		name  string
		nodes int
		delta Delta
	}{
		{name: "linear_15_generic", nodes: 15, delta: benchmarkLinearRGADelta(b, 15)},
		{name: "linear_16_fast_path", nodes: 16, delta: benchmarkLinearRGADelta(b, 16)},
		{name: "linear_100000_fast_path", nodes: 100_000, delta: benchmarkLinearRGADelta(b, 100_000)},
		{name: "wide_4096", nodes: 4_096, delta: benchmarkWideRGADelta(4_096)},
	} {
		b.Run(workload.name, func(b *testing.B) {
			encoded, err := workload.delta.MarshalBinary()
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(encoded)))
			b.ReportAllocs()
			for iteration := 0; iteration < b.N; iteration++ {
				b.StopTimer()
				target, err := NewWithOptions("target", benchmarkRGAOptions(workload.nodes))
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if err := target.ApplyDelta(workload.delta); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRGAApplyDuplicateDeltaParallel measures the common relay retry
// path under concurrent readers. RunParallel is intentionally used here;
// changing GOMAXPROCS alone would not create shared-document contention.
func BenchmarkRGAApplyDuplicateDeltaParallel(b *testing.B) {
	source := mustBenchmarkRGA(b, "source")
	delta, err := source.Insert(0, "duplicate delivery")
	if err != nil {
		b.Fatal(err)
	}
	target := mustBenchmarkRGA(b, "target")
	if err := target.ApplyDelta(delta); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(parallel *testing.PB) {
		for parallel.Next() {
			if err := target.ApplyDelta(delta); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func benchmarkWideRGADelta(count int) Delta {
	nodes := make(map[Position]node, count)
	for index := 1; index <= count; index++ {
		id := Position{ReplicaID: "wide-source", WallTime: uint64(index)}
		nodes[id] = node{rune: rune('a' + index%26)}
	}
	return Delta{nodes: nodes, tombstones: make(map[Position]struct{})}
}

func benchmarkRGAOptions(nodes int) Options {
	return Options{
		MaxNodes:        nodes * 2,
		MaxTombstones:   nodes * 2,
		MaxPendingNodes: nodes,
		MaxPendingBytes: nodes * 128,
	}
}

func mustBenchmarkRGA(b testing.TB, replicaID string) *RGA {
	b.Helper()
	value, err := New(replicaID)
	if err != nil {
		b.Fatal(fmt.Errorf("create RGA: %w", err))
	}
	return value
}
