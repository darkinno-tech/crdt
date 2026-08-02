package richtext

import (
	"strings"
	"testing"

	"github.com/DarkInno/crdt/text"
)

func BenchmarkDocumentAnchorRange(b *testing.B) {
	const runes = 100_000
	document, err := New("rich-anchor-benchmark")
	if err != nil {
		b.Fatal(err)
	}
	if _, err := document.Insert(0, strings.Repeat("x", runes)); err != nil {
		b.Fatal(err)
	}
	anchors, err := document.AnchorRangeAt(runes/3, runes*2/3)
	if err != nil {
		b.Fatal(err)
	}
	encoded, err := anchors.MarshalBinary()
	if err != nil {
		b.Fatal(err)
	}

	b.Run("resolve-relative-range", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			start, end, err := document.ResolveAnchorRange(anchors)
			if err != nil || start != runes/3 || end != runes*2/3 {
				b.Fatalf("ResolveAnchorRange() = %d, %d, %v", start, end, err)
			}
		}
	})

	b.Run("encode-relative-range", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			value, err := anchors.MarshalBinary()
			if err != nil || len(value) != len(encoded) {
				b.Fatalf("MarshalBinary() = %d bytes, %v", len(value), err)
			}
		}
	})

	b.Run("decode-relative-range", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			value, err := text.UnmarshalAnchorRange(encoded)
			if err != nil || value != anchors {
				b.Fatalf("UnmarshalAnchorRange() = %#v, %v", value, err)
			}
		}
	})

	b.Run("full-visible-projection", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			spans := document.Spans()
			if len(spans) != 1 || len([]rune(spans[0].Text)) != runes {
				b.Fatal("Spans() did not preserve the visible projection")
			}
		}
	})
}
