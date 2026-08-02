package telemetry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/DarkInno/crdt"
)

func TestNewOpenTelemetrySinkExportsPayloadFreeMetricsThroughReporter(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	sink, err := NewOpenTelemetrySink(OpenTelemetryOptions{MeterProvider: provider})
	if err != nil {
		t.Fatal(err)
	}
	delivered := make(chan struct{}, 1)
	reporter, err := New(Options{QueueSize: 1, Sink: func(event Event) {
		sink(event)
		delivered <- struct{}{}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeReporter(t, reporter)

	reporter.Record(Event{
		Time:      time.Unix(1, 0),
		Component: "tenantprivate",
		Operation: "appendprivate",
		Outcome:   OutcomeRejected,
		Duration:  125 * time.Millisecond,
		ErrorCode: crdt.ErrorCode("secret-error-code"),
	})
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("OpenTelemetry sink did not receive the reporter event")
	}

	metrics := collectOpenTelemetryMetrics(t, reader)
	count := findOpenTelemetrySum(t, metrics, openTelemetryEventsMetricName)
	if len(count.DataPoints) != 1 || count.DataPoints[0].Value != 1 {
		t.Fatalf("event counter = %#v, want one event", count.DataPoints)
	}
	assertOpenTelemetryAttributes(t, count.DataPoints[0].Attributes, map[string]string{
		"crdt.component":  openTelemetryOtherDimensionValue,
		"crdt.operation":  openTelemetryOtherDimensionValue,
		"crdt.outcome":    string(OutcomeRejected),
		"crdt.error_code": string(crdt.ErrorCodeUnknown),
	})
	if got := count.DataPoints[0].Attributes.Encoded(attribute.DefaultEncoder()); strings.Contains(got, "private") || strings.Contains(got, "secret") {
		t.Fatalf("OpenTelemetry attributes leaked untrusted value: %q", got)
	}

	duration := findOpenTelemetryHistogram(t, metrics, openTelemetryDurationMetricName)
	if len(duration.DataPoints) != 1 || duration.DataPoints[0].Count != 1 || duration.DataPoints[0].Sum != 0.125 {
		t.Fatalf("duration histogram = %#v, want one 125ms observation", duration.DataPoints)
	}
	assertOpenTelemetryAttributes(t, duration.DataPoints[0].Attributes, map[string]string{
		"crdt.component":  openTelemetryOtherDimensionValue,
		"crdt.operation":  openTelemetryOtherDimensionValue,
		"crdt.outcome":    string(OutcomeRejected),
		"crdt.error_code": string(crdt.ErrorCodeUnknown),
	})
}

func TestRegisterOpenTelemetryDroppedMetricObservesOverload(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	reporter, err := New(Options{QueueSize: 1, Sink: func(Event) {
		close(started)
		<-release
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		reporter.Close()
		close(release)
		select {
		case <-reporter.Done():
		case <-time.After(time.Second):
			t.Fatal("reporter did not stop")
		}
	}()
	reporter.Record(Event{})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sink did not block")
	}
	reporter.Record(Event{})
	reporter.Record(Event{})
	if got := reporter.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	registration, err := RegisterOpenTelemetryDroppedMetric(reporter, OpenTelemetryOptions{MeterProvider: provider})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registration.Unregister() })

	metrics := collectOpenTelemetryMetrics(t, reader)
	dropped := findOpenTelemetrySum(t, metrics, openTelemetryDroppedMetricName)
	if len(dropped.DataPoints) != 1 || dropped.DataPoints[0].Value != 1 {
		t.Fatalf("dropped counter = %#v, want one dropped event", dropped.DataPoints)
	}
	if attributes := dropped.DataPoints[0].Attributes.ToSlice(); len(attributes) != 0 {
		t.Fatalf("dropped counter attributes = %v, want none", attributes)
	}
}

func TestOpenTelemetryConfigurationRejectsNilBoundaries(t *testing.T) {
	if _, err := NewOpenTelemetrySink(OpenTelemetryOptions{}); !errors.Is(err, ErrInvalidConfig) || crdt.ErrorCodeOf(err) != crdt.ErrorCodeInvalidConfig {
		t.Fatalf("NewOpenTelemetrySink nil provider error = %v", err)
	}
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	if _, err := RegisterOpenTelemetryDroppedMetric(nil, OpenTelemetryOptions{MeterProvider: provider}); !errors.Is(err, ErrInvalidConfig) || crdt.ErrorCodeOf(err) != crdt.ErrorCodeInvalidConfig {
		t.Fatalf("RegisterOpenTelemetryDroppedMetric nil reporter error = %v", err)
	}
	if _, err := NewOpenTelemetrySink(OpenTelemetryOptions{MeterProvider: provider, AllowedComponents: []string{"invalid value"}}); !errors.Is(err, ErrInvalidConfig) || crdt.ErrorCodeOf(err) != crdt.ErrorCodeInvalidConfig {
		t.Fatalf("NewOpenTelemetrySink invalid component error = %v", err)
	}
	if _, err := NewOpenTelemetrySink(OpenTelemetryOptions{MeterProvider: provider, AllowedOperations: make([]string, maxOpenTelemetryHostDimensions+1)}); !errors.Is(err, ErrInvalidConfig) || crdt.ErrorCodeOf(err) != crdt.ErrorCodeInvalidConfig {
		t.Fatalf("NewOpenTelemetrySink excessive operations error = %v", err)
	}
}

func TestOpenTelemetryAttributeFilteringKeepsDimensionsBounded(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "fixed", value: "append_batch", want: "append_batch"},
		{name: "empty", value: "", want: openTelemetryOtherDimensionValue},
		{name: "URL", value: "https://example.test/private", want: openTelemetryOtherDimensionValue},
		{name: "Unicode", value: "同步", want: openTelemetryOtherDimensionValue},
		{name: "too long", value: strings.Repeat("a", maxOpenTelemetryDimensionLength+1), want: openTelemetryOtherDimensionValue},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := openTelemetryDimension(test.value); got != test.want {
				t.Fatalf("openTelemetryDimension(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
	if got := openTelemetryOutcome(Outcome("invalid")); got != openTelemetryOtherDimensionValue {
		t.Fatalf("openTelemetryOutcome(invalid) = %q", got)
	}
	if got := openTelemetryErrorCode(crdt.ErrorCode("private")); got != string(crdt.ErrorCodeUnknown) {
		t.Fatalf("openTelemetryErrorCode(private) = %q", got)
	}
	if got := openTelemetryCounterValue(uint64(maxOpenTelemetryCounterValue) + 1); got != maxOpenTelemetryCounterValue {
		t.Fatalf("openTelemetryCounterValue(overflow) = %d, want %d", got, maxOpenTelemetryCounterValue)
	}
}

func TestOpenTelemetrySinkSupportsConcurrentCalls(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	sink, err := NewOpenTelemetrySink(OpenTelemetryOptions{MeterProvider: provider})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 16
	const eventsPerWorker = 64
	event := Event{Component: "extensions", Operation: "append_batch", Outcome: OutcomeSuccess, Duration: time.Millisecond}
	var group sync.WaitGroup
	group.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func() {
			defer group.Done()
			for eventIndex := 0; eventIndex < eventsPerWorker; eventIndex++ {
				sink(event)
			}
		}()
	}
	group.Wait()

	metrics := collectOpenTelemetryMetrics(t, reader)
	count := findOpenTelemetrySum(t, metrics, openTelemetryEventsMetricName)
	if len(count.DataPoints) != 1 || count.DataPoints[0].Value != workers*eventsPerWorker {
		t.Fatalf("event counter = %#v, want %d concurrent events", count.DataPoints, workers*eventsPerWorker)
	}
}

func TestOpenTelemetrySinkExportsExplicitHostDimensions(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	sink, err := NewOpenTelemetrySink(OpenTelemetryOptions{
		MeterProvider:     provider,
		AllowedComponents: []string{"host_relay"},
		AllowedOperations: []string{"forward"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sink(Event{Component: "host_relay", Operation: "forward", Outcome: OutcomeSuccess})

	metrics := collectOpenTelemetryMetrics(t, reader)
	count := findOpenTelemetrySum(t, metrics, openTelemetryEventsMetricName)
	if len(count.DataPoints) != 1 || count.DataPoints[0].Value != 1 {
		t.Fatalf("event counter = %#v, want one event", count.DataPoints)
	}
	assertOpenTelemetryAttributes(t, count.DataPoints[0].Attributes, map[string]string{
		"crdt.component": "host_relay",
		"crdt.operation": "forward",
		"crdt.outcome":   string(OutcomeSuccess),
	})
}

func collectOpenTelemetryMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatal(err)
	}
	return metrics
}

func findOpenTelemetrySum(t *testing.T, resourceMetrics metricdata.ResourceMetrics, name string) metricdata.Sum[int64] {
	t.Helper()
	for _, scopeMetrics := range resourceMetrics.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			if metric.Name != name {
				continue
			}
			sum, ok := metric.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %q data = %T, want metricdata.Sum[int64]", name, metric.Data)
			}
			return sum
		}
	}
	t.Fatalf("metric %q was not collected", name)
	return metricdata.Sum[int64]{}
}

func findOpenTelemetryHistogram(t *testing.T, resourceMetrics metricdata.ResourceMetrics, name string) metricdata.Histogram[float64] {
	t.Helper()
	for _, scopeMetrics := range resourceMetrics.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			if metric.Name != name {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("metric %q data = %T, want metricdata.Histogram[float64]", name, metric.Data)
			}
			return histogram
		}
	}
	t.Fatalf("metric %q was not collected", name)
	return metricdata.Histogram[float64]{}
}

func assertOpenTelemetryAttributes(t *testing.T, attributes attribute.Set, want map[string]string) {
	t.Helper()
	got := make(map[string]string, attributes.Len())
	for _, attribute := range attributes.ToSlice() {
		got[string(attribute.Key)] = attribute.Value.AsString()
	}
	if len(got) != len(want) {
		t.Fatalf("attribute count = %d, want %d: got %v", len(got), len(want), got)
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Fatalf("attribute %q = %q, want %q; all=%v", key, got[key], wantValue, got)
		}
	}
}

func closeReporter(t *testing.T, reporter *Reporter) {
	t.Helper()
	reporter.Close()
	select {
	case <-reporter.Done():
	case <-time.After(time.Second):
		t.Fatal("reporter did not stop")
	}
}
