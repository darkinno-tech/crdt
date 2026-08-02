package documenttree

import (
	"fmt"
	"testing"
)

// BenchmarkDocumentTreeKanbanFrame models an application-shaped workboard: a
// root map, nested cards array, per-card maps, and independent field updates
// framed for a second offline editor. Setup is excluded from timing.
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

// BenchmarkDocumentTreeFullNestedSnapshot measures complete-tree persistence
// and recovery for a workspace with nested page, column, card, and comment
// objects. It deliberately exercises the v2 contract: all reachable content
// is included in each state frame, with no deferred descendant fetch.
func BenchmarkDocumentTreeFullNestedSnapshot(b *testing.B) {
	source := mustDocument(b, "source")
	workspace, _, err := source.CreateRootMap("workspace")
	if err != nil {
		b.Fatal(err)
	}
	pages, _, err := workspace.CreateArray("pages")
	if err != nil {
		b.Fatal(err)
	}
	for pageIndex := 0; pageIndex < 32; pageIndex++ {
		page, _, err := pages.InsertMap(pages.Len())
		if err != nil {
			b.Fatal(err)
		}
		if _, err := page.Set("title", []byte(fmt.Sprintf("page-%02d", pageIndex))); err != nil {
			b.Fatal(err)
		}
		columns, _, err := page.CreateArray("columns")
		if err != nil {
			b.Fatal(err)
		}
		for columnIndex := 0; columnIndex < 3; columnIndex++ {
			column, _, err := columns.InsertMap(columns.Len())
			if err != nil {
				b.Fatal(err)
			}
			cards, _, err := column.CreateArray("cards")
			if err != nil {
				b.Fatal(err)
			}
			for cardIndex := 0; cardIndex < 4; cardIndex++ {
				card, _, err := cards.InsertMap(cards.Len())
				if err != nil {
					b.Fatal(err)
				}
				if _, err := card.Set("body", []byte("complete nested state")); err != nil {
					b.Fatal(err)
				}
				comments, _, err := card.CreateArray("comments")
				if err != nil {
					b.Fatal(err)
				}
				if _, err := comments.Insert(0, []byte("replicated with the parent")); err != nil {
					b.Fatal(err)
				}
			}
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		state, err := source.MarshalBinary()
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(state)))
		restored := mustDocument(b, fmt.Sprintf("restore-%d", index))
		if err := restored.UnmarshalBinary(state); err != nil {
			b.Fatal(err)
		}
	}
}
