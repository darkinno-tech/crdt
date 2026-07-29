package lww

import (
	"strconv"
	"testing"

	"github.com/DarkInno/crdt"
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

// BenchmarkMapCreateAndCompactTombstones1024 measures the complete LWW map
// tombstone lifecycle rather than hiding creation outside the timed region.
func BenchmarkMapCreateAndCompactTombstones1024(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		value, tags := benchmarkMapTombstones(b, 1024)
		removed, err := value.CompactTombstones(tags)
		if err != nil || removed != len(tags) {
			b.Fatalf("CompactTombstones() = %d, %v; want %d, nil", removed, err, len(tags))
		}
	}
}

// BenchmarkSetCreateAndCompactTombstones1024 measures the complete LWW set
// tombstone lifecycle with the same retained-entry count as the map benchmark.
func BenchmarkSetCreateAndCompactTombstones1024(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		value, tags := benchmarkSetTombstones(b, 1024)
		removed, err := value.CompactTombstones(tags)
		if err != nil || removed != len(tags) {
			b.Fatalf("CompactTombstones() = %d, %v; want %d, nil", removed, err, len(tags))
		}
	}
}

func benchmarkMapTombstones(b testing.TB, count int) (*Map, []crdt.Tag) {
	b.Helper()
	value, err := NewMap("benchmark")
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < count; index++ {
		key := "key-" + strconv.Itoa(index)
		if err := value.Set(key, []byte("value")); err != nil {
			b.Fatal(err)
		}
		if err := value.Delete(key); err != nil {
			b.Fatal(err)
		}
	}
	return value, value.TombstoneTags()
}

func benchmarkSetTombstones(b testing.TB, count int) (*Set[string], []crdt.Tag) {
	b.Helper()
	value, err := NewSet[string]("benchmark")
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < count; index++ {
		element := "element-" + strconv.Itoa(index)
		if err := value.Add(element); err != nil {
			b.Fatal(err)
		}
		if err := value.Remove(element); err != nil {
			b.Fatal(err)
		}
	}
	return value, value.TombstoneTags()
}
