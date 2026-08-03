package text

import (
	"strconv"
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

func BenchmarkRGAApplyDuplicateDelta(b *testing.B) {
	source, err := New("source")
	if err != nil {
		b.Fatal(err)
	}
	delta, err := source.Insert(0, "duplicate delivery")
	if err != nil {
		b.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		b.Fatal(err)
	}
	if err := target.ApplyDelta(delta); err != nil {
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

// BenchmarkUndoManagerInsertUndoDiscardRedo models the common editor branch:
// insert a local character, undo it, then make another local edit that drops
// redo. The bounded manager must release obsolete position ownership while
// keeping the current local change undoable. Resetting the document outside
// the timed section prevents its retained CRDT tombstones from dominating this
// local-history measurement.
func BenchmarkUndoManagerInsertUndoDiscardRedo(b *testing.B) {
	const resetEvery = 4096
	var manager *UndoManager
	reset := func() {
		value, err := New("writer")
		if err != nil {
			b.Fatal(err)
		}
		manager, err = NewUndoManagerWithOptions(value, UndoOptions{MaxEntries: 8, MaxRunes: 64})
		if err != nil {
			b.Fatal(err)
		}
	}
	reset()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if index > 0 && index%resetEvery == 0 {
			b.StopTimer()
			reset()
			b.StartTimer()
		}
		if _, err := manager.Insert(0, "x"); err != nil {
			b.Fatal(err)
		}
		if _, err := manager.Undo(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRGACompactEligibleTombstones models completed document deletions.
// Setup time is stopped so ns/op isolates structural collection after an
// external coordinator has already proved exact acknowledgement. Go's
// benchmark allocation accounting still includes construction of fresh mutable
// state for each iteration.
func BenchmarkRGACompactEligibleTombstones(b *testing.B) {
	for _, workload := range []struct {
		name  string
		build func(testing.TB, int) (*RGA, []Position)
	}{
		{name: "chain", build: benchmarkTombstonedRGAChain},
		{name: "wide", build: benchmarkTombstonedRGAWide},
	} {
		for _, tombstoneCount := range []int{128, 1024} {
			b.Run(workload.name+"_"+strconv.Itoa(tombstoneCount), func(b *testing.B) {
				b.ReportAllocs()
				for index := 0; index < b.N; index++ {
					b.StopTimer()
					value, tags := workload.build(b, tombstoneCount)
					b.StartTimer()
					removed, err := value.CompactEligibleTombstones(tags)
					if err != nil || removed != tombstoneCount {
						b.Fatalf("CompactEligibleTombstones() = %d, %v; want %d, nil", removed, err, tombstoneCount)
					}
					b.StopTimer()
				}
			})
		}
	}
}

// BenchmarkRGACompactTombstonesOneLeafAtATime is the conservative fallback
// applications previously needed for a fully deleted chain or wide sibling
// set: submit one known leaf at a time, from the tail of the tag order.
func BenchmarkRGACompactTombstonesOneLeafAtATime(b *testing.B) {
	for _, workload := range []struct {
		name  string
		build func(testing.TB, int) (*RGA, []Position)
	}{
		{name: "chain", build: benchmarkTombstonedRGAChain},
		{name: "wide", build: benchmarkTombstonedRGAWide},
	} {
		for _, tombstoneCount := range []int{128, 1024} {
			b.Run(workload.name+"_"+strconv.Itoa(tombstoneCount), func(b *testing.B) {
				b.ReportAllocs()
				for index := 0; index < b.N; index++ {
					b.StopTimer()
					value, tags := workload.build(b, tombstoneCount)
					b.StartTimer()
					for tagIndex := len(tags) - 1; tagIndex >= 0; tagIndex-- {
						removed, err := value.CompactTombstones([]Position{tags[tagIndex]})
						if err != nil || removed != 1 {
							b.Fatalf("CompactTombstones(%d) = %d, %v; want 1, nil", tagIndex, removed, err)
						}
					}
					b.StopTimer()
				}
			})
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

func benchmarkTombstonedRGAChain(b testing.TB, count int) (*RGA, []Position) {
	b.Helper()
	value, err := New("writer")
	if err != nil {
		b.Fatal(err)
	}
	if _, err := value.Insert(0, strings.Repeat("a", count)); err != nil {
		b.Fatal(err)
	}
	if _, err := value.Delete(0, count); err != nil {
		b.Fatal(err)
	}
	return value, value.TombstoneTags()
}

func benchmarkTombstonedRGAWide(b testing.TB, count int) (*RGA, []Position) {
	b.Helper()
	value, err := New("writer")
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < count; index++ {
		if _, err := value.Insert(0, "a"); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := value.Delete(0, count); err != nil {
		b.Fatal(err)
	}
	return value, value.TombstoneTags()
}
