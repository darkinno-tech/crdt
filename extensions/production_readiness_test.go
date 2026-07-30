package extensions

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/counter"
	"github.com/DarkInno/crdt/telemetry"
)

func TestExtensionsConfigurationFailuresHaveStructuredErrors(t *testing.T) {
	_, err := NewHandler(Config{Features: Feature(1 << 7)})
	assertExtensionsConfigError(t, err, "extensions.new_handler")

	_, err = NewGroup(GroupConfig{})
	assertExtensionsConfigError(t, err, "extensions.new_group")

	handler := &Handler{}
	err = handler.Mount(nil, "/")
	assertExtensionsConfigError(t, err, "extensions.mount")
}

func TestExtensionsReportsRealWebSocketHandshakeAndAppend(t *testing.T) {
	events := make(chan telemetry.Event, 8)
	reporter, err := telemetry.New(telemetry.Options{QueueSize: 8, Sink: func(event telemetry.Event) { events <- event }})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()

	server, _, manifest, _ := newCounterHandler(t, FeatureWebSocket, func(config *Config) {
		config.Telemetry = reporter
	})
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	client := newWebSocketCounterClient(t, endpoint, manifest, "operator-a")
	change := incrementChange(t, client.state, manifest, "operator-a", 1, 3)
	if err := client.client.Publish(context.Background(), change); err != nil {
		t.Fatal(err)
	}

	want := map[string]struct{}{"handshake": {}, "append": {}}
	deadline := time.After(time.Second)
	for len(want) > 0 {
		select {
		case event := <-events:
			if event.Component != "extensions" || event.Outcome != telemetry.OutcomeSuccess {
				t.Fatalf("event = %+v, want successful extensions event", event)
			}
			delete(want, event.Operation)
		case <-deadline:
			t.Fatalf("missing real WebSocket telemetry operations: %v", want)
		}
	}
}

func TestExtensionsTelemetryClassifiesRejectedAndUnavailableFailures(t *testing.T) {
	events := make(chan telemetry.Event, 8)
	reporter, err := telemetry.New(telemetry.Options{QueueSize: 8, Sink: func(event telemetry.Event) { events <- event }})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	handler := &Handler{telemetry: reporter}
	for _, test := range []struct {
		err       error
		outcome   telemetry.Outcome
		errorCode crdt.ErrorCode
	}{
		{ErrUnauthorized, telemetry.OutcomeRejected, crdt.ErrorCodeUnauthorized},
		{ErrBatchLimit, telemetry.OutcomeRejected, crdt.ErrorCodeResourceLimit},
		{ErrClosed, telemetry.OutcomeFailure, crdt.ErrorCodeUnavailable},
		{errInvalidWireMessage, telemetry.OutcomeRejected, crdt.ErrorCodeInvalidInput},
		{errors.New("dependency failed"), telemetry.OutcomeFailure, crdt.ErrorCodeUnknown},
	} {
		handler.record("append", handler.started(), test.err)
		event := <-events
		if event.Outcome != test.outcome || event.ErrorCode != test.errorCode {
			t.Fatalf("record(%v) = %+v, want outcome=%q code=%q", test.err, event, test.outcome, test.errorCode)
		}
	}
}

func assertExtensionsConfigError(t *testing.T, err error, operation string) {
	t.Helper()
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("errors.Is(%v, ErrInvalidConfig) = false", err)
	}
	if crdt.ErrorCodeOf(err) != crdt.ErrorCodeInvalidConfig {
		t.Fatalf("ErrorCodeOf(%v) = %q", err, crdt.ErrorCodeOf(err))
	}
	var structured *crdt.Error
	if !errors.As(err, &structured) || structured.Operation != operation {
		t.Fatalf("structured error = %#v, want operation %q", structured, operation)
	}
}

func TestHTTPAppendTelemetryPreservesTransportResult(t *testing.T) {
	events := make(chan telemetry.Event, 2)
	reporter, err := telemetry.New(telemetry.Options{QueueSize: 2, Sink: func(event telemetry.Event) { events <- event }})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()

	server, _, manifest, relay := newCounterHandler(t, FeatureHTTP, func(config *Config) {
		config.Telemetry = reporter
	})
	writer, err := counter.NewGCounter("operator-a")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := writer.Increment(4)
	if err != nil {
		t.Fatal(err)
	}
	change := newCounterChange(t, manifest, "operator-a", 1, delta)
	encoded, err := marshalChange(change)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := httpGroupURL(server.URL, manifest.GroupID, "changes")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer operator-a")
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	if got := counterValue(t, relay); got != 4 {
		t.Fatalf("relay value = %d, want 4", got)
	}
	select {
	case event := <-events:
		if event.Operation != "append" || event.Outcome != telemetry.OutcomeSuccess {
			t.Fatalf("HTTP event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP append telemetry was not delivered")
	}
}
