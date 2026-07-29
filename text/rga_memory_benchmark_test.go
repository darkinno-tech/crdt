package text

import (
	"runtime"
	"testing"
)

// BenchmarkRGAResidentLinearDocument reports the receiver's retained heap per
// character after applying a document at the default MaxNodes boundary. Run
// it with -benchtime=1x: the heap metric is a regression signal, not a stable
// cross-machine memory contract.
func BenchmarkRGAResidentLinearDocument(b *testing.B) {
	const count = 1 << 20
	delta := benchmarkLinearRGADelta(b, count)
	options := DefaultOptions()
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
