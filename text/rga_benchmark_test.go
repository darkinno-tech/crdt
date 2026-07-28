package text

import (
	"strings"
	"testing"
)

func BenchmarkRGAStringLinearDocument(b *testing.B) {
	value, err := New("writer")
	if err != nil {
		b.Fatal(err)
	}
	if _, err := value.Insert(0, strings.Repeat("a", 10_000)); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if got := value.String(); len(got) != 10_000 {
			b.Fatalf("len(String()) = %d", len(got))
		}
	}
}

func BenchmarkRGAMergeTombstoneDelta(b *testing.B) {
	source, err := New("source")
	if err != nil {
		b.Fatal(err)
	}
	if _, err := source.Insert(0, strings.Repeat("a", 1_000)); err != nil {
		b.Fatal(err)
	}
	delta, err := source.Delete(0, 1_000)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		target, err := New("target")
		if err != nil {
			b.Fatal(err)
		}
		if err := target.ApplyDelta(delta); err != nil {
			b.Fatal(err)
		}
	}
}
