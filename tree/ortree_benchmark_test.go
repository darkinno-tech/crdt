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
