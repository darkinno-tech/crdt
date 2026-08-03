package durable

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/replica"
	"github.com/coder/websocket"
	bolt "go.etcd.io/bbolt"
)

func TestMerkleControlChunksAreBoundedAndCanonical(t *testing.T) {
	leaves := make([]MerkleLeaf, 0, 16)
	identities := make([]crdt.Tag, 0, 16)
	for index := uint64(1); index <= 16; index++ {
		identity := crdt.Tag{ReplicaID: "relay", WallTime: index}
		identities = append(identities, identity)
		leaves = append(leaves, MerkleLeaf{HLC: identity, Digest: sha256.Sum256([]byte{byte(index)})})
	}
	inventory, err := marshalMerkleInventoryChunks(leaves, 512)
	if err != nil || len(inventory) < 2 {
		t.Fatalf("inventory chunks=%d err=%v", len(inventory), err)
	}
	var restoredLeaves []MerkleLeaf
	for index, chunk := range inventory {
		if len(chunk) > 512 {
			t.Fatalf("inventory chunk %d has %d bytes", index, len(chunk))
		}
		part, done, err := unmarshalMerkleInventory(chunk, 128)
		if err != nil || done != (index == len(inventory)-1) {
			t.Fatalf("inventory chunk %d done=%v err=%v", index, done, err)
		}
		restoredLeaves = append(restoredLeaves, part...)
	}
	if len(restoredLeaves) != len(leaves) {
		t.Fatalf("restored %d leaves, want %d", len(restoredLeaves), len(leaves))
	}
	for index := range leaves {
		if restoredLeaves[index] != leaves[index] {
			t.Fatalf("leaf %d = %#v, want %#v", index, restoredLeaves[index], leaves[index])
		}
	}
	requests, err := marshalMerkleRequestChunks(identities, 256)
	if err != nil || len(requests) < 2 {
		t.Fatalf("request chunks=%d err=%v", len(requests), err)
	}
	var restoredIdentities []crdt.Tag
	for index, chunk := range requests {
		if len(chunk) > 256 {
			t.Fatalf("request chunk %d has %d bytes", index, len(chunk))
		}
		part, done, err := unmarshalMerkleRequest(chunk, 128)
		if err != nil || done != (index == len(requests)-1) {
			t.Fatalf("request chunk %d done=%v err=%v", index, done, err)
		}
		restoredIdentities = append(restoredIdentities, part...)
	}
	if len(restoredIdentities) != len(identities) {
		t.Fatalf("restored %d identities, want %d", len(restoredIdentities), len(identities))
	}
	for index := range identities {
		if restoredIdentities[index] != identities[index] {
			t.Fatalf("identity %d = %#v, want %#v", index, restoredIdentities[index], identities[index])
		}
	}
	for _, encode := range []func(int) error{
		func(limit int) error { _, err := marshalMerkleInventoryChunks(nil, limit); return err },
		func(limit int) error { _, err := marshalMerkleRequestChunks(nil, limit); return err },
	} {
		if err := encode(0); !errors.Is(err, errInvalidWire) {
			t.Fatalf("zero control limit err=%v", err)
		}
	}
	if chunks, err := marshalMerkleInventoryChunks(nil, 512); err != nil || len(chunks) != 1 {
		t.Fatalf("empty inventory chunks=%d err=%v", len(chunks), err)
	}
	if chunks, err := marshalMerkleRequestChunks(nil, 512); err != nil || len(chunks) != 1 {
		t.Fatalf("empty request chunks=%d err=%v", len(chunks), err)
	}
	if _, err := marshalMerkleInventoryChunks([]MerkleLeaf{{HLC: crdt.Tag{}}}, 512); !errors.Is(err, errInvalidWire) {
		t.Fatalf("invalid leaf inventory err=%v", err)
	}
	if _, err := marshalMerkleRequestChunks([]crdt.Tag{{}}, 256); !errors.Is(err, errInvalidWire) {
		t.Fatalf("invalid identity request err=%v", err)
	}
}

