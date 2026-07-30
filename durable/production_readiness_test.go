package durable

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/telemetry"
	"github.com/coder/websocket"
)

func TestPublicConfigurationFailuresHaveStructuredErrors(t *testing.T) {
	_, err := OpenStore("", StoreConfig{MaxEvents: 1, MaxBytes: 1})
	assertStructuredConfigError(t, err, "durable.open_store")

	manifest := durableTestManifest(t)
	_, err = NewGroup(GroupConfig{Manifest: manifest})
	assertStructuredConfigError(t, err, "durable.new_group")

	_, err = NewReconnectClient("https://relay.example/ws", manifest, ClientConfig{OnEvent: func(Event) error { return nil }})
	assertStructuredConfigError(t, err, "durable.new_reconnect_client")
}

func TestDurableRelayReportsRealHandshakeReplayAndAppend(t *testing.T) {
	events := make(chan telemetry.Event, 8)
	reporter, err := telemetry.New(telemetry.Options{QueueSize: 8, Sink: func(event telemetry.Event) {
		events <- event
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()

	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 8, 1<<20)
	defer func() { _ = store.Close() }()
	handler, group := durableTestHandler(t, store, manifest)
	handler.telemetry = reporter
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	connection, _, err := websocket.Dial(context.Background(), endpoint, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer alice"}},
		Subprotocols: []string{Subprotocol},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.CloseNow() }()
	hello, err := marshalHello(group.manifest, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	if _, _, err := connection.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	change := durableTestChange(t, manifest, "alice", 1, 1)
	encoded, err := marshalChange(change)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(context.Background(), websocket.MessageBinary, encoded); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{"handshake": false, "replay": false, "append": false}
	deadline := time.After(time.Second)
	for len(want) > 0 {
		select {
		case event := <-events:
			if event.Component != "durable" || event.Outcome != telemetry.OutcomeSuccess {
				t.Fatalf("event = %+v, want successful durable event", event)
			}
			if _, ok := want[event.Operation]; ok {
				delete(want, event.Operation)
			}
		case <-deadline:
			t.Fatalf("missing real relay telemetry operations: %v", want)
		}
	}
}

func TestDurableTelemetryClassifiesRejectedAndUnavailableFailures(t *testing.T) {
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
		{ErrStoreFull, telemetry.OutcomeRejected, crdt.ErrorCodeResourceLimit},
		{ErrClosed, telemetry.OutcomeFailure, crdt.ErrorCodeUnavailable},
		{ErrReplayUnavailable, telemetry.OutcomeFailure, crdt.ErrorCodeUnavailable},
		{errInvalidWire, telemetry.OutcomeRejected, crdt.ErrorCodeInvalidInput},
		{errors.New("dependency failed"), telemetry.OutcomeFailure, crdt.ErrorCodeUnknown},
	} {
		handler.record("append", handler.started(), test.err)
		event := <-events
		if event.Outcome != test.outcome || event.ErrorCode != test.errorCode {
			t.Fatalf("record(%v) = %+v, want outcome=%q code=%q", test.err, event, test.outcome, test.errorCode)
		}
	}
}

func assertStructuredConfigError(t *testing.T, err error, operation string) {
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
