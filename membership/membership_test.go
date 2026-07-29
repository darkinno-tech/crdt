package membership

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/replica"
	"github.com/DarkInno/crdt/set"
)

type stringCodec struct{}

func (stringCodec) ID() string                            { return "membership-test-string/v1" }
func (stringCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (stringCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }

type fixture struct {
	authorityPrivate ed25519.PrivateKey
	authorityPublic  ed25519.PublicKey
	members          map[string]ed25519.PrivateKey
	view             View
}

func newFixture(t testing.TB, epoch uint64, ids ...string) fixture {
	t.Helper()
	authorityPublic, authorityPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	members := make(map[string]ed25519.PrivateKey, len(ids))
	viewMembers := make([]Member, 0, len(ids))
	for _, id := range ids {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		members[id] = private
		viewMembers = append(viewMembers, Member{ID: id, PublicKey: public, Incarnation: 1})
	}
	manifestHash := sha256.Sum256([]byte("orders/or-set/v1"))
	view, err := SignView(View{GroupID: "orders/v1", Epoch: epoch, ManifestHash: manifestHash, Members: viewMembers}, authorityPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyView(view, authorityPublic); err != nil {
		t.Fatalf("fixture view verification: %v", err)
	}
	return fixture{authorityPrivate: authorityPrivate, authorityPublic: authorityPublic, members: members, view: view}
}

func TestManagerPersistsViewBeforeFencingCoordinator(t *testing.T) {
	t.Parallel()
	setup := newFixture(t, 4, "api", "mobile")
	store := &MemoryStore{}
	manager, err := NewManager[string](setup.view, setup.authorityPublic, store)
	if err != nil {
		t.Fatal(err)
	}
	if got := manager.Coordinator().Membership(); got.Epoch != 4 || len(got.Members) != 2 {
		t.Fatalf("coordinator membership = %#v", got)
	}
	next, err := SignView(View{
		GroupID:      setup.view.GroupID,
		Epoch:        5,
		PreviousHash: setup.view.Hash(),
		ManifestHash: setup.view.ManifestHash,
		Members:      setup.view.Members,
	}, setup.authorityPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(next); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenManager[string](setup.authorityPublic, store)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.View(); got.Epoch != 5 || got.Hash() != next.Hash() {
		t.Fatalf("restored view = %#v", got)
	}
	if err := manager.Install(setup.view); !errors.Is(err, ErrViewRollback) {
		t.Fatalf("old view install = %v, want %v", err, ErrViewRollback)
	}
}

func TestGCBridgeUsesSignedCurrentViewAndExactTags(t *testing.T) {
	t.Parallel()
	setup := newFixture(t, 1, "api", "mobile")
	manager, err := NewManager[string](setup.view, setup.authorityPublic, &MemoryStore{})
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := NewGCBridge(manager)
	if err != nil {
		t.Fatal(err)
	}
	target, err := set.NewORSet("api", stringCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.Add("order-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Remove("order-1"); err != nil {
		t.Fatal(err)
	}
	tags, err := SortedTags(target.TombstoneTags())
	if err != nil {
		t.Fatal(err)
	}
	for sequence, memberID := range []string{"api", "mobile"} {
		receipt, err := SignReceipt(Receipt{
			GroupID:     setup.view.GroupID,
			Epoch:       setup.view.Epoch,
			ViewHash:    setup.view.Hash(),
			MemberID:    memberID,
			Incarnation: 1,
			Sequence:    uint64(sequence + 1),
			Tags:        tags,
		}, setup.members[memberID])
		if err != nil {
			t.Fatal(err)
		}
		removed, err := bridge.Apply(receipt, target)
		if err != nil {
			t.Fatal(err)
		}
		if memberID == "api" && removed != 0 {
			t.Fatalf("first receipt removed %d tombstones", removed)
		}
		if memberID == "mobile" && removed != 1 {
			t.Fatalf("last receipt removed %d tombstones", removed)
		}
	}
	if got := target.TombstoneTags(); len(got) != 0 {
		t.Fatalf("tombstones after all receipts = %#v", got)
	}
	// A replay is rejected before Coordinator observes it.
	replay, err := SignReceipt(Receipt{GroupID: setup.view.GroupID, Epoch: 1, ViewHash: setup.view.Hash(), MemberID: "mobile", Incarnation: 1, Sequence: 2, Tags: tags}, setup.members["mobile"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Apply(replay, target); !errors.Is(err, ErrReceiptReplay) {
		t.Fatalf("receipt replay = %v, want %v", err, ErrReceiptReplay)
	}
}

func TestGossipSuspectCannotChangeMembership(t *testing.T) {
	t.Parallel()
	setup := newFixture(t, 3, "api", "mobile", "worker")
	now := time.Unix(100, 0)
	api, err := NewGossipAt(setup.view, "api", setup.members["api"], time.Second, now)
	if err != nil {
		t.Fatal(err)
	}
	mobile, err := NewGossipAt(setup.view, "mobile", setup.members["mobile"], time.Second, now)
	if err != nil {
		t.Fatal(err)
	}
	message, err := mobile.Heartbeat()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := api.Observe(message, now); err != nil {
		t.Fatal(err)
	}
	events := api.Suspects(now.Add(time.Second))
	if len(events) != 2 || events[0] != (LivenessEvent{MemberID: "mobile", State: Suspect}) || events[1] != (LivenessEvent{MemberID: "worker", State: Suspect}) {
		t.Fatalf("suspect events = %#v", events)
	}
	if peers := api.Peers(2); len(peers) != 2 {
		t.Fatalf("gossip fanout = %#v", peers)
	}
	manager, err := NewManager[string](setup.view, setup.authorityPublic, &MemoryStore{})
	if err != nil {
		t.Fatal(err)
	}
	if members := manager.Coordinator().Membership().Members; len(members) != 3 {
		t.Fatalf("suspect changed GC membership: %#v", members)
	}
	message, err = mobile.Heartbeat()
	if err != nil {
		t.Fatal(err)
	}
	event, changed, err := api.Observe(message, now.Add(2*time.Second))
	if err != nil || !changed || event != (LivenessEvent{MemberID: "mobile", State: Alive}) {
		t.Fatalf("recovery event = %#v, %v, %v", event, changed, err)
	}
}

func TestReceiptRejectsTamperingAndWrongEpoch(t *testing.T) {
	t.Parallel()
	setup := newFixture(t, 8, "api")
	manager, err := NewManager[string](setup.view, setup.authorityPublic, &MemoryStore{})
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := NewGCBridge(manager)
	if err != nil {
		t.Fatal(err)
	}
	target, err := set.NewORSet("api", stringCodec{})
	if err != nil {
		t.Fatal(err)
	}
	tag := crdt.Tag{ReplicaID: "api", WallTime: 1}
	receipt, err := SignReceipt(Receipt{GroupID: setup.view.GroupID, Epoch: setup.view.Epoch, ViewHash: setup.view.Hash(), MemberID: "api", Incarnation: 1, Sequence: 1, Tags: []crdt.Tag{tag}}, setup.members["api"])
	if err != nil {
		t.Fatal(err)
	}
	receipt.Epoch++
	if _, err := bridge.Apply(receipt, target); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("tampered receipt = %v", err)
	}
}

func TestViewBindsDataPlaneManifestAndEpoch(t *testing.T) {
	t.Parallel()
	setup := newFixture(t, 1, "api")
	manifest, err := replica.NewManifest("orders/v1", "orders", 1, replica.Protocol{
		StateID:          crdt.TypeIDORSetState,
		DeltaID:          crdt.TypeIDORSetDelta,
		CodecID:          "membership-test-string/v1",
		SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	view, err := SignView(View{
		GroupID:      manifest.GroupID,
		Epoch:        manifest.Epoch,
		ManifestHash: ManifestHash(manifest),
		Members:      setup.view.Members,
	}, setup.authorityPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if !view.MatchesManifest(manifest) {
		t.Fatal("view did not match its manifest")
	}
	manifest.Epoch++
	if view.MatchesManifest(manifest) {
		t.Fatal("view accepted a manifest from another epoch")
	}
}
