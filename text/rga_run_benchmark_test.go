package text

import "testing"

const benchmarkRGARunDocumentRunes = 100_000

func BenchmarkRGAMarshalLinearDocument(b *testing.B) {
	value := benchmarkRGALinearDocument(b, benchmarkRGARunDocumentRunes)
	benchmarks := []struct {
		name    string
		marshal func() ([]byte, error)
	}{
		{name: "v1", marshal: value.MarshalBinary},
		{name: "run_v2", marshal: value.MarshalRunBinary},
	}
	for _, benchmark := range benchmarks {
		benchmark := benchmark
		b.Run(benchmark.name, func(b *testing.B) {
			encoded, err := benchmark.marshal()
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(encoded)))
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if _, err := benchmark.marshal(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRGASnapshotRunLinearDocument measures the complete persistence
// path used by browser/WebView recovery: bounded state encoding, frontier and
// clock capture, and snapshot validation. The document is prepared outside
// the timer so this remains comparable across protocol implementations.
func BenchmarkRGASnapshotRunLinearDocument(b *testing.B) {
	value := benchmarkRGALinearDocument(b, benchmarkRGARunDocumentRunes)
	first, err := value.SnapshotRunCurrentState()
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(first.Bytes())))
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := value.SnapshotRunCurrentState(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRGAMarshalRunStateTopologies keeps both the local-paste fast path
// and a many-writer sibling shape visible in performance regressions.
func BenchmarkRGAMarshalRunStateTopologies(b *testing.B) {
	for _, workload := range []struct {
		name  string
		value *RGA
	}{
		{name: "linear_100000", value: benchmarkRGALinearDocument(b, benchmarkRGARunDocumentRunes)},
		{name: "branching_4096", value: benchmarkRGARunBranchingDocument(b, 4_096)},
	} {
		b.Run(workload.name, func(b *testing.B) {
			first, err := workload.value.MarshalRunBinary()
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(first)))
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				if _, err := workload.value.MarshalRunBinary(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchmarkRGALinearDocument(b testing.TB, count int) *RGA {
	b.Helper()
	value, err := NewWithOptions("benchmark", Options{
		MaxNodes:        count * 2,
		MaxTombstones:   count * 2,
		MaxPendingNodes: count,
		MaxPendingBytes: 32 << 20,
	})
	if err != nil {
		b.Fatal(err)
	}
	if err := value.ApplyDelta(benchmarkLinearRGADelta(b, count)); err != nil {
		b.Fatal(err)
	}
	return value
}

func benchmarkRGARunBranchingDocument(b testing.TB, count int) *RGA {
	b.Helper()
	value, err := NewWithOptions("branching", Options{
		MaxNodes:        count * 2,
		MaxTombstones:   count * 2,
		MaxPendingNodes: count,
		MaxPendingBytes: 32 << 20,
	})
	if err != nil {
		b.Fatal(err)
	}
	nodes := make(map[Position]node, count)
	for index := 0; index < count; index++ {
		id := Position{ReplicaID: "branching-source", WallTime: uint64(index + 1)}
		nodes[id] = node{rune: rune('a' + index%26)}
	}
	if err := value.ApplyDelta(Delta{nodes: nodes, tombstones: make(map[Position]struct{})}); err != nil {
		b.Fatal(err)
	}
	return value
}
