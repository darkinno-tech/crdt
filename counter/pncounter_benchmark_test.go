package counter

import (
	"fmt"
	"testing"
)

func BenchmarkPNCounterMerge(b *testing.B) {
	left, right := benchmarkPNCounters(b)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := left.Merge(right); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPNCounterApplyDelta(b *testing.B) {
	target, source := benchmarkPNCounters(b)
	delta, err := source.Increment(1)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := target.ApplyDelta(delta); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPNCounterValue(b *testing.B) {
	value, _ := benchmarkPNCounters(b)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := value.Value(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPNCounterMarshalBinary(b *testing.B) {
	value, _ := benchmarkPNCounters(b)
	encoded, err := value.MarshalBinary()
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(encoded)))
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := value.MarshalBinary(); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkPNCounters(b *testing.B) (*PNCounter, *PNCounter) {
	b.Helper()
	left := benchmarkNewPNCounter(b, "benchmark-left")
	right := benchmarkNewPNCounter(b, "benchmark-right")
	for component := 0; component < benchmarkCounterComponents; component++ {
		source := benchmarkNewPNCounter(b, fmt.Sprintf("benchmark-%03d", component))
		increment, err := source.Increment(uint64(component + 1))
		if err != nil {
			b.Fatal(err)
		}
		decrement, err := source.Decrement(uint64(component))
		if err != nil {
			b.Fatal(err)
		}
		for _, target := range []*PNCounter{left, right} {
			if err := target.ApplyDelta(increment); err != nil {
				b.Fatal(err)
			}
			if err := target.ApplyDelta(decrement); err != nil {
				b.Fatal(err)
			}
		}
	}
	return left, right
}

func benchmarkNewPNCounter(b *testing.B, replicaID string) *PNCounter {
	b.Helper()
	value, err := NewPNCounter(replicaID)
	if err != nil {
		b.Fatal(err)
	}
	return value
}
