package counter

import (
	"fmt"
	"testing"
)

const benchmarkCounterComponents = 128

func BenchmarkGCounterMerge(b *testing.B) {
	left, right := benchmarkCounters(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := left.Merge(right); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGCounterApplyDelta(b *testing.B) {
	target, source := benchmarkCounters(b)
	delta, err := source.Increment(1)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := target.ApplyDelta(delta); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGCounterMarshalBinary(b *testing.B) {
	value, _ := benchmarkCounters(b)
	encoded, err := value.MarshalBinary()
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(encoded)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := value.MarshalBinary(); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkCounters(b *testing.B) (*GCounter, *GCounter) {
	b.Helper()
	left := benchmarkNewGCounter(b, "benchmark-left")
	right := benchmarkNewGCounter(b, "benchmark-right")
	for component := 0; component < benchmarkCounterComponents; component++ {
		source := benchmarkNewGCounter(b, fmt.Sprintf("benchmark-%03d", component))
		delta, err := source.Increment(uint64(component + 1))
		if err != nil {
			b.Fatal(err)
		}
		if err := left.ApplyDelta(delta); err != nil {
			b.Fatal(err)
		}
		if err := right.ApplyDelta(delta); err != nil {
			b.Fatal(err)
		}
	}
	return left, right
}

func benchmarkNewGCounter(b *testing.B, replicaID string) *GCounter {
	b.Helper()
	value, err := NewGCounter(replicaID)
	if err != nil {
		b.Fatal(err)
	}
	return value
}
