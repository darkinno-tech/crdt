// Package telemetry provides bounded, payload-free operational events for
// hosts that need to observe CRDT transport boundaries.
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darkinno-tech/crdt"
)

const defaultQueueSize = 256

var (
	// ErrInvalidConfig reports a nil sink or invalid bounded queue size.
	ErrInvalidConfig = errors.New("crdt telemetry: invalid configuration")
)

// Outcome is the privacy-safe result category for one operational event.
type Outcome string

const (
	OutcomeSuccess  Outcome = "success"
	OutcomeRejected Outcome = "rejected"
	OutcomeFailure  Outcome = "failure"
)

// Event describes an operational boundary without including replica IDs,
// group IDs, endpoints, headers, payloads, or application values. Component
// and Operation should be fixed names chosen by the library or host.
type Event struct {
	Time      time.Time
	Component string
	Operation string
	Outcome   Outcome
	Duration  time.Duration
	ErrorCode crdt.ErrorCode
}

// Sink receives events on a Reporter-owned goroutine. It may block or panic
// without delaying CRDT, transport, or request paths; a blocked sink only
// causes the report queue to fill and later events to be dropped.
type Sink func(Event)

// Options configures one bounded Reporter. QueueSize defaults to 256. Sink is
// required so construction cannot silently create a background no-op worker.
type Options struct {
	QueueSize int
	Sink      Sink
}

// Reporter asynchronously delivers a bounded stream of operational events.
// Record never waits for Sink and never changes application behavior. It is
// safe for concurrent use. Call Close during host shutdown to request delivery
// shutdown; Close does not wait for a blocked third-party sink.
type Reporter struct {
	sink Sink

	events chan Event
	stop   chan struct{}
	done   chan struct{}

	closed    atomic.Bool
	dropped   atomic.Uint64
	closeOnce sync.Once
}

// New creates a bounded asynchronous Reporter.
func New(options Options) (*Reporter, error) {
	if options.Sink == nil || options.QueueSize < 0 {
		return nil, crdt.WrapError(crdt.ErrorCodeInvalidConfig, "telemetry.new", ErrInvalidConfig)
	}
	if options.QueueSize == 0 {
		options.QueueSize = defaultQueueSize
	}
	reporter := &Reporter{
		sink:   options.Sink,
		events: make(chan Event, options.QueueSize),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go reporter.run()
	return reporter, nil
}

// Record queues event when capacity is available. It is intentionally lossy:
// the caller does not wait for observation, and Dropped exposes overload to a
// host metric. A nil Reporter is a zero-cost no-op.
func (reporter *Reporter) Record(event Event) {
	if reporter == nil || reporter.closed.Load() {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now()
	}
	select {
	case reporter.events <- event:
	default:
		reporter.dropped.Add(1)
	}
}

// Dropped reports events that could not enter the bounded queue.
func (reporter *Reporter) Dropped() uint64 {
	if reporter == nil {
		return 0
	}
	return reporter.dropped.Load()
}

// Close stops normal event acceptance and requests that the worker exit. One
// event that raced with Close may still reach Sink. Close is idempotent and
// returns immediately even when a third-party Sink has blocked.
func (reporter *Reporter) Close() {
	if reporter == nil {
		return
	}
	reporter.closeOnce.Do(func() {
		reporter.closed.Store(true)
		close(reporter.stop)
	})
}

// Done closes after the delivery goroutine exits. It can remain open when a
// third-party Sink blocks, which is why Close never waits for it.
func (reporter *Reporter) Done() <-chan struct{} {
	if reporter == nil {
		return nil
	}
	return reporter.done
}

func (reporter *Reporter) run() {
	defer close(reporter.done)
	for {
		select {
		case <-reporter.stop:
			return
		case event := <-reporter.events:
			reporter.deliver(event)
		}
	}
}

func (reporter *Reporter) deliver(event Event) {
	defer func() { _ = recover() }()
	reporter.sink(event)
}

// SlogSink adapts a standard-library structured logger. It records only the
// fields in Event; in particular, an underlying error and caller data are not
// attached. Failures are warnings, while successful events are debug records.
func SlogSink(logger *slog.Logger) Sink {
	if logger == nil {
		logger = slog.Default()
	}
	return func(event Event) {
		level := slog.LevelDebug
		if event.Outcome != OutcomeSuccess {
			level = slog.LevelWarn
		}
		attributes := []slog.Attr{
			slog.String("component", event.Component),
			slog.String("operation", event.Operation),
			slog.String("outcome", string(event.Outcome)),
			slog.Duration("duration", event.Duration),
		}
		if event.ErrorCode != "" && event.ErrorCode != crdt.ErrorCodeUnknown {
			attributes = append(attributes, slog.String("error_code", string(event.ErrorCode)))
		}
		logger.LogAttrs(context.Background(), level, "crdt operational event", attributes...)
	}
}
