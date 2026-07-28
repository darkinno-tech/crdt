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
