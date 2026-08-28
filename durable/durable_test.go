package durable

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/counter"
	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/replica"
	"github.com/coder/websocket"
)

func TestStorePersistsExactDotBindingAndReplay(t *testing.T) {
	manifest := durableTestManifest(t)
	path := t.TempDir() + "/relay.db"
	store := durableTestStore(t, path, 16, 1<<20)
	first := durableTestChange(t, manifest, "alice", 1, 2)
	appended, err := store.Append(manifest.GroupID, first)
	if err != nil {
		t.Fatalf("append first: %v", err)
	}
	if appended.Duplicate || appended.Event.Sequence != 1 {
		t.Fatalf("first append = %+v, want sequence 1", appended)
	}
	retried, err := store.Append(manifest.GroupID, first)
	if err != nil {
		t.Fatalf("retry first: %v", err)
	}
	if !retried.Duplicate || retried.Event.Sequence != appended.Event.Sequence {
		t.Fatalf("retry = %+v, want duplicate sequence %d", retried, appended.Event.Sequence)
	}
	conflict := durableTestChange(t, manifest, "alice", 1, 9)
	if _, err := store.Append(manifest.GroupID, conflict); !errors.Is(err, ErrConflictingDot) {
		t.Fatalf("conflicting retry error = %v, want %v", err, ErrConflictingDot)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}
	store = durableTestStore(t, path, 16, 1<<20)
	defer func() { _ = store.Close() }()
	events, highWater, err := store.Replay(manifest.GroupID, 0, 16, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
	if err != nil {
		t.Fatalf("replay reopened store: %v", err)
	}
	if highWater != 1 || len(events) != 1 || events[0].Sequence != 1 || events[0].Change.Dot != first.Dot {
		t.Fatalf("replay = high %d events %+v, want first event", highWater, events)
	}
	if got, want := events[0].Change.Delta(), first.Delta(); string(got) != string(want) {
		t.Fatalf("replayed delta differs")
	}
}

func TestStoreRejectsPartialReplayAndRetentionOverflow(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 2, 1<<20)
	defer func() { _ = store.Close() }()
	for sequence := uint64(1); sequence <= 2; sequence++ {
		if _, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "alice", sequence, sequence)); err != nil {
			t.Fatalf("append %d: %v", sequence, err)
		}
	}
	if _, _, err := store.Replay(manifest.GroupID, 0, 1, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrReplayUnavailable) {
		t.Fatalf("short replay error = %v, want %v", err, ErrReplayUnavailable)
	}
	if _, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "alice", 3, 3)); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("overflow append error = %v, want %v", err, ErrStoreFull)
	}
	if _, _, err := store.Replay(manifest.GroupID, 3, 2, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrReplayUnavailable) {
		t.Fatalf("future cursor error = %v, want %v", err, ErrReplayUnavailable)
	}
}

func TestDurableServerRestartsAndReplaysCommittedEvent(t *testing.T) {
	manifest := durableTestManifest(t)
	path := t.TempDir() + "/relay.db"
	store := durableTestStore(t, path, 32, 1<<20)
	handler, _ := durableTestHandler(t, store, manifest)
	server := httptest.NewServer(handler)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	firstEvents := make(chan Event, 2)
	client := durableTestClient(t, endpoint, manifest, 0, func(event Event) error {
		firstEvents <- event
		return nil
	})
	stop := runDurableClient(t, client)
	change := durableTestChange(t, manifest, "alice", 1, 2)
	if err := client.Publish(context.Background(), change); err != nil {
		t.Fatalf("publish first event: %v", err)
	}
	first := awaitDurableEvent(t, firstEvents)
	if first.Sequence != 1 || first.Change.Dot != change.Dot {
		t.Fatalf("first event = %+v", first)
	}
	stop()
	server.Close()
	if err := store.Close(); err != nil {
		t.Fatalf("close store before restart: %v", err)
	}

	store = durableTestStore(t, path, 32, 1<<20)
	defer func() { _ = store.Close() }()
	handler, _ = durableTestHandler(t, store, manifest)
	server = httptest.NewServer(handler)
	defer server.Close()
	endpoint = "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	replayedEvents := make(chan Event, 2)
	replayedClient := durableTestClient(t, endpoint, manifest, 0, func(event Event) error {
		replayedEvents <- event
		return nil
	})
	stopReplay := runDurableClient(t, replayedClient)
	defer stopReplay()
	replayed := awaitDurableEvent(t, replayedEvents)
	if replayed.Sequence != 1 || replayed.Change.Dot != change.Dot {
		t.Fatalf("replayed event = %+v", replayed)
	}
}

