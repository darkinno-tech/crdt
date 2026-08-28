package extensions

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/im10furry/crdt/counter"
	"github.com/im10furry/crdt/replica"
)

func TestChangeBatchWireRoundTripAndBounds(t *testing.T) {
	manifest := testManifest(t, "batch-wire")
	writer, err := counter.NewGCounter("writer")
	if err != nil {
		t.Fatal(err)
	}
	changes := make([]replica.Change, 0, 3)
	for sequence, amount := range []uint64{1, 2, 3} {
		changes = append(changes, incrementChange(t, writer, manifest, "writer", uint64(sequence+1), amount))
	}
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
		if decoded[index].dot != change.Dot || !bytes.Equal(decoded[index].delta, change.Delta()) {
			t.Fatalf("decoded change %d differs", index)
		}
	}
	if _, err := unmarshalChangeBatch(encoded, len(encoded), 128, len(changes)-1); !errors.Is(err, errInvalidWireMessage) {
		t.Fatalf("over-limit batch error = %v, want invalid wire", err)
	}
	for _, malformed := range [][]byte{
		nil,
		{batchTransportVersion, 0},
		append(append([]byte(nil), encoded...), 0),
	} {
		if _, err := unmarshalChangeBatch(malformed, 1024, 128, len(changes)); !errors.Is(err, errInvalidWireMessage) {
			t.Fatalf("malformed batch %x error = %v", malformed, err)
		}
	}
}

func TestChangeBatchMarshalRejectsInvalidInput(t *testing.T) {
	for _, changes := range [][]replica.Change{
		nil,
		{{}},
	} {
		if _, err := marshalChangeBatch(changes); !errors.Is(err, errInvalidWireMessage) {
			t.Fatalf("marshalChangeBatch(%#v) error = %v, want invalid wire", changes, err)
		}
	}
	for _, items := range [][][]byte{
		nil,
		{{}},
		{{batchTransportVersion}},
	} {
		if _, err := marshalEncodedChangeBatch(items); !errors.Is(err, errInvalidWireMessage) {
			t.Fatalf("marshalEncodedChangeBatch(%x) error = %v, want invalid wire", items, err)
		}
	}
}

func TestWebSocketBatchDeliversEachDotToV1AndV2Peers(t *testing.T) {
	server, group, manifest, relay := newCounterHandler(t, FeatureWebSocket|FeatureWebSocketBatch, nil)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	publisher := newBatchWebSocketCounterClient(t, endpoint, manifest, "writer", true)
	v1Observer := newBatchWebSocketCounterClient(t, endpoint, manifest, "v1-observer", false)
	v2Observer := newBatchWebSocketCounterClient(t, endpoint, manifest, "v2-observer", true)
	if !publisher.client.batchEnabled || v1Observer.client.batchEnabled || !v2Observer.client.batchEnabled {
		t.Fatal("unexpected WebSocket batch negotiation")
	}

	changes := make([]replica.Change, 0, 3)
	for sequence, amount := range []uint64{1, 2, 3} {
		changes = append(changes, incrementChange(t, publisher.state, manifest, "writer", uint64(sequence+1), amount))
	}
	if err := publisher.client.PublishBatch(context.Background(), changes); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		return counterValue(t, relay) == 6 &&
			counterValue(t, publisher.state) == 6 &&
			counterValue(t, v1Observer.state) == 6 &&
			counterValue(t, v2Observer.state) == 6 &&
			group.Frontier().Counter("writer") == 3 &&
			v1Observer.inbox.Frontier().Counter("writer") == 3 &&
			v2Observer.inbox.Frontier().Counter("writer") == 3
	})
}

func TestWebSocketBatchFallsBackToV1AndRejectsPublishBatch(t *testing.T) {
	server, _, manifest, _ := newCounterHandler(t, FeatureWebSocket, nil)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	client := newBatchWebSocketCounterClient(t, endpoint, manifest, "writer", true)
	if client.client.batchEnabled {
		t.Fatal("v1-only relay negotiated the batch subprotocol")
	}
	change := incrementChange(t, client.state, manifest, "writer", 1, 1)
	if err := client.client.PublishBatch(context.Background(), []replica.Change{change}); !errors.Is(err, ErrBatchUnsupported) {
		t.Fatalf("v1 fallback PublishBatch error = %v, want %v", err, ErrBatchUnsupported)
	}
}

