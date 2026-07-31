package document

import (
	"fmt"
	"testing"
)

// BenchmarkDocManagerParallelIndependentMoves models a service hosting many
// independent drag-and-drop documents. Setup is outside the timed region; the
// benchmark measures registry lookup plus per-document MoveRGA work without a
// shared document lock.
func BenchmarkDocManagerParallelIndependentMoves(b *testing.B) {
	const documents = 10_000
	manager := mustManager(b)
	ids := make([]string, documents)
	for index := range ids {
		ids[index] = fmt.Sprintf("document-%05d", index)
		if _, err := manager.CreateDocument(ids[index], fmt.Sprintf("writer-%05d", index)); err != nil {
			b.Fatal(err)
		}
		if _, err := manager.Insert(ids[index], 0, []string{"left", "right"}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.SetBytes(2)
	b.ResetTimer()
	b.RunParallel(func(worker *testing.PB) {
		index := 0
		for worker.Next() {
			if _, err := manager.Move(ids[index%len(ids)], 0, 1, 1); err != nil {
				b.Fatal(err)
			}
			index++
		}
	})
}