func TestReconnectClientReplaysAfterForcedPeerDrop(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 32, 1<<20)
	defer func() { _ = store.Close() }()
	handler, group := durableTestHandler(t, store, manifest)
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	events := make(chan Event, 2)
	client := durableTestClient(t, endpoint, manifest, 0, func(event Event) error {
		events <- event
		return nil
	})
	client.limits.minBackoff = 300 * time.Millisecond
	client.limits.maxBackoff = 300 * time.Millisecond
	stop := runDurableClient(t, client)
	defer stop()
	waitUntil(t, func() bool {
		group.mu.Lock()
		defer group.mu.Unlock()
		return len(group.peers) == 1
	})
	group.mu.Lock()
	for peer := range group.peers {
		peer.close()
	}
	group.mu.Unlock()

	publisher, closePublisher := durableRawClient(t, endpoint, manifest, 0, "alice")
	defer closePublisher()
	change := durableTestChange(t, manifest, "alice", 1, 3)
	encoded, err := marshalChange(change)
	if err != nil {
		t.Fatalf("marshal raw publish: %v", err)
	}
	if err := publisher.Write(context.Background(), websocket.MessageBinary, encoded); err != nil {
		t.Fatalf("raw publish: %v", err)
	}
	waitUntil(t, func() bool {
		_, highWater, err := store.Replay(manifest.GroupID, 0, 32, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
		return err == nil && highWater == 1
	})
	replayed := awaitDurableEvent(t, events)
	if replayed.Sequence != 1 || replayed.Change.Dot != change.Dot {
		t.Fatalf("reconnected replay = %+v", replayed)
	}
	// OnEvent exposes the event before it returns. The client advances its
	// cursor only after that callback has completed, so wait for that documented
	// ordering rather than racing the callback return.
	waitUntil(t, func() bool { return client.Cursor() == replayed.Sequence })
}

func TestOutOfOrderActorDotsConvergeThroughDurableReplay(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 32, 1<<20)
	defer func() { _ = store.Close() }()
	handler, _ := durableTestHandler(t, store, manifest)
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	target, err := counter.NewGCounter("observer")
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := replica.NewInbox(manifest, replica.Frontier{}, 8, 1<<20, func(data []byte) error {
		delta, err := counter.UnmarshalGCounterDeltaWithLimits(data, frame.DefaultLimits())
		if err != nil {
			return err
		}
		return target.ApplyDelta(delta)
	})
	if err != nil {
		t.Fatal(err)
	}
	installed := make(chan Event, 3)
	client := durableTestClient(t, endpoint, manifest, 0, func(event Event) error {
		if _, err := inbox.Receive(event.Change); err != nil {
			return err
		}
		installed <- event
		return nil
	})
	stop := runDurableClient(t, client)
	defer stop()
	publisher, closePublisher := durableRawClient(t, endpoint, manifest, 0, "alice")
	defer closePublisher()
	source, err := counter.NewGCounter("alice")
	if err != nil {
		t.Fatal(err)
	}
	firstDelta, err := source.Increment(2)
	if err != nil {
		t.Fatal(err)
	}
	secondDelta, err := source.Increment(3)
	if err != nil {
		t.Fatal(err)
	}
	first, err := durableCounterChange(manifest, "alice", 1, firstDelta)
	if err != nil {
		t.Fatal(err)
	}
	second, err := durableCounterChange(manifest, "alice", 2, secondDelta)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []replica.Change{second, first} {
		encoded, err := marshalChange(change)
		if err != nil {
			t.Fatal(err)
		}
		if err := publisher.Write(context.Background(), websocket.MessageBinary, encoded); err != nil {
			t.Fatal(err)
		}
	}
	_ = awaitDurableEvent(t, installed)
	_ = awaitDurableEvent(t, installed)
	value, err := target.Value()
	if err != nil {
		t.Fatal(err)
	}
	if value != 5 || inbox.Frontier().Counter("alice") != 2 {
		t.Fatalf("out-of-order delivery value=%d frontier=%d", value, inbox.Frontier().Counter("alice"))
	}
}