func TestMerkleWireAndHelperRejectInvalidBoundaries(t *testing.T) {
	manifest := durableTestManifest(t)
	change := durableTestChange(t, manifest, "alice", 1, 1)
	event := Event{Sequence: 1, HLC: crdt.Tag{ReplicaID: "relay", WallTime: 1}, Change: change}
	encoded, err := marshalMerkleEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	sequence, identity, dot, delta, err := unmarshalMerkleEvent(encoded, 1<<20, 128)
	if err != nil || sequence != event.Sequence || identity != event.HLC || dot != change.Dot {
		t.Fatalf("event sequence=%d identity=%#v dot=%#v err=%v", sequence, identity, dot, err)
	}
	if restored, err := newEventFromWire(manifest, crdt.ProtocolPolicy{}, sequence, dot, delta); err != nil || restored.Change.Dot != change.Dot {
		t.Fatalf("restored=%#v err=%v", restored, err)
	}
	if _, _, _, _, err := unmarshalMerkleEvent(append(encoded, 0), 1<<20, 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("trailing HLC/Merkle event err=%v", err)
	}
	if _, err := marshalMerkleEvent(Event{Sequence: 1, Change: change}); !errors.Is(err, errInvalidWire) {
		t.Fatalf("missing event HLC err=%v", err)
	}
	if _, err := marshalMerkleEvent(Event{Sequence: 1, HLC: event.HLC}); !errors.Is(err, errInvalidWire) {
		t.Fatalf("invalid event change err=%v", err)
	}
	noChange := []byte{merkleEventMessage}
	noChange = frame.AppendUvarint(noChange, event.Sequence)
	noChange = frame.AppendTag(noChange, event.HLC)
	if _, _, _, _, err := unmarshalMerkleEvent(noChange, 1<<20, 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("HLC/Merkle event without change err=%v", err)
	}
	if _, err := unmarshalMerkleLeaf(merkleLeafMessage{Digest: encodeMerkleDigest(sha256.Sum256([]byte("leaf")))}, 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("leaf without HLC err=%v", err)
	}
	if _, _, err := unmarshalMerkleWelcome([]byte(`{"version":3,"kind":"welcome","manifest":{},"root":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","high_water":1}`), 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("welcome without HLC err=%v", err)
	}
	if _, _, err := unmarshalMerkleWelcome([]byte(`{"version":3,"kind":"welcome","manifest":{},"root":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","high_water":1,"hlc":{"replica_id":"","wall_time":1,"logical":0}}`), 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("welcome with invalid HLC err=%v", err)
	}
	if _, err := unmarshalMerkleComplete([]byte(`{"version":3,"kind":"complete","root":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","high_water":1}`), 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("complete without HLC err=%v", err)
	}
	if _, _, err := unmarshalMerkleInventory([]byte(`{"version":3,"kind":"inventory","leaves":[{"hlc":{"replica_id":"r","wall_time":1,"logical":0},"digest":"not-base64"}],"done":true}`), 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("bad inventory digest err=%v", err)
	}
	if _, _, err := unmarshalMerkleRequest([]byte(`{"version":3,"kind":"request","identities":[{"replica_id":"","wall_time":1,"logical":0}],"done":true}`), 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("bad request identity err=%v", err)
	}
	if err := unmarshalError([]byte(`{"version":1,"code":"anti_entropy_unavailable"}`)); !errors.Is(err, ErrAntiEntropyUnavailable) {
		t.Fatalf("anti-entropy error = %v", err)
	}
	if errorCodeFor(ErrAntiEntropyUnavailable) != "anti_entropy_unavailable" || errorCodeFor(ErrReplayUnavailable) != "replay_unavailable" {
		t.Fatal("unexpected server error mapping")
	}
	leaf, err := merkleLeafForEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := MerkleSnapshot{Leaves: []MerkleLeaf{leaf}}
	if !snapshotContainsMerkleIdentities(snapshot, []crdt.Tag{event.HLC}) || snapshotContainsMerkleIdentities(snapshot, []crdt.Tag{{ReplicaID: "relay", WallTime: 2}}) {
		t.Fatal("snapshot identity membership mismatch")
	}
}

