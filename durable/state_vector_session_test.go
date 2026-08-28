package durable

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/replica"
	bolt "go.etcd.io/bbolt"
)

func TestStoreCatchUpFiltersFrontierAndRebuildsActorIndex(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 16, 1<<20)
	defer func() { _ = store.Close() }()
	for _, change := range []replica.Change{
		durableTestChange(t, manifest, "alice", 1, 1),
		durableTestChange(t, manifest, "bob", 1, 1),
		durableTestChange(t, manifest, "alice", 2, 2),
	} {
		if _, err := store.Append(manifest.GroupID, change); err != nil {
			t.Fatal(err)
		}
	}
	frontier, err := replica.NewFrontier(map[string]uint64{"alice": 1})
	if err != nil {
		t.Fatal(err)
	}
	events, highWater, err := store.CatchUp(manifest.GroupID, frontier, 16, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
	if err != nil || highWater != 3 || len(events) != 2 || events[0].Sequence != 2 || events[0].Change.Dot != (replica.Dot{Actor: "bob", Counter: 1}) || events[1].Sequence != 3 || events[1].Change.Dot != (replica.Dot{Actor: "alice", Counter: 2}) {
		t.Fatalf("catch-up high=%d events=%+v err=%v", highWater, events, err)
	}
	if _, _, err := store.CatchUp(manifest.GroupID, frontier, 1, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrReplayUnavailable) {
		t.Fatalf("limited catch-up = %v", err)
	}
	ahead, err := replica.NewFrontier(map[string]uint64{"alice": 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CatchUp(manifest.GroupID, ahead, 16, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrReplayUnavailable) {
		t.Fatalf("ahead frontier catch-up = %v", err)
	}
	if err := store.db.Update(func(transaction *bolt.Tx) error {
		group, err := store.groupBucket(transaction, manifest.GroupID, false)
		if err != nil {
			return err
		}
		if err := group.DeleteBucket(bucketActors); err != nil {
			return err
		}
		return group.Bucket(bucketMeta).Delete(keyActorIndex)
	}); err != nil {
		t.Fatal(err)
	}
	events, highWater, err = store.CatchUp(manifest.GroupID, frontier, 16, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
	if err != nil || highWater != 3 || len(events) != 2 || events[0].Sequence != 2 || events[1].Sequence != 3 {
		t.Fatalf("rebuilt-index catch-up high=%d events=%+v err=%v", highWater, events, err)
	}
}

func TestStateVectorCatchUpConvergesOutOfOrderActorDots(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 8, 1<<20)
	defer func() { _ = store.Close() }()
	for _, change := range []replica.Change{
		durableTestChange(t, manifest, "alice", 2, 2),
		durableTestChange(t, manifest, "alice", 1, 1),
	} {
		if _, err := store.Append(manifest.GroupID, change); err != nil {
			t.Fatal(err)
		}
	}
	events, highWater, err := store.CatchUp(manifest.GroupID, replica.Frontier{}, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
	if err != nil || highWater != 2 || len(events) != 2 || events[0].Change.Dot.Counter != 2 || events[1].Change.Dot.Counter != 1 {
		t.Fatalf("out-of-order catch-up high=%d events=%+v err=%v", highWater, events, err)
	}
	inbox, err := replica.NewInbox(manifest, replica.Frontier{}, 4, 1<<20, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if _, err := inbox.Receive(event.Change); err != nil {
			t.Fatal(err)
		}
	}
	if got := inbox.Frontier().Counter("alice"); got != 2 {
		t.Fatalf("frontier alice=%d", got)
	}
	if pending, _ := inbox.Pending(); pending != 0 {
		t.Fatalf("pending=%d", pending)
	}
	frontier := inbox.Frontier()
	if events, highWater, err := store.CatchUp(manifest.GroupID, frontier, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); err != nil || highWater != 2 || len(events) != 0 {
		t.Fatalf("converged catch-up high=%d events=%+v err=%v", highWater, events, err)
	}
}

func TestReconnectClientStateVectorCatchUpAndForcedReconnect(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 16, 1<<20)
	defer func() { _ = store.Close() }()
	for _, change := range []replica.Change{
		durableTestChange(t, manifest, "alice", 1, 1),
		durableTestChange(t, manifest, "bob", 1, 1),
		durableTestChange(t, manifest, "alice", 2, 2),
	} {
		if _, err := store.Append(manifest.GroupID, change); err != nil {
			t.Fatal(err)
		}
	}
	handler, group := durableTestHandler(t, store, manifest)
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	frontier, err := replica.NewFrontier(map[string]uint64{"alice": 1})
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := replica.NewInbox(manifest, frontier, 8, 1<<20, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 4)
	catchUps := make(chan uint64, 4)
	client, err := NewReconnectClient(endpoint, manifest, ClientConfig{
		Header:      http.Header{"Authorization": []string{"Bearer alice"}},
		StateVector: inbox.Frontier,
		OnCatchUp:   func(highWater uint64) error { catchUps <- highWater; return nil },
		OnEvent: func(event Event) error {
			if _, err := inbox.Receive(event.Change); err != nil {
				return err
			}
			events <- event
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stop := runDurableClient(t, client)
	defer stop()
	first := awaitDurableEvent(t, events)
	second := awaitDurableEvent(t, events)
	if first.Sequence != 2 || second.Sequence != 3 || first.Change.Dot != (replica.Dot{Actor: "bob", Counter: 1}) || second.Change.Dot != (replica.Dot{Actor: "alice", Counter: 2}) {
		t.Fatalf("initial catch-up = %+v %+v", first, second)
	}
	select {
	case highWater := <-catchUps:
		if highWater != 3 {
			t.Fatalf("initial high-water=%d", highWater)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("state-vector catch-up did not complete")
	}
	waitUntil(t, func() bool {
		return client.Cursor() == 3 && inbox.Frontier().Counter("alice") == 2 && inbox.Frontier().Counter("bob") == 1
	})
	if _, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "bob", 2, 2)); err != nil {
		t.Fatal(err)
	}
	var peers []*serverPeer
	group.mu.Lock()
	for peer := range group.peers {
		peers = append(peers, peer)
	}
	group.mu.Unlock()
	if len(peers) != 1 {
		t.Fatalf("connected peers=%d", len(peers))
	}
	for _, peer := range peers {
		peer.close()
	}
	recovered := awaitDurableEvent(t, events)
	if recovered.Sequence != 4 || recovered.Change.Dot != (replica.Dot{Actor: "bob", Counter: 2}) {
		t.Fatalf("recovered event=%+v", recovered)
	}
	select {
	case highWater := <-catchUps:
		if highWater != 4 {
			t.Fatalf("recovered high-water=%d", highWater)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect state-vector catch-up did not complete")
	}
	waitUntil(t, func() bool { return client.Cursor() == 4 && inbox.Frontier().Counter("bob") == 2 })
}

func TestLongLivedSubscriptionIsPeriodicallyRevalidated(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 4, 1<<20)
	defer func() { _ = store.Close() }()
	group, err := NewGroup(GroupConfig{Manifest: manifest, Validate: func([]byte) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	var revalidations atomic.Int32
	handler, err := NewHandler(Config{
		Store:  store,
		Groups: []*Group{group},
		Authenticate: func(*http.Request) (Peer, error) {
			return Peer{ID: "alice"}, nil
		},
		Authorize:             func(Peer, replica.Manifest, replica.Dot) error { return nil },
		AuthorizeSubscription: func(Peer, replica.Manifest) error { return nil },
		RevalidateSubscription: func(Peer, replica.Manifest) error {
			revalidations.Add(1)
			return ErrUnauthorized
		},
		PingInterval: 10 * time.Millisecond,
		PingTimeout:  50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	connection, closeConnection := durableRawClient(t, endpoint, manifest, 0, "alice")
	defer closeConnection()
	readContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := connection.Read(readContext); err == nil {
		t.Fatal("revoked long-lived subscription remained open")
	}
	if revalidations.Load() == 0 {
		t.Fatal("long-lived subscription was not revalidated")
	}
}

func TestStateVectorClientRequiresCatchUpCheckpoint(t *testing.T) {
	manifest := durableTestManifest(t)
	if _, err := NewReconnectClient("ws://example.test/ws", manifest, ClientConfig{
		StateVector: func() replica.Frontier { return replica.Frontier{} },
		OnEvent:     func(Event) error { return nil },
	}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("state-vector client without checkpoint = %v", err)
	}
}

func TestStateVectorClientCheckpointFailureDoesNotAdvanceCursor(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 4, 1<<20)
	defer func() { _ = store.Close() }()
	if _, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "alice", 1, 1)); err != nil {
		t.Fatal(err)
	}
	handler, _ := durableTestHandler(t, store, manifest)
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	called := make(chan struct{}, 1)
	client, err := NewReconnectClient(endpoint, manifest, ClientConfig{
		Header:      http.Header{"Authorization": []string{"Bearer alice"}},
		StateVector: func() replica.Frontier { return replica.Frontier{} },
		OnEvent:     func(Event) error { return nil },
		OnCatchUp: func(uint64) error {
			select {
			case called <- struct{}{}:
			default:
			}
			return errors.New("checkpoint failed")
		},
		MinReconnectBackoff: 10 * time.Millisecond,
		MaxReconnectBackoff: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	stop := runDurableClient(t, client)
	defer stop()
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("catch-up checkpoint was not called")
	}
	if client.Cursor() != 0 {
		t.Fatalf("cursor advanced after failed state-vector checkpoint: %d", client.Cursor())
	}
}
