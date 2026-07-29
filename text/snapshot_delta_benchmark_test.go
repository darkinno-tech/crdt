package text

import (
	"strings"
	"testing"
)

func BenchmarkRGAMarshalDeltaSinceSnapshot(b *testing.B) {
	value := benchmarkRGALinearDocument(b, 20_000)
	base, err := value.SnapshotRunCurrentState()
	if err != nil {
		b.Fatal(err)
	}
	if _, err := value.Insert(value.State().ElementCount, strings.Repeat("b", 256)); err != nil {
		b.Fatal(err)
	}
	encoded, err := value.MarshalDeltaSince(base)
	if err != nil {
		b.Fatal(err)
	}
	cachedBase, err := NewSnapshotBase(base)
	if err != nil {
		b.Fatal(err)
	}
	benchmarks := []struct {
		name    string
		marshal func() ([]byte, error)
	}{
		{name: "snapshot", marshal: func() ([]byte, error) { return value.MarshalDeltaSince(base) }},
		{name: "cached_base", marshal: func() ([]byte, error) { return value.MarshalDeltaSinceBase(cachedBase) }},
	}
	for _, benchmark := range benchmarks {
		benchmark := benchmark
		b.Run(benchmark.name, func(b *testing.B) {
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
