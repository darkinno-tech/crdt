package list

import (
	"strconv"
	"testing"
)

// BenchmarkMoveRGASequentialMoveTail measures the correctness-preserving
// splice path for a long document. Moving the first visible element to the end
// rewrites its visible suffix so chained RGA insertions cannot be dragged with
// the moved element. Run with -benchtime=1x for a single realistic drag.
func BenchmarkMoveRGASequentialMoveTail(b *testing.B) {
	const elements = 10_000
	value := mustMoveList(b, "writer")
	seed := make([]string, elements)
	for index := range seed {
		seed[index] = strconv.Itoa(index)
	}
	if _, err := value.Append(seed); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(elements)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := value.Move(0, 1, elements-1); err != nil {
			b.Fatal(err)
		}
	}
}