func TestWebSocketBatchHonorsClientLimit(t *testing.T) {
	server, _, manifest, _ := newCounterHandler(t, FeatureWebSocket|FeatureWebSocketBatch, nil)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	state, inbox := newCounterInbox(t, manifest, "writer")
	client, err := DialWebSocket(context.Background(), endpoint, manifest, ClientConfig{
		Header:          bearerHeader("writer"),
		EnableBatches:   true,
		MaxBatchChanges: 1,
		OnChange: func(change replica.Change) error {
			_, err := inbox.Receive(change)
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	first := incrementChange(t, state, manifest, "writer", 1, 1)
	second := incrementChange(t, state, manifest, "writer", 2, 1)
	if err := client.PublishBatch(context.Background(), []replica.Change{first, second}); !errors.Is(err, ErrBatchLimit) {
		t.Fatalf("over-limit PublishBatch error = %v, want %v", err, ErrBatchLimit)
	}
}

func TestWebSocketBatchRejectsInvalidLocalInputBeforeWriting(t *testing.T) {
	manifest := testManifest(t, "batch-local-input")
	client := &WebSocketClient{
		manifest:        manifest,
		context:         context.Background(),
		batchEnabled:    true,
		maxBatchChanges: 1,
		maxMessageBytes: 1,
		maxActorBytes:   128,
	}
	if err := client.PublishBatch(context.Background(), nil); !errors.Is(err, ErrBatchLimit) {
		t.Fatalf("empty PublishBatch error = %v, want %v", err, ErrBatchLimit)
	}
	if err := client.PublishBatch(context.Background(), []replica.Change{{}}); err == nil {
		t.Fatal("invalid local change was written")
	}
	writer, err := counter.NewGCounter("writer")
	if err != nil {
		t.Fatal(err)
	}
	change := incrementChange(t, writer, manifest, "writer", 1, 1)
	if err := client.PublishBatch(context.Background(), []replica.Change{change}); !errors.Is(err, ErrBatchLimit) {
		t.Fatalf("oversized PublishBatch error = %v, want %v", err, ErrBatchLimit)
	}
	closedContext, cancel := context.WithCancel(context.Background())
	cancel()
	client.context = closedContext
	if err := client.PublishBatch(context.Background(), []replica.Change{change}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed PublishBatch error = %v, want %v", err, ErrClosed)
	}
}

func TestBatchPreflightRejectsWithoutMutation(t *testing.T) {
	_, group, manifest, _ := newCounterHandler(t, FeatureHTTP, nil)
	subscriber := &batchRecordingSubscriber{}
	group.add(subscriber)
	t.Cleanup(subscriber.close)
	first := rawReplicaChange(t, manifest, "writer", 1)
	forged := rawReplicaChange(t, manifest, "other", 1)
	encoded, err := marshalChangeBatch([]replica.Change{first, forged})
	if err != nil {
		t.Fatal(err)
	}
	if err := group.receiveBatch(Peer{ID: "writer"}, allowActor, encoded, 1024, 128, 2); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("preflight authorization error = %v, want %v", err, ErrUnauthorized)
	}
	if group.Frontier().Counter("writer") != 0 || len(subscriber.messages) != 0 {
		t.Fatal("batch preflight rejection mutated or broadcast")
	}
}

func TestBatchForwardsAcceptedPrefixWhenLaterAdmissionFails(t *testing.T) {
	manifest := testManifest(t, "batch-prefix")
	applied := 0
	group, err := NewGroup(GroupConfig{
		Manifest:          manifest,
		MaxPendingChanges: 8,
		MaxPendingBytes:   1024,
		Apply: func([]byte) error {
			applied++
			if applied == 2 {
				return errors.New("application rejected second item")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	subscriber := &batchRecordingSubscriber{}
	group.add(subscriber)
	t.Cleanup(subscriber.close)
	first := rawReplicaChange(t, manifest, "writer", 1)
	second := rawReplicaChange(t, manifest, "writer", 2)
	encoded, err := marshalChangeBatch([]replica.Change{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if err := group.receiveBatch(Peer{ID: "writer"}, allowActor, encoded, 1024, 128, 2); err == nil {
		t.Fatal("partial batch unexpectedly succeeded")
	}
	if got := group.Frontier().Counter("writer"); got != 1 {
		t.Fatalf("frontier after partial batch = %d, want 1", got)
	}
	if len(subscriber.messages) != 1 {
		t.Fatalf("accepted prefix broadcasts = %d, want 1", len(subscriber.messages))
	}
	dot, _, err := unmarshalChange(subscriber.messages[0], 1024, 128)
	if err != nil || dot != first.Dot {
		t.Fatalf("accepted prefix = %#v, %v", dot, err)
	}
	firstWire, err := marshalChange(first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := group.receive(Peer{ID: "writer"}, allowActor, firstWire, 1024, 128); err != nil {
		t.Fatal(err)
	}
	if len(subscriber.messages) != 1 {
		t.Fatal("duplicate retry re-broadcast an already forwarded prefix")
	}
}

func TestBatchFeatureRequiresWebSocketAndQueueCapacity(t *testing.T) {
	_, group, _, _ := newCounterHandler(t, FeatureHTTP, nil)
	config := Config{
		Features: FeatureWebSocket | FeatureWebSocketBatch,
		Groups:   []*Group{group},
		Authenticate: func(*http.Request) (Peer, error) {
			return Peer{ID: "writer"}, nil
		},
		Authorize:             allowActor,
		AuthorizeSubscription: func(Peer, replica.Manifest) error { return nil },
	}
	config.Features = FeatureWebSocketBatch
	if _, err := NewHandler(config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("batch without WebSocket error = %v, want ErrInvalidConfig", err)
	}
	config.Features = FeatureWebSocket | FeatureWebSocketBatch
	config.MaxQueuedMessages = defaultMaxQueuedMessages
	config.MaxBatchChanges = defaultMaxQueuedMessages + 1
	if _, err := NewHandler(config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("batch larger than legacy queue error = %v, want ErrInvalidConfig", err)
	}
	config.MaxQueuedMessages = maximumBatchChanges
	config.MaxBatchChanges = maximumBatchChanges + 1
	if _, err := NewHandler(config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("batch above absolute bound error = %v, want ErrInvalidConfig", err)
	}
}

func TestPeerQueueEnqueueAllIsAtomic(t *testing.T) {
	queue := newPeerQueue(2, 8)
	if !queue.enqueue([]byte{1, 2}) {
		t.Fatal("initial queue enqueue failed")
	}
	if queue.enqueueAll([][]byte{{3}, {4}}) {
		t.Fatal("queue accepted an all-or-nothing batch without enough slots")
	}
	if queue.queuedBytes.Load() != 2 {
		t.Fatalf("failed batch changed queued bytes to %d", queue.queuedBytes.Load())
	}
	data, ok := queue.dequeue()
	if !ok || string(data) != string([]byte{1, 2}) {
		t.Fatalf("queue retained wrong message %x, %t", data, ok)
	}
}

func TestSSESubscriberQueuesBatchOrCloses(t *testing.T) {
	subscriber := newSSESubscriber(2, 8)
	if !subscriber.enqueueAll([][]byte{{1}, {2}}) {
		t.Fatal("SSE subscriber rejected a bounded batch")
	}
	for _, want := range [][]byte{{1}, {2}} {
		got, ok := subscriber.dequeue()
		if !ok || string(got) != string(want) {
			t.Fatalf("SSE batch item = %x, %t; want %x", got, ok, want)
		}
	}
	subscriber.close()
	if subscriber.enqueueAll([][]byte{{3}}) {
		t.Fatal("closed SSE subscriber accepted a batch")
	}
}

func newBatchWebSocketCounterClient(t *testing.T, endpoint string, manifest replica.Manifest, actor string, enableBatches bool) webSocketCounterClient {
	t.Helper()
	state, inbox := newCounterInbox(t, manifest, actor)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := DialWebSocket(ctx, endpoint, manifest, ClientConfig{
		Header:        bearerHeader(actor),
		EnableBatches: enableBatches,
		OnChange: func(change replica.Change) error {
			_, err := inbox.Receive(change)
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return webSocketCounterClient{client: client, state: state, inbox: inbox}
}

type batchRecordingSubscriber struct {
	messages [][]byte
	closed   bool
}

func (s *batchRecordingSubscriber) enqueue(data []byte) bool {
	if s == nil || s.closed {
		return false
	}
	s.messages = append(s.messages, append([]byte(nil), data...))
	return true
}

func (s *batchRecordingSubscriber) enqueueAll(data [][]byte) bool {
	if s == nil || s.closed {
		return false
	}
	for _, item := range data {
		s.messages = append(s.messages, append([]byte(nil), item...))
	}
	return true
}

func (s *batchRecordingSubscriber) close() {
	if s != nil {
		s.closed = true
	}
}
