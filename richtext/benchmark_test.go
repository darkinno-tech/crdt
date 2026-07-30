package richtext

import (
	"strings"
	"testing"
)

const benchmarkRichTextRunes = 10_000

func BenchmarkRichTextFormatTenThousandRunes(b *testing.B) {
	seed := benchmarkRichTextSeed(b, benchmarkRichTextRunes)
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		b.StopTimer()
		document := mustDocument(b, "format-target")
		if err := document.ApplyDelta(seed); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := document.Format(0, benchmarkRichTextRunes, []AttributeChange{{Key: "highlight", Value: "yellow"}}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRichTextApplyFormattedSelectionTenThousandRunes(b *testing.B) {
	source := mustDocument(b, "format-source")
	seed, err := source.Insert(0, strings.Repeat("a", benchmarkRichTextRunes))
	if err != nil {
		b.Fatal(err)
	}
	format, err := source.Format(0, benchmarkRichTextRunes, []AttributeChange{{Key: "highlight", Value: "yellow"}})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		b.StopTimer()
		document := mustDocument(b, "format-receiver")
		if err := document.ApplyDelta(seed); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := document.ApplyDelta(format); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRichTextMarshalStateTenThousandFormattedRunes(b *testing.B) {
	document := mustDocument(b, "marshal")
	if _, err := document.Insert(0, strings.Repeat("a", benchmarkRichTextRunes)); err != nil {
		b.Fatal(err)
	}
	if _, err := document.Format(0, benchmarkRichTextRunes, []AttributeChange{{Key: "bold", Value: "true"}}); err != nil {
		b.Fatal(err)
	}
	encoded, err := document.MarshalBinary()
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(encoded)))
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := document.MarshalBinary(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRichTextRenderSpansTenThousandFormattedRunes(b *testing.B) {
	document := mustDocument(b, "render")
	if _, err := document.Insert(0, strings.Repeat("a", benchmarkRichTextRunes)); err != nil {
		b.Fatal(err)
	}
	if _, err := document.Format(0, benchmarkRichTextRunes/2, []AttributeChange{{Key: "bold", Value: "true"}}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if spans := document.Spans(); len(spans) != 2 {
			b.Fatalf("Spans() = %d spans, want 2", len(spans))
		}
	}
}

func benchmarkRichTextSeed(b testing.TB, runes int) Delta {
	b.Helper()
	document := mustDocument(b, "seed")
	change, err := document.Insert(0, strings.Repeat("a", runes))
	if err != nil {
		b.Fatal(err)
	}
	return change
}
