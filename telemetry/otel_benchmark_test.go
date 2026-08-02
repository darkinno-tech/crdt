package telemetry

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/DarkInno/crdt"
)

// BenchmarkOpenTelemetrySinkRecord measures the Metrics SDK aggregation work
// performed on Reporter's delivery goroutine. It deliberately calls Sink
// directly; BenchmarkReporterRecordOverloaded separately covers the hot-path
// queue-full behavior that must not wait for this work.
func BenchmarkOpenTelemetrySinkRecord(b *testing.B) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	b.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	sink, err := NewOpenTelemetrySink(OpenTelemetryOptions{MeterProvider: provider})
	if err != nil {
		b.Fatal(err)
	}
	event := Event{
		Time:      time.Unix(1, 0),
		Component: "durable",
		Operation: "append",
		Outcome:   OutcomeSuccess,
		Duration:  time.Millisecond,
		ErrorCode: crdt.ErrorCodeUnavailable,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		sink(event)
	}
}

func BenchmarkOpenTelemetrySinkRecordDisabled(b *testing.B) {
	sink, err := NewOpenTelemetrySink(OpenTelemetryOptions{MeterProvider: noop.NewMeterProvider()})
	if err != nil {
		b.Fatal(err)
	}
	event := Event{
		Time:      time.Unix(1, 0),
		Component: "durable",
		Operation: "append",
		Outcome:   OutcomeSuccess,
		Duration:  time.Millisecond,
		ErrorCode: crdt.ErrorCodeUnavailable,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		sink(event)
	}
}

func BenchmarkOpenTelemetrySinkRecordSanitized(b *testing.B) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	b.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	sink, err := NewOpenTelemetrySink(OpenTelemetryOptions{MeterProvider: provider})
	if err != nil {
		b.Fatal(err)
	}
	event := Event{
		Time:      time.Unix(1, 0),
		Component: "tenantprivate",
		Operation: "appendprivate",
		Outcome:   OutcomeRejected,
		Duration:  time.Millisecond,
		ErrorCode: crdt.ErrorCode("private"),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		sink(event)
	}
}