func TestMerkleStoreAndIndexFailClosedAtResourceBoundaries(t *testing.T) {
	manifest := durableTestManifest(t)
	disabled, err := OpenStore(t.TempDir()+"/disabled.db", StoreConfig{MaxEvents: 4, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = disabled.Close() }()
	if disabled.MerkleEnabled() {
		t.Fatal("legacy store advertised Merkle")
	}
	if _, err := disabled.MerkleSnapshot(manifest.GroupID, 4, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrAntiEntropyUnavailable) {
		t.Fatalf("legacy Merkle snapshot err=%v", err)
	}
	if _, err := disabled.MerkleEvents(manifest.GroupID, nil, 4, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrAntiEntropyUnavailable) {
		t.Fatalf("legacy Merkle events err=%v", err)
	}
	if _, err := OpenStore(t.TempDir()+"/invalid.db", StoreConfig{MaxEvents: 1, MaxBytes: 1, HLCReplicaID: " "}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid HLC relay id err=%v", err)
	}

	store := durableMerkleTestStore(t, t.TempDir()+"/relay.db", 4)
	defer func() { _ = store.Close() }()
	first, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "alice", 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "bob", 1, 2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MerkleSnapshot(manifest.GroupID, 1, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrAntiEntropyUnavailable) {
		t.Fatalf("leaf bound err=%v", err)
	}
	if _, err := store.MerkleSnapshot(manifest.GroupID, 4, 1, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrAntiEntropyUnavailable) {
		t.Fatalf("byte bound err=%v", err)
	}
	events, err := store.MerkleEvents(manifest.GroupID, []crdt.Tag{first.Event.HLC, second.Event.HLC}, 2, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
	if err != nil || len(events) != 2 || events[0].HLC != first.Event.HLC || events[1].HLC != second.Event.HLC {
		t.Fatalf("Merkle events=%#v err=%v", events, err)
	}
	if _, err := store.MerkleEvents(manifest.GroupID, []crdt.Tag{first.Event.HLC, first.Event.HLC}, 2, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("duplicate requested HLC err=%v", err)
	}
	if _, err := store.MerkleEvents(manifest.GroupID, []crdt.Tag{{ReplicaID: "relay", WallTime: second.Event.HLC.WallTime + 1}}, 1, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrAntiEntropyUnavailable) {
		t.Fatalf("unknown requested HLC err=%v", err)
	}

	index := NewMerkleIndex()
	if err := index.Put(first.Event); err != nil {
		t.Fatal(err)
	}
	if err := index.Put(Event{}); !errors.Is(err, errInvalidWire) {
		t.Fatalf("invalid index event err=%v", err)
	}
	if err := index.Put(first.Event); err != nil {
		t.Fatalf("idempotent put err=%v", err)
	}
	conflict := first.Event
	conflict.Change = durableTestChange(t, manifest, "alice", 1, 9)
	if err := index.Put(conflict); !errors.Is(err, ErrMerkleDiverged) {
		t.Fatalf("same HLC conflict err=%v", err)
	}
	secondLeaf, err := merkleLeafForEvent(second.Event)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := index.Reconcile([]MerkleLeaf{mustMerkleLeaf(t, first.Event), secondLeaf})
	if err != nil || len(missing) != 1 || missing[0] != second.Event.HLC {
		t.Fatalf("missing identities=%#v err=%v", missing, err)
	}
	if _, err := index.Reconcile([]MerkleLeaf{secondLeaf, secondLeaf}); !errors.Is(err, errInvalidWire) {
		t.Fatalf("non-canonical remote inventory err=%v", err)
	}
	conflictingLeaf := mustMerkleLeaf(t, first.Event)
	conflictingLeaf.Digest[0] ^= 0xff
	if _, err := index.Reconcile([]MerkleLeaf{conflictingLeaf}); !errors.Is(err, ErrMerkleDiverged) {
		t.Fatalf("same identity digest conflict err=%v", err)
	}
	var nilIndex *MerkleIndex
	if err := nilIndex.Put(first.Event); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil index put err=%v", err)
	}
	if _, err := nilIndex.Reconcile(nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil index reconcile err=%v", err)
	}
}

func mustMerkleLeaf(t *testing.T, event Event) MerkleLeaf {
	t.Helper()
	leaf, err := merkleLeafForEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func TestMerkleInternalValidation(t *testing.T) {
	if got := merkleUint64(-1); got != 0 {
		t.Fatalf("merkleUint64(-1) = %d, want 0", got)
	}
	if got := merkleUint64(7); got != 7 {
		t.Fatalf("merkleUint64(7) = %d, want 7", got)
	}

	identity := crdt.Tag{ReplicaID: "relay", WallTime: 1}
	if !validMerkleHLC(identity, 128) || validMerkleHLC(crdt.Tag{ReplicaID: " "}, 128) {
		t.Fatal("unexpected HLC validity")
	}
	if err := validateMerkleLeaves([]MerkleLeaf{{HLC: identity}}, 1, merkleLeafBytes(MerkleLeaf{HLC: identity}), 128); err != nil {
		t.Fatalf("valid leaf err=%v", err)
	}
	if err := validateMerkleIdentityRequest([]crdt.Tag{identity}, 1, 1, 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("identity byte bound err=%v", err)
	}
	if _, err := relayHLCState(nil, "relay", 128); err != nil {
		t.Fatalf("new relay state err=%v", err)
	}
	if _, err := relayHLCState([]byte{1}, "relay", 128); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("corrupt relay state err=%v", err)
	}
	if _, err := relayHLCState(nil, " ", 128); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("invalid relay id err=%v", err)
	}
	if _, err := merkleLeafValue(Event{}); !errors.Is(err, errInvalidWire) {
		t.Fatalf("invalid Merkle leaf value err=%v", err)
	}
	if _, err := merkleLeafForEvent(Event{HLC: identity}); !errors.Is(err, errInvalidWire) {
		t.Fatalf("invalid Merkle leaf event err=%v", err)
	}
	if err := validateMerkleLeaves(nil, 0, 1, 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("zero leaf limit err=%v", err)
	}
	if err := validateMerkleIdentityRequest(nil, 0, 1, 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("zero identity limit err=%v", err)
	}
	if (&MerkleIndex{}).Root() != NewMerkleIndex().Root() {
		t.Fatal("uninitialized index did not return canonical empty root")
	}
	client := &ReconnectClient{}
	if _, err := client.marshalHello(MerkleSubprotocol, replica.Frontier{}); !errors.Is(err, errInvalidWire) {
		t.Fatalf("Merkle hello without root err=%v", err)
	}
}

