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

func BenchmarkRGAApplyDeltaLinearChain(b *testing.B) {
	delta := benchmarkLinearRGADelta(b, 100_000)
	b.SetBytes(100_000)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		target, err := NewWithOptions("target", Options{
			MaxNodes:        200_000,
			MaxTombstones:   200_000,
			MaxPendingNodes: 100_000,
			MaxPendingBytes: 16 << 20,
		})
		if err != nil {
			b.Fatal(err)
		}
		if err := target.ApplyDelta(delta); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRGAAppendToIndexedDocument(b *testing.B) {
	options := DefaultOptions()
	options.MaxNodes = 16 << 20
	value, err := NewWithOptions("writer", options)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := value.Insert(0, strings.Repeat("a", 10_000)); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := value.Insert(value.State().ElementCount, "a"); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkLinearRGADelta(b testing.TB, count int) Delta {
	b.Helper()
	nodes := make(map[Position]node, count)
	parent := Position{}
	for index := 1; index <= count; index++ {
		id := Position{ReplicaID: "source", WallTime: uint64(index)}
		nodes[id] = node{parent: parent, rune: 'a'}
		parent = id
	}
	return Delta{nodes: nodes, tombstones: make(map[Position]struct{})}
}
