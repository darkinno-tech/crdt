package list

import (
	"strconv"
	"testing"
)

func BenchmarkRGAAppendIndexedList(b *testing.B) {
	value, err := New("writer", stringCodec{})
	if err != nil {
		b.Fatal(err)
	}
	seed := make([]string, 10_000)
	for index := range seed {
		seed[index] = strconv.Itoa(index)
	}
	if _, err := value.Insert(0, seed); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := value.Insert(value.State().ElementCount, []string{"x"}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRGAValuesTenThousand(b *testing.B) {
	value, err := New("writer", stringCodec{})
	if err != nil {
		b.Fatal(err)
	}
	seed := make([]string, 10_000)
	for index := range seed {
		seed[index] = "x"
	}
	if _, err := value.Insert(0, seed); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		values, err := value.Values()
		if err != nil || len(values) != 10_000 {
			b.Fatalf("Values() = %d, %v", len(values), err)
		}
	}
}
