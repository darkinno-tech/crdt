package telemetry

import (
	"testing"
	"time"
)

func BenchmarkReporterRecordNil(b *testing.B) {
	var reporter *Reporter
	event := Event{Time: time.Unix(1, 0), Component: "durable", Operation: "append", Outcome: OutcomeSuccess}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		reporter.Record(event)
	}
}

func BenchmarkReporterRecordOverloaded(b *testing.B) {
	block := make(chan struct{})
	reporter, err := New(Options{QueueSize: 1, Sink: func(Event) { <-block }})
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		reporter.Close()
		close(block)
		<-reporter.Done()
	}()
	event := Event{Time: time.Unix(1, 0), Component: "durable", Operation: "append", Outcome: OutcomeSuccess}
	reporter.Record(event)
	time.Sleep(time.Millisecond)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		reporter.Record(event)
	}
}
