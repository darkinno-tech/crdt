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

func BenchmarkMapApplyDeltaTenKeys(b *testing.B) {
	source, err := NewMap("source")
	if err != nil {
		b.Fatal(err)
	}
	var combined MapDelta
	for index := 0; index < 10; index++ {
		delta, err := source.SetWithDelta("key-"+strconv.Itoa(index), []byte("value"))
		if err != nil {
			b.Fatal(err)
		}
		combined, err = combined.Merge(delta)
		if err != nil {
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
		if err := target.ApplyDelta(combined); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMapApplyDuplicateDelta(b *testing.B) {
	source, err := NewMap("source")
	if err != nil {
		b.Fatal(err)
	}
	change, err := source.SetWithDelta("key", []byte("value"))
	if err != nil {
		b.Fatal(err)
	}
	target, err := NewMap("target")
	if err != nil {
		b.Fatal(err)
	}
	if err := target.ApplyDelta(change); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := target.ApplyDelta(change); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMapMarshalTenThousandKeys(b *testing.B) {
	value, err := NewMap("source")
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 10_000; index++ {
		if err := value.Set("key-"+strconv.Itoa(index), []byte("value")); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := value.MarshalBinary(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSetApplyDeltaTenElements(b *testing.B) {
	source, err := NewSet[string]("source")
	if err != nil {
		b.Fatal(err)
	}
	var combined SetDelta[string]
	for index := 0; index < 10; index++ {
		delta, err := source.AddWithDelta("element-" + strconv.Itoa(index))
		if err != nil {
			b.Fatal(err)
		}
		combined, err = combined.Merge(delta)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		target, err := NewSet[string]("target")
		if err != nil {
			b.Fatal(err)
		}
		if err := target.ApplyDelta(combined); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSetMarshalTenThousandElements(b *testing.B) {
	codec := setStringCodec{id: "example.com/lww-set-benchmark/v1"}
	value, err := NewSet[string]("source")
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 10_000; index++ {
		if err := value.Add("element-" + strconv.Itoa(index)); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := value.MarshalBinary(codec); err != nil {
			b.Fatal(err)
		}
	}
}
