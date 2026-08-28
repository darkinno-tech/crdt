package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/darkinno-tech/crdt"
)

const (
	openTelemetryMeterName           = "github.com/darkinno-tech/crdt/telemetry"
	openTelemetryEventsMetricName    = "crdt.telemetry.events"
	openTelemetryDurationMetricName  = "crdt.telemetry.duration"
	openTelemetryDroppedMetricName   = "crdt.telemetry.dropped"
	openTelemetryOtherDimensionValue = "other"
	maxOpenTelemetryDimensionLength  = 64
	maxOpenTelemetryCounterValue     = int64(1<<63 - 1)
	maxOpenTelemetryHostDimensions   = 16
)

// OpenTelemetryOptions configures an explicit OpenTelemetry Metrics export.
// MeterProvider is required: this package never selects a process-global
// provider or configures an exporter, endpoint, credential, or retry policy.
// The host owns those deployment concerns.
type OpenTelemetryOptions struct {
	MeterProvider metric.MeterProvider

	// AllowedComponents and AllowedOperations explicitly add fixed host labels
	// to the library's built-in telemetry vocabulary. Each list is limited to
	// 16 short ASCII labels. Undeclared Event values are exported as "other".
	// Do not add IDs, endpoints, headers, payload fragments, or business data.
	AllowedComponents []string
	AllowedOperations []string
}

type openTelemetryAttributeKey struct {
	component string
	operation string
	outcome   Outcome
	errorCode crdt.ErrorCode
}

type openTelemetrySink struct {
	events            metric.Int64Counter
	duration          metric.Float64Histogram
	attributes        map[openTelemetryAttributeKey]metric.MeasurementOption
	allowedComponents map[string]struct{}
	allowedOperations map[string]struct{}
}

// NewOpenTelemetrySink adapts payload-free Events to OpenTelemetry Metrics.
// It records crdt.telemetry.events ({event}) and crdt.telemetry.duration (s)
// with only crdt.component, crdt.operation, crdt.outcome, and an optional
// crdt.error_code attribute. The Event timestamp is intentionally not
// exported: the configured Metrics SDK assigns collection timestamps.
//
// Component and Operation are reduced to "other" unless they belong to the
// library's fixed vocabulary or an explicit host allow-list. This prevents
// URLs, headers, IDs, payload fragments, and application values from becoming
// metric attributes. Hosts must use fixed, low-cardinality labels.
//
// The returned Sink is intended for Reporter. Reporter calls it from its
// bounded delivery goroutine, so a slow Metrics provider cannot delay CRDT or
// transport paths. This function does not configure an OTLP exporter.
func NewOpenTelemetrySink(options OpenTelemetryOptions) (Sink, error) {
	meter, err := openTelemetryMeter(options)
	if err != nil {
		return nil, err
	}
	allowedComponents, err := openTelemetryDimensions(options.AllowedComponents, openTelemetryComponents)
	if err != nil {
		return nil, err
	}
	allowedOperations, err := openTelemetryDimensions(options.AllowedOperations, openTelemetryOperations)
	if err != nil {
		return nil, err
	}
	events, err := meter.Int64Counter(
		openTelemetryEventsMetricName,
		metric.WithDescription("Count of payload-free CRDT operational events."),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return nil, crdt.WrapError(crdt.ErrorCodeInvalidConfig, "telemetry.new_opentelemetry_sink", err)
	}
	duration, err := meter.Float64Histogram(
		openTelemetryDurationMetricName,
		metric.WithDescription("Duration of payload-free CRDT operational events."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, crdt.WrapError(crdt.ErrorCodeInvalidConfig, "telemetry.new_opentelemetry_sink", err)
	}
	sink := &openTelemetrySink{
		events:            events,
		duration:          duration,
		attributes:        newOpenTelemetryAttributeOptions(),
		allowedComponents: allowedComponents,
		allowedOperations: allowedOperations,
	}
	return sink.record, nil
}

func (sink *openTelemetrySink) record(event Event) {
	ctx := context.Background()
	attributes := sink.attributeOption(event)
	sink.events.Add(ctx, 1, attributes)
	if event.Duration >= 0 {
		sink.duration.Record(ctx, event.Duration.Seconds(), attributes)
	}
}

func (sink *openTelemetrySink) attributeOption(event Event) metric.MeasurementOption {
	event.Component = openTelemetryAllowedDimension(event.Component, sink.allowedComponents)
	event.Operation = openTelemetryAllowedDimension(event.Operation, sink.allowedOperations)
	key := openTelemetryAttributeKey{
		component: event.Component,
		operation: event.Operation,
		outcome:   event.Outcome,
		errorCode: event.ErrorCode,
	}
	if attributes, ok := sink.attributes[key]; ok {
		return attributes
	}
	return metric.WithAttributeSet(openTelemetryAttributes(event))
}

func newOpenTelemetryAttributeOptions() map[openTelemetryAttributeKey]metric.MeasurementOption {
	components := openTelemetryComponents
	operations := openTelemetryOperations
	outcomes := []Outcome{OutcomeSuccess, OutcomeRejected, OutcomeFailure}
	errorCodes := []crdt.ErrorCode{
		"",
		crdt.ErrorCodeUnknown,
		crdt.ErrorCodeInvalidConfig,
		crdt.ErrorCodeInvalidInput,
		crdt.ErrorCodeUnauthorized,
		crdt.ErrorCodeConflict,
		crdt.ErrorCodeResourceLimit,
		crdt.ErrorCodeUnavailable,
	}
	attributes := make(map[openTelemetryAttributeKey]metric.MeasurementOption, len(components)*len(operations)*len(outcomes)*len(errorCodes))
	for _, component := range components {
		for _, operation := range operations {
			for _, outcome := range outcomes {
				for _, errorCode := range errorCodes {
					key := openTelemetryAttributeKey{
						component: component,
						operation: operation,
						outcome:   outcome,
						errorCode: errorCode,
					}
					attributes[key] = metric.WithAttributeSet(openTelemetryAttributes(Event{
						Component: component,
						Operation: operation,
						Outcome:   outcome,
						ErrorCode: errorCode,
					}))
				}
			}
		}
	}
	return attributes
}

var (
	openTelemetryComponents = []string{"durable", "extensions"}
	openTelemetryOperations = []string{"handshake", "replay", "append", "append_batch"}
)

func openTelemetryDimensions(hostDimensions, libraryDimensions []string) (map[string]struct{}, error) {
	if len(hostDimensions) > maxOpenTelemetryHostDimensions {
		return nil, crdt.WrapError(crdt.ErrorCodeInvalidConfig, "telemetry.new_opentelemetry_sink", ErrInvalidConfig)
	}
	dimensions := make(map[string]struct{}, len(libraryDimensions)+len(hostDimensions))
	for _, dimension := range libraryDimensions {
		dimensions[dimension] = struct{}{}
	}
	for _, dimension := range hostDimensions {
		if openTelemetryDimension(dimension) != dimension {
			return nil, crdt.WrapError(crdt.ErrorCodeInvalidConfig, "telemetry.new_opentelemetry_sink", ErrInvalidConfig)
		}
		dimensions[dimension] = struct{}{}
	}
	return dimensions, nil
}

func openTelemetryAllowedDimension(value string, allowed map[string]struct{}) string {
	if _, ok := allowed[value]; ok {
		return value
	}
	return openTelemetryOtherDimensionValue
}

// RegisterOpenTelemetryDroppedMetric exports Reporter.Dropped as the
// crdt.telemetry.dropped ({event}) cumulative counter. It reads one atomic
// value only when the configured Metrics SDK collects, so it adds no work to
// Record's overload path. Call Unregister during host shutdown before
// discarding the Reporter or MeterProvider.
//
// One reporter should be registered for a MeterProvider. Multiple reporters
// have no reporter-ID attribute by design: adding such an unbounded label
// would weaken the payload-free, low-cardinality contract.
func RegisterOpenTelemetryDroppedMetric(reporter *Reporter, options OpenTelemetryOptions) (metric.Registration, error) {
	if reporter == nil {
		return nil, crdt.WrapError(crdt.ErrorCodeInvalidConfig, "telemetry.register_opentelemetry_dropped_metric", ErrInvalidConfig)
	}
	meter, err := openTelemetryMeter(options)
	if err != nil {
		return nil, err
	}
	dropped, err := meter.Int64ObservableCounter(
		openTelemetryDroppedMetricName,
		metric.WithDescription("Payload-free CRDT telemetry events dropped because the local queue was full."),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return nil, crdt.WrapError(crdt.ErrorCodeInvalidConfig, "telemetry.register_opentelemetry_dropped_metric", err)
	}
	registration, err := meter.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		observer.ObserveInt64(dropped, openTelemetryCounterValue(reporter.Dropped()))
		return nil
	}, dropped)
	if err != nil {
		return nil, crdt.WrapError(crdt.ErrorCodeInvalidConfig, "telemetry.register_opentelemetry_dropped_metric", err)
	}
	return registration, nil
}

