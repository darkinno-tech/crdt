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

func BenchmarkRuntimeInsertAndApply4096RunesPackedV3(b *testing.B) {
	runtime, err := NewRuntime(DefaultPackedRGAOptions())
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

// BenchmarkRuntimeInitialSnapshotAndRestore65536Runes models browser initial
// synchronization: a source has accumulated a large document through bounded
// editor transactions, emits one complete state frame, and a fresh receiver
// validates and restores it. It is a controlled local measurement, not a
// network, storage, or device-capacity claim.
func BenchmarkRuntimeInitialSnapshotAndRestore65536Runes(b *testing.B) {
	for _, workload := range []struct {
		name    string
		options RGAOptions
	}{
		{name: "run_v2", options: DefaultRunRGAOptions()},
		{name: "packed_v3", options: DefaultPackedRGAOptions()},
	} {
		b.Run(workload.name, func(b *testing.B) {
			runtime, err := NewRuntime(workload.options)
			if err != nil {
				b.Fatal(err)
			}
			source, err := runtime.Create("initial-source")
			if err != nil {
				b.Fatal(err)
			}
			want := populateInitialDocument(b, runtime, source, 64<<10)
			first, err := runtime.Snapshot(source)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(first.State)))
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				saved, err := runtime.Snapshot(source)
				if err != nil {
					b.Fatal(err)
				}
				restored, err := runtime.Restore(saved)
				if err != nil {
					b.Fatal(err)
				}
				if got, err := runtime.Text(restored); err != nil || got != want {
					b.Fatalf("restored initial state = %d runes, %v", len([]rune(got)), err)
				}
				if !runtime.Drop(restored) {
					b.Fatal("restored document release failed")
				}
			}
			b.ReportMetric(float64(len(first.State)), "snapshot_bytes")
		})
	}
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
