package text

import (
	"strings"
	"testing"
)

func BenchmarkRGAAnchorResolve(b *testing.B) {
	const runes = 100_000
	document := mustBenchmarkRGA(b, "anchor-benchmark")
	if _, err := document.Insert(0, strings.Repeat("x", runes)); err != nil {
		b.Fatal(err)
	}
	anchor, err := document.AnchorAt(runes / 2)
	if err != nil {
		b.Fatal(err)
	}

	b.Run("relative-anchor", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			offset, err := document.ResolveAnchor(anchor)
			if err != nil || offset != runes/2 {
				b.Fatalf("ResolveAnchor = %d, %v", offset, err)
			}
		}
	})

	b.Run("visible-projection", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			positions := document.Positions()
			if len(positions) != runes || positions[runes/2] != anchor.Position {
				b.Fatal("visible projection did not preserve the anchor position")
			}
		}
	})
}
