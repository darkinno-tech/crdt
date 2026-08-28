package durable

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/replica"
)

func TestStorePersistsRelayHLCAndMerkleInventory(t *testing.T) {
	manifest := durableTestManifest(t)
	path := t.TempDir() + "/relay.db"
	store := durableMerkleTestStore(t, path, 8)
	first, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "alice", 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "bob", 1, 2))
	if err != nil {
		t.Fatal(err)
	}
	if !first.Event.HLC.Valid() || !second.Event.HLC.Valid() || first.Event.HLC.ReplicaID != "relay" || first.Event.HLC.Compare(second.Event.HLC) >= 0 {
		t.Fatalf("relay HLCs = %#v %#v", first.Event.HLC, second.Event.HLC)
	}
	snapshot, err := store.MerkleSnapshot(manifest.GroupID, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
	if err != nil || snapshot.HighWater != 2 || snapshot.HLC != second.Event.HLC || len(snapshot.Leaves) != 2 {
		t.Fatalf("snapshot = %#v, %v", snapshot, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = durableMerkleTestStore(t, path, 8)
	defer func() { _ = store.Close() }()
	third, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "alice", 2, 3))
	if err != nil {
		t.Fatal(err)
	}
	if third.Event.HLC.Compare(second.Event.HLC) <= 0 {
		t.Fatalf("restarted HLC = %#v, want after %#v", third.Event.HLC, second.Event.HLC)
	}
	if _, err := store.MerkleEvents(manifest.GroupID, []crdt.Tag{first.Event.HLC, third.Event.HLC}, 2, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); err != nil {
		t.Fatalf("read requested Merkle events: %v", err)
	}
}