func TestMerkleStoreAdditionalFailureAndDuplicatePaths(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableMerkleTestStore(t, t.TempDir()+"/relay.db", 4)
	defer func() { _ = store.Close() }()
	if _, err := store.MerkleSnapshot("missing", 4, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); err != nil {
		t.Fatalf("empty HLC/Merkle snapshot err=%v", err)
	}
	if _, err := store.MerkleSnapshot("", 4, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid snapshot config err=%v", err)
	}
	if _, err := store.MerkleEvents("", nil, 4, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid events config err=%v", err)
	}
	if events, err := store.MerkleEvents(manifest.GroupID, nil, 4, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); err != nil || events != nil {
		t.Fatalf("empty identity request events=%#v err=%v", events, err)
	}
	change := durableTestChange(t, manifest, "alice", 1, 1)
	first, err := store.Append(manifest.GroupID, change)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := store.Append(manifest.GroupID, change)
	if err != nil || !duplicate.Duplicate || duplicate.Event.Sequence != first.Event.Sequence || duplicate.Event.HLC != first.Event.HLC {
		t.Fatalf("duplicate=%#v err=%v", duplicate, err)
	}
	stored, err := marshalChange(change)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MerkleEvents(manifest.GroupID, []crdt.Tag{first.Event.HLC}, 1, uint64(len(stored)-1), manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrAntiEntropyUnavailable) {
		t.Fatalf("event byte bound err=%v", err)
	}
	if _, err := store.MerkleEvents(manifest.GroupID, []crdt.Tag{first.Event.HLC}, 1, 1<<20, manifest, crdt.ProtocolPolicy{}, len(stored), 128); !errors.Is(err, ErrAntiEntropyUnavailable) {
		t.Fatalf("event message bound err=%v", err)
	}
	if err := store.db.Update(func(transaction *bolt.Tx) error {
		group, err := store.groupBucket(transaction, manifest.GroupID, false)
		if err != nil {
			return err
		}
		return group.Bucket(bucketHLCIdx).Delete([]byte(merkleLeafKey(first.Event.HLC)))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MerkleSnapshot(manifest.GroupID, 4, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrAntiEntropyUnavailable) {
		t.Fatalf("missing HLC index snapshot err=%v", err)
	}
	if _, err := store.MerkleEvents("missing", []crdt.Tag{first.Event.HLC}, 1, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrAntiEntropyUnavailable) {
		t.Fatalf("missing group event request err=%v", err)
	}

	corrupt := durableMerkleTestStore(t, t.TempDir()+"/corrupt.db", 4)
	defer func() { _ = corrupt.Close() }()
	committed, err := corrupt.Append(manifest.GroupID, durableTestChange(t, manifest, "bob", 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := corrupt.db.Update(func(transaction *bolt.Tx) error {
		group, err := corrupt.groupBucket(transaction, manifest.GroupID, false)
		if err != nil {
			return err
		}
		return group.Bucket(bucketHLCs).Put(sequenceKey(committed.Event.Sequence), []byte{1})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := corrupt.MerkleEvents(manifest.GroupID, []crdt.Tag{committed.Event.HLC}, 1, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrAntiEntropyUnavailable) {
		t.Fatalf("corrupt HLC event request err=%v", err)
	}
	if _, err := corrupt.MerkleSnapshot(manifest.GroupID, 4, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("corrupt HLC snapshot err=%v", err)
	}
}

func TestMerkleHandlerSubscriptionGuards(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableMerkleTestStore(t, t.TempDir()+"/relay.db", 4)
	defer func() { _ = store.Close() }()
	handler, group := durableTestHandler(t, store, manifest)
	peer := &serverPeer{}
	if _, err := group.subscribeMerkle(store, peer, subscriptionRequest{}, handler.limits); !errors.Is(err, errInvalidWire) {
		t.Fatalf("request without root err=%v", err)
	}
	root := NewMerkleIndex().Root()
	request := subscriptionRequest{merkleRoot: &root}
	snapshot, err := group.subscribeMerkle(store, peer, request, handler.limits)
	if err != nil || snapshot.Root != root || snapshot.HighWater != 0 {
		t.Fatalf("empty subscribe snapshot=%#v err=%v", snapshot, err)
	}
	group.remove(peer)
	legacy := durableTestStore(t, t.TempDir()+"/legacy.db", 4, 1<<20)
	defer func() { _ = legacy.Close() }()
	if _, err := group.subscribeMerkle(legacy, peer, request, handler.limits); !errors.Is(err, ErrAntiEntropyUnavailable) {
		t.Fatalf("legacy Merkle subscription err=%v", err)
	}
}

func TestMerkleControlsRejectMalformedOrUnboundedInputs(t *testing.T) {
	manifest := durableTestManifest(t)
	root := NewMerkleIndex().Root()
	hello, err := marshalMerkleHello(manifest, root)
	if err != nil {
		t.Fatal(err)
	}
	boundary := MerkleBoundary{Root: root}
	welcome, err := marshalMerkleWelcome(manifest, boundary)
	if err != nil {
		t.Fatal(err)
	}
	complete, err := marshalMerkleComplete(boundary)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{nil, make([]byte, maxControlBytes+1), append(hello, 'x'), []byte(`{"version":3,"kind":"hello","manifest":{},"root":"short"}`)} {
		if _, _, err := unmarshalMerkleHello(data); !errors.Is(err, errInvalidWire) {
			t.Fatalf("invalid hello %q err=%v", data, err)
		}
	}
	for _, data := range [][]byte{nil, append(welcome, 'x'), []byte(`{"version":3,"kind":"welcome","manifest":{},"root":"bad","high_water":0,"unknown":true}`)} {
		if _, _, err := unmarshalMerkleWelcome(data, 128); !errors.Is(err, errInvalidWire) {
			t.Fatalf("invalid welcome %q err=%v", data, err)
		}
	}
	for _, data := range [][]byte{nil, append(complete, 'x'), []byte(`{"version":3,"kind":"complete","root":"bad","high_water":0,"unknown":true}`)} {
		if _, err := unmarshalMerkleComplete(data, 128); !errors.Is(err, errInvalidWire) {
			t.Fatalf("invalid complete %q err=%v", data, err)
		}
	}
	for _, decode := range []func([]byte) error{
		func(data []byte) error { _, _, err := unmarshalMerkleInventory(data, 128); return err },
		func(data []byte) error { _, _, err := unmarshalMerkleRequest(data, 128); return err },
	} {
		if err := decode([]byte(`{"version":3,"kind":"unexpected"}`)); !errors.Is(err, errInvalidWire) {
			t.Fatalf("invalid control kind err=%v", err)
		}
	}
	if _, err := marshalMerkleInventoryChunks(nil, 1); !errors.Is(err, errInvalidWire) {
		t.Fatalf("tiny inventory limit err=%v", err)
	}
	if _, err := marshalMerkleRequestChunks(nil, 1); !errors.Is(err, errInvalidWire) {
		t.Fatalf("tiny request limit err=%v", err)
	}
	unsorted := []MerkleLeaf{
		{HLC: crdt.Tag{ReplicaID: "relay", WallTime: 2}, Digest: sha256.Sum256([]byte("two"))},
		{HLC: crdt.Tag{ReplicaID: "relay", WallTime: 1}, Digest: sha256.Sum256([]byte("one"))},
	}
	if _, err := marshalMerkleInventoryChunks(unsorted, 512); !errors.Is(err, errInvalidWire) {
		t.Fatalf("unsorted inventory err=%v", err)
	}
	if _, err := marshalMerkleRequestChunks([]crdt.Tag{unsorted[0].HLC, unsorted[1].HLC}, 256); !errors.Is(err, errInvalidWire) {
		t.Fatalf("unsorted request err=%v", err)
	}
	if _, _, _, _, err := unmarshalMerkleEvent([]byte{merkleEventMessage, 0}, 1<<20, 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("zero sequence HLC/Merkle event err=%v", err)
	}
	if _, err := marshalWelcomeForSubprotocol(MerkleSubprotocol, manifest, 0); !errors.Is(err, errInvalidWire) {
		t.Fatalf("legacy welcome helper accepted v3 err=%v", err)
	}
}

func TestMerkleClientRefusesLegacyProtocolDowngrade(t *testing.T) {
	manifest := durableTestManifest(t)
	legacyOnly := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}})
		if err != nil {
			return
		}
		defer func() { _ = connection.CloseNow() }()
		<-request.Context().Done()
	}))
	defer legacyOnly.Close()
	index := NewMerkleIndex()
	client, err := NewReconnectClient("ws"+strings.TrimPrefix(legacyOnly.URL, "http"), manifest, ClientConfig{
		MerkleRoot:      index.Root,
		ReconcileMerkle: index.Reconcile,
		OnMerkleCatchUp: func(MerkleBoundary) error { return nil },
		OnEvent:         func(Event) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Run(context.Background()); !errors.Is(err, errInvalidWire) {
		t.Fatalf("legacy downgrade Run err=%v", err)
	}
}

func TestMerkleClientCheckpointFailureDoesNotAdvanceCursor(t *testing.T) {
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
	called := make(chan struct{}, 1)
	client, err := NewReconnectClient("ws"+strings.TrimPrefix(server.URL, "http")+"/ws", manifest, ClientConfig{
		Header:          http.Header{"Authorization": []string{"Bearer alice"}},
		MerkleRoot:      index.Root,
		ReconcileMerkle: index.Reconcile,
		OnEvent:         func(Event) error { return nil },
		OnMerkleCatchUp: func(MerkleBoundary) error {
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
		t.Fatal("Merkle checkpoint was not called")
	}
	if client.Cursor() != 0 {
		t.Fatalf("cursor advanced after failed Merkle checkpoint: %d", client.Cursor())
	}
}

func TestMerkleClientRejectsRootChangedDuringHandshake(t *testing.T) {
	manifest := durableTestManifest(t)
	root := NewMerkleIndex().Root()
	changed := root
	changed[0] = 1
	boundary := MerkleBoundary{Root: root}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{MerkleSubprotocol}})
		if err != nil {
			return
		}
		defer func() { _ = connection.CloseNow() }()
		if _, _, err := connection.Read(request.Context()); err != nil {
			return
		}
		welcome, err := marshalMerkleWelcome(manifest, boundary)
		if err != nil || connection.Write(request.Context(), websocket.MessageText, welcome) != nil {
			return
		}
		complete, err := marshalMerkleComplete(boundary)
		if err == nil {
			_ = connection.Write(request.Context(), websocket.MessageText, complete)
		}
	}))
	defer server.Close()
	calls := 0
	client, err := NewReconnectClient("ws"+strings.TrimPrefix(server.URL, "http"), manifest, ClientConfig{
		MerkleRoot: func() [sha256.Size]byte {
			calls++
			if calls == 1 {
				return root
			}
			return changed
		},
		ReconcileMerkle: NewMerkleIndex().Reconcile,
		OnMerkleCatchUp: func(MerkleBoundary) error { return nil },
		OnEvent:         func(Event) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Run(context.Background()); !errors.Is(err, ErrAntiEntropyUnavailable) {
		t.Fatalf("changed root Run err=%v", err)
	}
	if calls != 2 {
		t.Fatalf("MerkleRoot called %d times, want 2", calls)
	}
}

func TestMerkleClientFailsClosedForMalformedCompletionAndInstallError(t *testing.T) {
	manifest := durableTestManifest(t)
	root := NewMerkleIndex().Root()
	boundary := MerkleBoundary{Root: root}
	malformed := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{MerkleSubprotocol}})
		if err != nil {
			return
		}
		defer func() { _ = connection.CloseNow() }()
		if _, _, err := connection.Read(request.Context()); err != nil {
			return
		}
		welcome, err := marshalMerkleWelcome(manifest, boundary)
		if err == nil && connection.Write(request.Context(), websocket.MessageText, welcome) == nil {
			_ = connection.Write(request.Context(), websocket.MessageText, []byte(`{"version":3,"kind":"complete","root":"bad","high_water":0}`))
		}
	}))
	defer malformed.Close()
	client, err := NewReconnectClient("ws"+strings.TrimPrefix(malformed.URL, "http"), manifest, ClientConfig{
		MerkleRoot:      func() [sha256.Size]byte { return root },
		ReconcileMerkle: NewMerkleIndex().Reconcile,
		OnMerkleCatchUp: func(MerkleBoundary) error { return nil },
		OnEvent:         func(Event) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.runSession(context.Background()); !errors.Is(err, errInvalidWire) {
		t.Fatalf("malformed complete runSession err=%v", err)
	}

	store := durableMerkleTestStore(t, t.TempDir()+"/relay.db", 4)
	defer func() { _ = store.Close() }()
	if _, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "alice", 1, 1)); err != nil {
		t.Fatal(err)
	}
	handler, _ := durableTestHandler(t, store, manifest)
	server := httptest.NewServer(handler)
	defer server.Close()
	index := NewMerkleIndex()
	installFailure, err := NewReconnectClient("ws"+strings.TrimPrefix(server.URL, "http")+"/ws", manifest, ClientConfig{
		Header:          http.Header{"Authorization": []string{"Bearer alice"}},
		MerkleRoot:      index.Root,
		ReconcileMerkle: index.Reconcile,
		OnMerkleCatchUp: func(MerkleBoundary) error { return nil },
		OnEvent:         func(Event) error { return errors.New("install failed") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := installFailure.runSession(context.Background()); err == nil || errors.Is(err, errInvalidWire) {
		t.Fatalf("install failure runSession err=%v", err)
	}
}
