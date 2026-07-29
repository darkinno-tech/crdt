package merkle

import (
	"strconv"
	"testing"
)

func BenchmarkTreeRoot(b *testing.B) {
	for _, entryCount := range []int{128, 4096} {
		b.Run("cached_entries_"+strconv.Itoa(entryCount), func(b *testing.B) {
			tree := benchmarkTree(b, entryCount)
			_ = tree.Root()
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				_ = tree.Root()
			}
		})

		b.Run("after_write_entries_"+strconv.Itoa(entryCount), func(b *testing.B) {
			tree := benchmarkTree(b, entryCount)
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				tree.Insert("hot-key", []byte(strconv.Itoa(index)))
				_ = tree.Root()
			}
		})
	}
}

func BenchmarkTreeDiff(b *testing.B) {
	for _, entryCount := range []int{128, 4096} {
		b.Run("equal_entries_"+strconv.Itoa(entryCount), func(b *testing.B) {
			left := benchmarkTree(b, entryCount)
			right := benchmarkTree(b, entryCount)
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				leftOnly, rightOnly, different := Diff(left, right)
				if len(leftOnly) != 0 || len(rightOnly) != 0 || len(different) != 0 {
					b.Fatal("equal trees differ")
				}
			}
		})
	}
}

func benchmarkTree(b *testing.B, entryCount int) *Tree {
	b.Helper()
	tree := NewTree()
	for index := 0; index < entryCount; index++ {
		tree.Insert("key-"+strconv.Itoa(index), []byte("value-"+strconv.Itoa(index)))
	}
	return tree
}