// TestThreeReplicaPartitionRecoverySimulation models a mobile replica losing
// its live socket while another replica commits a reversed actor sequence.
// Recovery must obtain the durable suffix, not rely on the dropped live queue.
func TestThreeReplicaPartitionRecoverySimulation(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 32, 1<<20)
	defer func() { _ = store.Close() }()
	handler, group := durableTestHandler(t, store, manifest)
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	offlineState, err := counter.NewGCounter("offline")
	if err != nil {
		t.Fatal(err)
	}
	offlineInbox, err := replica.NewInbox(manifest, replica.Frontier{}, 8, 1<<20, func(data []byte) error {
		delta, err := counter.UnmarshalGCounterDeltaWithLimits(data, frame.DefaultLimits())
		if err != nil {
			return err
		}
		return offlineState.ApplyDelta(delta)
	})
	if err != nil {
		t.Fatal(err)
	}
	installed := make(chan Event, 3)
	offline := durableTestClient(t, endpoint, manifest, 0, func(event Event) error {
		if _, err := offlineInbox.Receive(event.Change); err != nil {
			return err
		}
		installed <- event
		return nil
	})
	offline.limits.minBackoff = 300 * time.Millisecond
	offline.limits.maxBackoff = 300 * time.Millisecond
	stop := runDurableClient(t, offline)
	defer stop()
	waitUntil(t, func() bool {
		group.mu.Lock()
		defer group.mu.Unlock()
		return len(group.peers) == 1
	})
	group.mu.Lock()
	for peer := range group.peers {
		peer.close()
	}
	group.mu.Unlock()

	publisher, closePublisher := durableRawClient(t, endpoint, manifest, 0, "alice")
	defer closePublisher()
	source, err := counter.NewGCounter("alice")
	if err != nil {
		t.Fatal(err)
	}
	firstDelta, err := source.Increment(2)
	if err != nil {
		t.Fatal(err)
	}
	secondDelta, err := source.Increment(3)
	if err != nil {
		t.Fatal(err)
	}
	first, err := durableCounterChange(manifest, "alice", 1, firstDelta)
	if err != nil {
		t.Fatal(err)
	}
	second, err := durableCounterChange(manifest, "alice", 2, secondDelta)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []replica.Change{second, first} {
		if err := publisher.Write(context.Background(), websocket.MessageBinary, mustMarshalChange(t, change)); err != nil {
			t.Fatal(err)
		}
	}
	_ = awaitDurableEvent(t, installed)
	_ = awaitDurableEvent(t, installed)
	// Receiving the callback notification does not imply the callback has
	// returned. The resume cursor advances only after that return.
	waitUntil(t, func() bool { return offline.Cursor() == 2 })
	value, err := offlineState.Value()
	if err != nil {
		t.Fatal(err)
	}
	if value != 5 || offlineInbox.Frontier().Counter("alice") != 2 {
		t.Fatalf("partition recovery value=%d frontier=%d cursor=%d", value, offlineInbox.Frontier().Counter("alice"), offline.Cursor())
	}
}

func TestServerRejectsInvalidDeltaBeforeDurableAppend(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 8, 1<<20)
	defer func() { _ = store.Close() }()
	handler, _ := durableTestHandler(t, store, manifest)
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	publisher, closePublisher := durableRawClient(t, endpoint, manifest, 0, "alice")
	defer closePublisher()
	if err := publisher.Write(context.Background(), websocket.MessageBinary, []byte{changeMessage, 1, 'a', 1, 1, 0}); err != nil {
		t.Fatalf("write malformed change: %v", err)
	}
	waitUntil(t, func() bool {
		_, highWater, err := store.Replay(manifest.GroupID, 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
		return err == nil && highWater == 0
	})
}

