package set

import (
	"fmt"
	"testing"
)

func BenchmarkGSetMerge(b *testing.B) {
	left, right := benchmarkGSets(b)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := left.Merge(right); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGSetApplyDelta(b *testing.B) {
	target, source := benchmarkGSets(b)
	delta, err := source.Add("delta")
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

func BenchmarkGSetApplyDeltaParallelDuplicate(b *testing.B) {
	target, source := benchmarkGSets(b)
	delta, err := source.Add("delta")
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

func BenchmarkGSetMarshalBinary(b *testing.B) {
	value, _ := benchmarkGSets(b)
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

func BenchmarkGSetScale(b *testing.B) {
	for _, size := range []int{16, 256, 4096} {
		b.Run(fmt.Sprintf("merge-%d", size), func(b *testing.B) {
			left, right := benchmarkGSetsOfSize(b, size)
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if err := left.Merge(right); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("marshal-%d", size), func(b *testing.B) {
			value, _ := benchmarkGSetsOfSize(b, size)
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
		})
	}
}

func benchmarkGSets(b *testing.B) (*GSet[string], *GSet[string]) {
	return benchmarkGSetsOfSize(b, benchmarkORSetElements)
}

func benchmarkGSetsOfSize(b *testing.B, size int) (*GSet[string], *GSet[string]) {
	b.Helper()
	codec := stringCodec{id: "example.com/gset-benchmark/v1"}
	left, err := NewGSet("left", codec)
	if err != nil {
		b.Fatal(err)
	}
	right, err := NewGSet("right", codec)
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < size; index++ {
		element := fmt.Sprintf("element-%03d", index)
		if _, err := left.Add(element); err != nil {
			b.Fatal(err)
		}
		if _, err := right.Add(element); err != nil {
			b.Fatal(err)
		}
	}
	return left, right
}
