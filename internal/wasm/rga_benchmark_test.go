package wasm

import (
	"strings"
	"testing"
)

func BenchmarkRuntimeInsertAndApply4096Runes(b *testing.B) {
	runtime, err := NewRuntime(DefaultRGAOptions())
	if err != nil {
		b.Fatal(err)
	}
	payload := strings.Repeat("协", 4096)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	frameBytes := 0
	for index := 0; index < b.N; index++ {
		source, err := runtime.Create("source")
		if err != nil {
			b.Fatal(err)
		}
		target, err := runtime.Create("target")
		if err != nil {
			b.Fatal(err)
		}
		delta, err := runtime.Insert(source, 0, payload)
		if err != nil {
			b.Fatal(err)
		}
		frameBytes = len(delta)
		if err := runtime.ApplyDelta(target, delta); err != nil {
			b.Fatal(err)
		}
		if !runtime.Drop(source) || !runtime.Drop(target) {
			b.Fatal("document release failed")
		}
	}
	b.ReportMetric(float64(frameBytes), "frame_bytes")
}

func BenchmarkRuntimeInsertAndApply4096RunesRunV2(b *testing.B) {
	runtime, err := NewRuntime(DefaultRunRGAOptions())
	if err != nil {
		b.Fatal(err)
	}
	payload := strings.Repeat("协", 4096)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	frameBytes := 0
	for index := 0; index < b.N; index++ {
		source, err := runtime.Create("source")
		if err != nil {
			b.Fatal(err)
		}
		target, err := runtime.Create("target")
		if err != nil {
			b.Fatal(err)
		}
		delta, err := runtime.Insert(source, 0, payload)
		if err != nil {
			b.Fatal(err)
		}
		frameBytes = len(delta)
		if err := runtime.ApplyDelta(target, delta); err != nil {
			b.Fatal(err)
		}
		if !runtime.Drop(source) || !runtime.Drop(target) {
			b.Fatal("document release failed")
		}
	}
	b.ReportMetric(float64(frameBytes), "frame_bytes")
}

func BenchmarkRuntimeApplyDuplicateDelta(b *testing.B) {
	runtime, err := NewRuntime(DefaultRGAOptions())
	if err != nil {
		b.Fatal(err)
	}
	source, err := runtime.Create("source")
	if err != nil {
		b.Fatal(err)
	}
	target, err := runtime.Create("target")
	if err != nil {
		b.Fatal(err)
	}
	delta, err := runtime.Insert(source, 0, "duplicate delivery")
	if err != nil {
		b.Fatal(err)
	}
	if err := runtime.ApplyDelta(target, delta); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(delta)))
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := runtime.ApplyDelta(target, delta); err != nil {
			b.Fatal(err)
		}
	}
}