func durableTestManifest(t *testing.T) replica.Manifest {
	t.Helper()
	manifest, err := replica.NewManifest("durable-counter", "example.com/durable-counter/v1", 1, replica.Protocol{
		StateID:          crdt.TypeIDGCounterState,
		DeltaID:          crdt.TypeIDGCounterDelta,
		SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func durableTestStore(t *testing.T, path string, maxEvents, maxBytes uint64) *Store {
	t.Helper()
	store, err := OpenStore(path, StoreConfig{MaxEvents: maxEvents, MaxBytes: maxBytes})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func durableTestHandler(t *testing.T, store *Store, manifest replica.Manifest) (*Handler, *Group) {
	t.Helper()
	group, err := NewGroup(GroupConfig{
		Manifest: manifest,
		Validate: func(data []byte) error {
			_, err := counter.UnmarshalGCounterDeltaWithLimits(data, frame.DefaultLimits())
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(Config{
		Store:  store,
		Groups: []*Group{group},
		Authenticate: func(request *http.Request) (Peer, error) {
			const prefix = "Bearer "
			value := request.Header.Get("Authorization")
			if !strings.HasPrefix(value, prefix) || strings.TrimPrefix(value, prefix) == "" {
				return Peer{}, ErrUnauthorized
			}
			return Peer{ID: strings.TrimPrefix(value, prefix)}, nil
		},
		Authorize: func(peer Peer, _ replica.Manifest, dot replica.Dot) error {
			if peer.ID != dot.Actor {
				return ErrUnauthorized
			}
			return nil
		},
		AuthorizeSubscription: func(Peer, replica.Manifest) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, group
}

func durableTestClient(t *testing.T, endpoint string, manifest replica.Manifest, cursor uint64, onEvent func(Event) error) *ReconnectClient {
	t.Helper()
	client, err := NewReconnectClient(endpoint, manifest, ClientConfig{
		Header:  http.Header{"Authorization": []string{"Bearer alice"}},
		Cursor:  cursor,
		OnEvent: onEvent,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func durableRawClient(t *testing.T, endpoint string, manifest replica.Manifest, resume uint64, actor string) (*websocket.Conn, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + actor}},
		Subprotocols: []string{Subprotocol},
	})
	if err != nil {
		t.Fatal(err)
	}
	hello, err := marshalHello(manifest, resume)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	messageType, data, err := connection.Read(ctx)
	if err != nil || messageType != websocket.MessageText {
		t.Fatalf("read welcome: type=%v data=%q error=%v", messageType, data, err)
	}
	remote, _, err := unmarshalWelcome(data)
	if err != nil || manifest.Compatible(remote) != nil {
		t.Fatalf("invalid welcome: %v", err)
	}
	return connection, func() { _ = connection.CloseNow() }
}

func durableTestChange(t *testing.T, manifest replica.Manifest, actor string, sequence, amount uint64) replica.Change {
	t.Helper()
	state, err := counter.NewGCounter(actor)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := state.Increment(amount)
	if err != nil {
		t.Fatal(err)
	}
	change, err := durableCounterChange(manifest, actor, sequence, delta)
	if err != nil {
		t.Fatal(err)
	}
	return change
}

func durableCounterChange(manifest replica.Manifest, actor string, sequence uint64, delta counter.GCounterDelta) (replica.Change, error) {
	encoded, err := delta.MarshalBinary()
	if err != nil {
		return replica.Change{}, err
	}
	return replica.NewChange(manifest, replica.Dot{Actor: actor, Counter: sequence}, encoded)
}

func runDurableClient(t *testing.T, client *ReconnectClient) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("client run: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("client did not stop")
		}
	}
}

func awaitDurableEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for durable event")
		return Event{}
	}
}

func waitUntil(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
