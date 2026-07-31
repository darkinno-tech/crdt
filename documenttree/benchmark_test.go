package documenttree

import (
	"fmt"
	"testing"
)

// BenchmarkDocumentTreeKanbanFrame models a realistic workboard: a root map,
// nested cards array, per-card maps, and independent field updates framed for
// a second offline editor. Setup is excluded from timing.
func BenchmarkDocumentTreeKanbanFrame(b *testing.B) {
	source := mustDocument(b, "source")
	root, _, err := source.CreateRootMap("workspace")
	if err != nil {
		b.Fatal(err)
	}
	board, _, err := root.CreateMap("board")
	if err != nil {
		b.Fatal(err)
	}
	cards, _, err := board.CreateArray("cards")
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 128; index++ {
		card, _, err := cards.InsertMap(cards.Len())
		if err != nil {
			b.Fatal(err)
		}
		if _, err := card.Set("id", []byte(fmt.Sprintf("card-%03d", index))); err != nil {
			b.Fatal(err)
		}
		if _, err := card.Set("state", []byte("draft")); err != nil {
			b.Fatal(err)
		}
	}
	state, err := source.MarshalBinary()
	if err != nil {
		b.Fatal(err)
	}
	target := mustDocument(b, "target")
	if err := target.UnmarshalBinary(state); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		card, ok := cards.Map(index % cards.Len())
		if !ok {
			b.Fatal("card missing")
		}
		delta, err := card.Set("state", []byte(fmt.Sprintf("review-%d", index&1)))
		if err != nil {
			b.Fatal(err)
		}
		encoded, err := delta.MarshalBinary()
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(encoded)))
		decoded, err := UnmarshalDelta(encoded)
		if err != nil {
			b.Fatal(err)
		}
		if err := target.ApplyDelta(decoded); err != nil {
			b.Fatal(err)
		}
	}
}