func TestMerkleReconnectClientRepairsMissingEventsThenStartsLive(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableMerkleTestStore(t, t.TempDir()+"/relay.db", 16)
	defer func() { _ = store.Close() }()
	var committed []Event
	for _, change := range []replica.Change{
		durableTestChange(t, manifest, "alice", 1, 1),
		durableTestChange(t, manifest, "bob", 1, 1),
		durableTestChange(t, manifest, "alice", 2, 2),
	} {
		result, err := store.Append(manifest.GroupID, change)
		if err != nil {
			t.Fatal(err)
		}
		committed = append(committed, result.Event)
	}
	handler, group := durableTestHandler(t, store, manifest)
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	index := NewMerkleIndex()
	if err := index.Put(committed[0]); err != nil {
		t.Fatal(err)
	}
	frontier, err := replica.NewFrontier(map[string]uint64{"alice": 1})
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := replica.NewInbox(manifest, frontier, 8, 1<<20, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	installed := make(chan Event, 4)
	boundaries := make(chan MerkleBoundary, 2)
	client, err := NewReconnectClient(endpoint, manifest, ClientConfig{
		Header:          http.Header{"Authorization": []string{"Bearer alice"}},
		MerkleRoot:      index.Root,
		ReconcileMerkle: index.Reconcile,
		OnEvent: func(event Event) error {
			if _, err := inbox.Receive(event.Change); err != nil {
				return err
			}
			if err := index.Put(event); err != nil {
				return err
			}
			installed <- event
			return nil
		},
		OnMerkleCatchUp: func(boundary MerkleBoundary) error {
			boundaries <- boundary
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stop := runDurableClient(t, client)
	defer stop()
	first := awaitDurableEvent(t, installed)
	second := awaitDurableEvent(t, installed)
	if first.Sequence != 2 || second.Sequence != 3 || !first.HLC.Valid() || !second.HLC.Valid() {
		t.Fatalf("repaired events = %#v %#v", first, second)
	}
	var boundary MerkleBoundary
	select {
	case boundary = <-boundaries:
	case <-time.After(2 * time.Second):
		t.Fatal("HLC/Merkle boundary was not persisted")
	}
	if boundary.HighWater != 3 || boundary.Root != index.Root() || client.Cursor() != 3 {
		t.Fatalf("boundary=%#v root=%x cursor=%d", boundary, index.Root(), client.Cursor())
	}
	if got := inbox.Frontier(); got.Counter("alice") != 2 || got.Counter("bob") != 1 {
		t.Fatalf("repaired frontier = %#v", got.Entries())
	}

	liveChange := durableTestChange(t, manifest, "bob", 2, 4)
	encoded, err := marshalChange(liveChange)
	if err != nil {
		t.Fatal(err)
	}
	if err := group.publish(Peer{ID: "bob"}, encoded, handler); err != nil {
		t.Fatal(err)
	}
	live := awaitDurableEvent(t, installed)
	if live.Sequence != 4 || !live.HLC.Valid() {
		t.Fatalf("live HLC/Merkle event = %#v", live)
	}
	waitUntil(t, func() bool {
		return client.Cursor() == 4 && index.Root() == mustMerkleSnapshot(t, store, manifest).Root
	})
}

func TestMerkleReconnectClientAcceptsEqualRootWithoutHistoryTransfer(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableMerkleTestStore(t, t.TempDir()+"/relay.db", 4)
	defer func() { _ = store.Close() }()
	committed, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "alice", 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	handler, _ := durableTestHandler(t, store, manifest)
	server := httptest.NewServer(handler)
	defer server.Close()
	index := NewMerkleIndex()
	if err := index.Put(committed.Event); err != nil {
		t.Fatal(err)
	}
	installed := make(chan Event, 1)
	boundaries := make(chan MerkleBoundary, 1)
	client, err := NewReconnectClient("ws"+strings.TrimPrefix(server.URL, "http")+"/ws", manifest, ClientConfig{
		Header:          http.Header{"Authorization": []string{"Bearer alice"}},
		MerkleRoot:      index.Root,
		ReconcileMerkle: index.Reconcile,
		OnEvent: func(event Event) error {
			installed <- event
			return nil
		},
		OnMerkleCatchUp: func(boundary MerkleBoundary) error {
			boundaries <- boundary
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stop := runDurableClient(t, client)
	defer stop()
	select {
	case boundary := <-boundaries:
		if boundary.HighWater != 1 || boundary.Root != index.Root() {
			t.Fatalf("equal-root boundary = %#v", boundary)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("equal root did not complete")
	}
	select {
	case event := <-installed:
		t.Fatalf("equal root replayed event %#v", event)
	case <-time.After(100 * time.Millisecond):
	}
	if client.Cursor() != 1 {
		t.Fatalf("equal-root cursor=%d", client.Cursor())
	}
}

func TestMerkleIndexFailsClosedOnUnexpectedHistory(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableMerkleTestStore(t, t.TempDir()+"/relay.db", 4)
	defer func() { _ = store.Close() }()
	first, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "alice", 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "bob", 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	index := NewMerkleIndex()
	if err := index.Put(first.Event); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Reconcile([]MerkleLeaf{}); !errors.Is(err, ErrMerkleDiverged) {
		t.Fatalf("local-only reconciliation = %v", err)
	}
	leaf, err := merkleLeafForEvent(second.Event)
	if err != nil {
		t.Fatal(err)
	}
	leaf.Digest[0] ^= 0xff
	if _, err := index.Reconcile([]MerkleLeaf{leaf}); !errors.Is(err, ErrMerkleDiverged) {
		t.Fatalf("different leaf reconciliation = %v", err)
	}
}

func durableMerkleTestStore(t *testing.T, path string, maxEvents uint64) *Store {
	t.Helper()
	store, err := OpenStore(path, StoreConfig{MaxEvents: maxEvents, MaxBytes: 1 << 20, HLCReplicaID: "relay"})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func mustMerkleSnapshot(t *testing.T, store *Store, manifest replica.Manifest) MerkleSnapshot {
	t.Helper()
	snapshot, err := store.MerkleSnapshot(manifest.GroupID, 16, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestMerkleWireRoundTripAndBounds(t *testing.T) {
	manifest := durableTestManifest(t)
	root := NewMerkleIndex().Root()
	hello, err := marshalMerkleHello(manifest, root)
	if err != nil {
		t.Fatal(err)
	}
	remote, decodedRoot, err := unmarshalMerkleHello(hello)
	if err != nil || manifest.Compatible(remote) != nil || decodedRoot != root {
		t.Fatalf("merkle hello remote=%#v root=%x err=%v", remote, decodedRoot, err)
	}
	boundary := MerkleBoundary{Root: root, HighWater: 1, HLC: crdt.Tag{ReplicaID: "relay", WallTime: 1}}
	welcome, err := marshalMerkleWelcome(manifest, boundary)
	if err != nil {
		t.Fatal(err)
	}
	remote, decodedBoundary, err := unmarshalMerkleWelcome(welcome, 128)
	if err != nil || manifest.Compatible(remote) != nil || decodedBoundary != boundary {
		t.Fatalf("merkle welcome remote=%#v boundary=%#v err=%v", remote, decodedBoundary, err)
	}
	if _, _, err := unmarshalMerkleHello([]byte(`{"version":3,"kind":"hello","manifest":{},"root":"bad"}`)); !errors.Is(err, errInvalidWire) {
		t.Fatalf("invalid merkle hello = %v", err)
	}
}

func TestMerkleClientConfigurationRequiresCompleteExclusiveCallbacks(t *testing.T) {
	manifest := durableTestManifest(t)
	validEvent := func(Event) error { return nil }
	if _, err := NewReconnectClient("ws://example.test/ws", manifest, ClientConfig{OnEvent: validEvent, MerkleRoot: NewMerkleIndex().Root}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("partial Merkle config = %v", err)
	}
	if _, err := NewReconnectClient("ws://example.test/ws", manifest, ClientConfig{
		OnEvent:         validEvent,
		StateVector:     func() replica.Frontier { return replica.Frontier{} },
		OnCatchUp:       func(uint64) error { return nil },
		MerkleRoot:      NewMerkleIndex().Root,
		ReconcileMerkle: NewMerkleIndex().Reconcile,
		OnMerkleCatchUp: func(MerkleBoundary) error { return nil },
	}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("mixed state-vector/Merkle config = %v", err)
	}
}
