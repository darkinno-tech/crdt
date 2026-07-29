package text

import (
	"runtime"
	"strings"
	"testing"
)

const (
	benchmarkRGAExtremeLinearNodes   = 200_000
	benchmarkRGAExtremeWideNodes     = 8_192
	benchmarkRGAExtremeResidentNodes = 2 << 20
)

// BenchmarkRGAExtremeInsert200K doubles the 100K pasted-document workload.
// It measures the complete local insert path, excluding the construction of a
// fresh replica from the timed region.
func BenchmarkRGAExtremeInsert200K(b *testing.B) {
	value := strings.Repeat("a", benchmarkRGAExtremeLinearNodes)
	b.SetBytes(int64(len(value)))
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		b.StopTimer()
		target, err := NewWithOptions("writer", benchmarkRGAOptions(benchmarkRGAExtremeLinearNodes))
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := target.Insert(0, value); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRGAExtremeApplyDelta doubles the linear and concurrent-sibling
// receiver workloads. Its timer includes only applying a complete delta to a
// new replica, not preparing the source delta or encoding it.
func BenchmarkRGAExtremeApplyDelta(b *testing.B) {
	for _, workload := range []struct {
		name  string
		nodes int
		delta Delta
	}{
		{name: "linear_200000_fast_path", nodes: benchmarkRGAExtremeLinearNodes, delta: benchmarkLinearRGADelta(b, benchmarkRGAExtremeLinearNodes)},
		{name: "wide_8192", nodes: benchmarkRGAExtremeWideNodes, delta: benchmarkWideRGADelta(benchmarkRGAExtremeWideNodes)},
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

// BenchmarkRGAExtremeResidentLinearDocument2M doubles the default document
// boundary. It intentionally raises limits only on this benchmark instance;
// DefaultOptions still rejects a document larger than its 1M node cap. Run
// with -benchtime=1x because retained heap is a regression signal rather than
// a stable cross-machine memory contract.
func BenchmarkRGAExtremeResidentLinearDocument2M(b *testing.B) {
	const count = benchmarkRGAExtremeResidentNodes
	delta := benchmarkLinearRGADelta(b, count)
	options := benchmarkRGAOptions(count)
	b.SetBytes(count)
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		b.StopTimer()
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		b.StartTimer()
		target, err := NewWithOptions("target", options)
		if err != nil {
			b.Fatal(err)
		}
		if err := target.ApplyDelta(delta); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		if after.HeapAlloc < before.HeapAlloc {
			b.Fatalf("retained heap regressed below baseline: before=%d after=%d", before.HeapAlloc, after.HeapAlloc)
		}
		b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/count, "heap-B/char")
		runtime.KeepAlive(target)
		runtime.KeepAlive(delta)
		b.StartTimer()
	}
}
