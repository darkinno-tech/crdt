package durable

import (
	"testing"
	"time"

	"github.com/darkinno-tech/crdt/telemetry"
)

func BenchmarkHandlerRecordDisabled(b *testing.B) {
	handler := &Handler{}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		handler.record("append", handler.started(), nil)
	}
}

func BenchmarkHandlerRecordOverloaded(b *testing.B) {
	block := make(chan struct{})
	reporter, err := telemetry.New(telemetry.Options{QueueSize: 1, Sink: func(telemetry.Event) { <-block }})
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		reporter.Close()
		close(block)
		<-reporter.Done()
	}()
	handler := &Handler{telemetry: reporter}
	handler.record("append", handler.started(), nil)
	time.Sleep(time.Millisecond)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		handler.record("append", handler.started(), nil)
	}
}