func openTelemetryCounterValue(value uint64) int64 {
	if value > uint64(maxOpenTelemetryCounterValue) {
		return maxOpenTelemetryCounterValue
	}
	return int64(value)
}

func openTelemetryMeter(options OpenTelemetryOptions) (metric.Meter, error) {
	if options.MeterProvider == nil {
		return nil, crdt.WrapError(crdt.ErrorCodeInvalidConfig, "telemetry.opentelemetry_meter", ErrInvalidConfig)
	}
	return options.MeterProvider.Meter(openTelemetryMeterName), nil
}

func openTelemetryAttributes(event Event) attribute.Set {
	attributes := []attribute.KeyValue{
		attribute.String("crdt.component", openTelemetryDimension(event.Component)),
		attribute.String("crdt.operation", openTelemetryDimension(event.Operation)),
		attribute.String("crdt.outcome", openTelemetryOutcome(event.Outcome)),
	}
	if event.ErrorCode != "" {
		attributes = append(attributes, attribute.String("crdt.error_code", openTelemetryErrorCode(event.ErrorCode)))
	}
	return attribute.NewSet(attributes...)
}

func openTelemetryDimension(value string) string {
	if len(value) == 0 || len(value) > maxOpenTelemetryDimensionLength {
		return openTelemetryOtherDimensionValue
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') &&
			character != '.' && character != '_' && character != '-' {
			return openTelemetryOtherDimensionValue
		}
	}
	return value
}

func openTelemetryOutcome(outcome Outcome) string {
	switch outcome {
	case OutcomeSuccess, OutcomeRejected, OutcomeFailure:
		return string(outcome)
	default:
		return openTelemetryOtherDimensionValue
	}
}

func openTelemetryErrorCode(code crdt.ErrorCode) string {
	switch code {
	case crdt.ErrorCodeUnknown,
		crdt.ErrorCodeInvalidConfig,
		crdt.ErrorCodeInvalidInput,
		crdt.ErrorCodeUnauthorized,
		crdt.ErrorCodeConflict,
		crdt.ErrorCodeResourceLimit,
		crdt.ErrorCodeUnavailable:
		return string(code)
	default:
		return string(crdt.ErrorCodeUnknown)
	}
}
