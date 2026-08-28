package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darkinno-tech/crdt/counter"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/replica"
	"github.com/coder/websocket"
)

func TestChangeBatchWireRoundTripAndLimits(t *testing.T) {
	_, _, manifest, _ := newCounterServer(t)
	changes := batchCounterChanges(t, manifest, "operator-a")
	encoded, err := marshalChangeBatch(changes)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := unmarshalChangeBatch(encoded, len(encoded), 128, len(changes))
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != len(changes) {
		t.Fatalf("decoded changes = %d, want %d", len(decoded), len(changes))
	}
	for index, change := range changes {
		if decoded[index].dot != change.Dot {
			t.Fatalf("decoded dot %d = %#v, want %#v", index, decoded[index].dot, change.Dot)
		}
		if got, want := decoded[index].delta, change.Delta(); string(got) != string(want) {
			t.Fatalf("decoded delta %d differs", index)
		}
	}
	if _, err := unmarshalChangeBatch(encoded, len(encoded), 128, len(changes)-1); !errors.Is(err, errInvalidWireMessage) {
		t.Fatalf("batch over limit error = %v, want invalid wire", err)
	}
	if _, err := unmarshalChangeBatch([]byte{batchProtocolVersion, 0}, 1024, 128, 1); !errors.Is(err, errInvalidWireMessage) {
		t.Fatalf("empty batch error = %v, want invalid wire", err)
	}
}

func TestPublishBatchDeliversEveryDotToV1AndV2Peers(t *testing.T) {
	server, _, manifest, relay := newCounterServer(t)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	publisher, err := Dial(context.Background(), endpoint, manifest, ClientConfig{
		Header:        http.Header{"Authorization": []string{"Bearer operator-a"}},
		EnableBatches: true,
		OnChange:      func(replica.Change) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = publisher.Close() })

	_, v1State, v1Changes := newBatchObserver(t, endpoint, manifest, "operator-v1", false)
	_, v2State, v2Changes := newBatchObserver(t, endpoint, manifest, "operator-v2", true)

	changes := batchCounterChanges(t, manifest, "operator-a")
	if err := publisher.PublishBatch(context.Background(), changes); err != nil {
		t.Fatal(err)
	}
	awaitBatchDots(t, v1Changes, changes)
	awaitBatchDots(t, v2Changes, changes)
	eventually(t, func() bool {
		return counterValue(t, relay) == 6 && counterValue(t, v1State) == 6 && counterValue(t, v2State) == 6
	})
}

func TestPublishBatchRequiresV2AndHonorsLimit(t *testing.T) {
	server, _, manifest, _ := newCounterServer(t)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	changes := batchCounterChanges(t, manifest, "operator-a")

	v1, err := Dial(context.Background(), endpoint, manifest, ClientConfig{
		Header:   http.Header{"Authorization": []string{"Bearer operator-a"}},
		OnChange: func(replica.Change) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v1.Close() })
	if err := v1.PublishBatch(context.Background(), changes); !errors.Is(err, ErrBatchUnsupported) {
		t.Fatalf("v1 PublishBatch error = %v, want %v", err, ErrBatchUnsupported)
	}

	v2, err := Dial(context.Background(), endpoint, manifest, ClientConfig{
		Header:          http.Header{"Authorization": []string{"Bearer operator-a"}},
		EnableBatches:   true,
		MaxBatchChanges: 1,
		OnChange:        func(replica.Change) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v2.Close() })
	if err := v2.PublishBatch(context.Background(), changes); !errors.Is(err, ErrBatchLimit) {
		t.Fatalf("over-limit PublishBatch error = %v, want %v", err, ErrBatchLimit)
	}
}

func TestBatchEnabledClientFallsBackToLegacyV1Server(t *testing.T) {
	_, _, manifest, _ := newCounterServer(t)
	legacy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
			Subprotocols:    []string{Subprotocol},
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			return
		}
		defer func() { _ = connection.CloseNow() }()
		messageType, data, err := connection.Read(context.Background())
		if err != nil || messageType != websocket.MessageText {
			return
		}
		remote, err := unmarshalHello(data)
		if err != nil {
			return
		}
		hello, err := marshalHello(remote)
		if err != nil {
			return
		}
		if err := connection.Write(context.Background(), websocket.MessageText, hello); err != nil {
			return
		}
	}))
	t.Cleanup(legacy.Close)
	endpoint := "ws" + strings.TrimPrefix(legacy.URL, "http")
	client, err := Dial(context.Background(), endpoint, manifest, ClientConfig{
		EnableBatches: true,
		OnChange:      func(replica.Change) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if client.batchEnabled {
		t.Fatal("legacy server negotiated batch transport")
	}
	if err := client.PublishBatch(context.Background(), batchCounterChanges(t, manifest, "operator-a")); !errors.Is(err, ErrBatchUnsupported) {
		t.Fatalf("legacy fallback PublishBatch error = %v, want %v", err, ErrBatchUnsupported)
	}
}

func batchCounterChanges(t *testing.T, manifest replica.Manifest, actor string) []replica.Change {
	t.Helper()
	writer, err := counter.NewGCounter(actor)
	if err != nil {
		t.Fatal(err)
	}
	changes := make([]replica.Change, 0, 3)
	for index, increment := range []uint64{1, 2, 3} {
		delta, err := writer.Increment(increment)
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, newCounterChange(t, manifest, actor, uint64(index+1), delta))
	}
	return changes
}

func newBatchObserver(t *testing.T, endpoint string, manifest replica.Manifest, actor string, enableBatches bool) (*Client, *counter.GCounter, <-chan replica.Change) {
	t.Helper()
	state, err := counter.NewGCounter(actor)
	if err != nil {
		t.Fatal(err)
	}
	frontier, err := replica.NewFrontier(nil)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := replica.NewInbox(manifest, frontier, 8, 16<<10, func(data []byte) error {
		delta, err := counter.UnmarshalGCounterDeltaWithLimits(data, frame.DefaultLimits())
		if err != nil {
			return err
		}
		return state.ApplyDelta(delta)
	})
	if err != nil {
		t.Fatal(err)
	}
	changes := make(chan replica.Change, 3)
	client, err := Dial(context.Background(), endpoint, manifest, ClientConfig{
		Header:        http.Header{"Authorization": []string{"Bearer " + actor}},
		EnableBatches: enableBatches,
		OnChange: func(change replica.Change) error {
			if _, err := inbox.Receive(change); err != nil {
				return err
			}
			changes <- change
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, state, changes
}

func awaitBatchDots(t *testing.T, received <-chan replica.Change, want []replica.Change) {
	t.Helper()
	for index, expected := range want {
		select {
		case actual := <-received:
			if actual.Dot != expected.Dot {
				t.Fatalf("received dot %d = %#v, want %#v", index, actual.Dot, expected.Dot)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for dot %#v", expected.Dot)
		}
	}
}
