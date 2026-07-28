package lww

import (
	"strconv"
	"testing"
)

func BenchmarkMapMergeTenThousandKeys(b *testing.B) {
	source, err := NewMap("source")
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 10_000; index++ {
		if err := source.Set("key-"+strconv.Itoa(index), []byte("value")); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		target, err := NewMap("target")
		if err != nil {
			b.Fatal(err)
		}
		if err := target.Merge(source); err != nil {
			b.Fatal(err)
		}
	}
}
