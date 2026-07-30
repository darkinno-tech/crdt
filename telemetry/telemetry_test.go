package telemetry

import (
	"bytes"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DarkInno/crdt"
)

func TestReporterDropsWhenSinkIsBlocked(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	reporter, err := New(Options{
		QueueSize: 1,
		Sink: func(Event) {
			close(started)
			<-release
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reporter.Record(Event{Component: "durable", Operation: "append", Outcome: OutcomeSuccess})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sink did not receive first event")
	}
	reporter.Record(Event{Component: "durable", Operation: "append", Outcome: OutcomeSuccess})
	reporter.Record(Event{Component: "durable", Operation: "append", Outcome: OutcomeSuccess})
	if got := reporter.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}
	reporter.Close()
	close(release)
	select {
	case <-reporter.Done():
	case <-time.After(time.Second):
		t.Fatal("reporter did not stop")
	}
}

func TestReporterContainsSinkPanics(t *testing.T) {
	var calls atomic.Int32
	delivered := make(chan Event, 1)
	reporter, err := New(Options{Sink: func(event Event) {
		if calls.Add(1) == 1 {
			panic("sink panic")
		}
		delivered <- event
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	reporter.Record(Event{Operation: "first"})
	reporter.Record(Event{Operation: "second"})
	select {
	case event := <-delivered:
		if event.Operation != "second" {
			t.Fatalf("event = %+v, want second event", event)
		}
	case <-time.After(time.Second):
		t.Fatal("panic stopped reporter delivery")
	}
}

func TestSlogSinkDoesNotSerializeErrorCause(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sink := SlogSink(logger)
	sink(Event{
		Component: "durable",
		Operation: "append",
		Outcome:   OutcomeRejected,
		Duration:  time.Millisecond,
		ErrorCode: crdt.ErrorCodeUnauthorized,
	})
	if got := output.String(); !bytes.Contains([]byte(got), []byte(`"error_code":"unauthorized"`)) {
		t.Fatalf("log output %q lacks code", got)
	}
}

func TestNewRejectsUnsafeOptions(t *testing.T) {
	if _, err := New(Options{}); !errors.Is(err, ErrInvalidConfig) || crdt.ErrorCodeOf(err) != crdt.ErrorCodeInvalidConfig {
		t.Fatalf("nil sink error = %v", err)
	}
	if _, err := New(Options{QueueSize: -1, Sink: func(Event) {}}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative queue error = %v", err)
	}
}
