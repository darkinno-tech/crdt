package register

import (
	"fmt"
	"testing"
)

func BenchmarkMVRegisterMerge(b *testing.B) {
	left, right := benchmarkMVRegisters(b)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := left.Merge(right); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMVRegisterApplyDelta(b *testing.B) {
	target, source := benchmarkMVRegisters(b)
	delta, err := source.Set([]byte("delta"))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := target.ApplyDelta(delta); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMVRegisterApplyDeltaParallelDuplicate(b *testing.B) {
	target, source := benchmarkMVRegisters(b)
	delta, err := source.Set([]byte("delta"))
	if err != nil {
		b.Fatal(err)
	}
	if err := target.ApplyDelta(delta); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(parallel *testing.PB) {
		for parallel.Next() {
			if err := target.ApplyDelta(delta); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkMVRegisterMarshalBinary(b *testing.B) {
	value, _ := benchmarkMVRegisters(b)
	encoded, err := value.MarshalBinary()
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(encoded)))
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := value.MarshalBinary(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMVRegisterScale keeps one concurrent value per replica. It exposes
// the actual version-vector and visible-value scaling, unlike a sequence of
// local writes that retains only the final value.
func BenchmarkMVRegisterScale(b *testing.B) {
	for _, replicas := range []int{16, 256, 1024} {
		b.Run(fmt.Sprintf("merge-%d", replicas), func(b *testing.B) {
			left := benchmarkMVRegisterWithConcurrentValues(b, replicas)
			right := benchmarkMVRegisterWithConcurrentValues(b, replicas)
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if err := left.Merge(right); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("duplicate-delta-%d", replicas), func(b *testing.B) {
			target := benchmarkMVRegisterWithConcurrentValues(b, replicas)
			source := benchmarkMVRegisterWithConcurrentValues(b, replicas)
			delta, err := source.Set([]byte("resolved"))
			if err != nil {
				b.Fatal(err)
			}
			if err := target.ApplyDelta(delta); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if err := target.ApplyDelta(delta); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("marshal-%d", replicas), func(b *testing.B) {
			value := benchmarkMVRegisterWithConcurrentValues(b, replicas)
			encoded, err := value.MarshalBinary()
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(encoded)))
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if _, err := value.MarshalBinary(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchmarkMVRegisters(b *testing.B) (*MVRegister, *MVRegister) {
	b.Helper()
	left, err := NewMVRegister("left")
	if err != nil {
		b.Fatal(err)
	}
	right, err := NewMVRegister("right")
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 128; index++ {
		if _, err := left.Set([]byte(fmt.Sprintf("left-%03d", index))); err != nil {
			b.Fatal(err)
		}
		if _, err := right.Set([]byte(fmt.Sprintf("right-%03d", index))); err != nil {
			b.Fatal(err)
		}
	}
	return left, right
}

func benchmarkMVRegisterWithConcurrentValues(b *testing.B, replicas int) *MVRegister {
	b.Helper()
	target, err := NewMVRegister("target")
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < replicas; index++ {
		source, err := NewMVRegister(fmt.Sprintf("replica-%04d", index))
		if err != nil {
			b.Fatal(err)
		}
		if err := target.Merge(source); err != nil {
			b.Fatal(err)
		}
		delta, err := source.Set([]byte(fmt.Sprintf("value-%04d", index)))
		if err != nil {
			b.Fatal(err)
		}
		if err := target.ApplyDelta(delta); err != nil {
			b.Fatal(err)
		}
	}
	return target
}
