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
