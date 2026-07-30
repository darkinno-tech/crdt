package tree

import "testing"

func BenchmarkORTreeStateWideTree(b *testing.B) {
	value, err := New("benchmark")
	if err != nil {
		b.Fatal(err)
	}
	root, _, err := value.Add(NodeID{}, nil)
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 1024; index++ {
		if _, _, err := value.Add(root, nil); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		_ = value.State()
	}
}

func BenchmarkORTreeNodesWideTree(b *testing.B) {
	value, err := New("benchmark")
	if err != nil {
		b.Fatal(err)
	}
	root, _, err := value.Add(NodeID{}, nil)
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 1024; index++ {
		if _, _, err := value.Add(root, nil); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		_ = value.Nodes()
	}
}

// BenchmarkORTreeNodesWideTreeParallel measures concurrent reader pressure on
// one shared document. RunParallel is required here: changing GOMAXPROCS alone
// does not create concurrent callers.
func BenchmarkORTreeNodesWideTreeParallel(b *testing.B) {
	value, err := New("benchmark")
	if err != nil {
		b.Fatal(err)
	}
	root, _, err := value.Add(NodeID{}, nil)
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 1024; index++ {
		if _, _, err := value.Add(root, nil); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(worker *testing.PB) {
		for worker.Next() {
			_ = value.Nodes()
		}
	})
}

func BenchmarkORTreeUnmarshalState1024(b *testing.B) {
	state := benchmarkORTreeState(b, 1024)
	target, err := New("target")
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(state)))
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := target.UnmarshalBinary(state); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkORTreeRejectOverLimitState1024 measures untrusted state rejection
// under a receiver-local node budget. The parser must reject the node count
// before allocating a decoded node map or copying node values.
func BenchmarkORTreeRejectOverLimitState1024(b *testing.B) {
	state := benchmarkORTreeState(b, 1024)
	target, err := NewWithOptions("target", Options{MaxNodes: 1, MaxTombstones: 1, MaxValueBytes: 256})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(state)))
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := target.UnmarshalBinary(state); err != ErrResourceLimit {
			b.Fatalf("UnmarshalBinary() = %v, want %v", err, ErrResourceLimit)
		}
	}
}

func BenchmarkORTreeTombstoneTags1024(b *testing.B) {
	value, _ := benchmarkORTreeTombstones(b, 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		_ = value.TombstoneTags()
	}
}

// BenchmarkORTreeCreateAndCompactLeafTombstones1024 measures the full
// tombstone lifecycle. Keeping creation in the timed region makes long runs
// representative and avoids hiding an unbounded amount of setup work.
func BenchmarkORTreeCreateAndCompactLeafTombstones1024(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		value, tags := benchmarkORTreeTombstones(b, 1024)
		removed, err := value.CompactTombstones(tags)
		if err != nil || removed != len(tags) {
			b.Fatalf("CompactTombstones() = %d, %v; want %d, nil", removed, err, len(tags))
		}
	}
}

func BenchmarkORTreeApplyDeltaLinearChain(b *testing.B) {
	delta := linearTreeDelta(100_000)
	b.SetBytes(100_000)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		target, err := NewWithOptions("target", Options{MaxNodes: 200_000, MaxTombstones: 200_000, MaxValueBytes: 1})
		if err != nil {
			b.Fatal(err)
		}
		if err := target.ApplyDelta(delta); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkORTreeRejectOverLimitLinearChain(b *testing.B) {
	delta := linearTreeDelta(100_000)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		target, err := NewWithOptions("target", Options{MaxNodes: 1, MaxTombstones: 1, MaxValueBytes: 1})
		if err != nil {
			b.Fatal(err)
		}
		if err := target.ApplyDelta(delta); err != ErrResourceLimit {
			b.Fatalf("ApplyDelta() = %v, want %v", err, ErrResourceLimit)
		}
	}
}

func BenchmarkORTreeApplyDuplicateDelta(b *testing.B) {
	source, err := New("source")
	if err != nil {
		b.Fatal(err)
	}
	_, delta, err := source.Add(NodeID{}, []byte("root"))
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

func benchmarkORTreeTombstones(b testing.TB, count int) (*ORTree, []NodeID) {
	b.Helper()
	value, err := New("benchmark")
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < count; index++ {
		id, _, err := value.Add(NodeID{}, nil)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := value.Remove(id); err != nil {
			b.Fatal(err)
		}
	}
	return value, value.TombstoneTags()
}

func benchmarkORTreeState(b testing.TB, children int) []byte {
	b.Helper()
	value, err := New("benchmark")
	if err != nil {
		b.Fatal(err)
	}
	payload := make([]byte, 256)
	root, _, err := value.Add(NodeID{}, payload)
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < children; index++ {
		if _, _, err := value.Add(root, payload); err != nil {
			b.Fatal(err)
		}
	}
	state, err := value.MarshalBinary()
	if err != nil {
		b.Fatal(err)
	}
	return state
}
