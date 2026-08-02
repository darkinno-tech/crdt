package set

import (
	"fmt"
	"testing"
)

const benchmarkORSetElements = 128

func BenchmarkORSetMerge(b *testing.B) {
	left, right := benchmarkORSets(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := left.Merge(right); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkORSetApplyDelta(b *testing.B) {
	target, source := benchmarkORSets(b)
	delta, err := source.Add("delta-element")
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

// BenchmarkORSetApplyDeltaParallelDuplicate measures receiver lock contention
// under concurrent delivery of the same already-observed delta.
func BenchmarkORSetApplyDeltaParallelDuplicate(b *testing.B) {
	target, source := benchmarkORSets(b)
	delta, err := source.Add("delta-element")
	if err != nil {
		b.Fatal(err)
	}
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

func BenchmarkORSetMarshalBinary(b *testing.B) {
	value, _ := benchmarkORSets(b)
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

func BenchmarkORSetUnmarshalBinary(b *testing.B) {
	value, _ := benchmarkORSets(b)
	encoded, err := value.MarshalBinary()
	if err != nil {
		b.Fatal(err)
	}
	codec := stringCodec{id: "example.com/benchmark-string/v1"}
	target := benchmarkNewORSet(b, "benchmark-target", codec)
	b.SetBytes(int64(len(encoded)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := target.UnmarshalBinary(encoded); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkORSetMarshalBinaryTombstoneHeavy(b *testing.B) {
	value, _ := benchmarkORSets(b)
	for element := 0; element < benchmarkORSetElements; element++ {
		if _, err := value.Remove(fmt.Sprintf("element-%03d", element)); err != nil {
			b.Fatal(err)
		}
	}
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

func benchmarkORSets(b *testing.B) (*ORSet[string], *ORSet[string]) {
	b.Helper()
	codec := stringCodec{id: "example.com/benchmark-string/v1"}
	left := benchmarkNewORSet(b, "benchmark-left", codec)
	right := benchmarkNewORSet(b, "benchmark-right", codec)
	for element := 0; element < benchmarkORSetElements; element++ {
		value := fmt.Sprintf("element-%03d", element)
		leftDelta, err := left.Add(value)
		if err != nil {
			b.Fatal(err)
		}
		rightDelta, err := right.Add(value)
		if err != nil {
			b.Fatal(err)
		}
		if err := left.ApplyDelta(rightDelta); err != nil {
			b.Fatal(err)
		}
		if err := right.ApplyDelta(leftDelta); err != nil {
			b.Fatal(err)
		}
	}
	return left, right
}

func benchmarkNewORSet(b *testing.B, replicaID string, codec ElementCodec[string]) *ORSet[string] {
	b.Helper()
	value, err := NewORSet(replicaID, codec)
	if err != nil {
		b.Fatal(err)
	}
	return value
}
