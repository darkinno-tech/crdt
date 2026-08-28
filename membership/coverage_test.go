package membership

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/im10furry/crdt"
)

type failingStore struct {
	view    View
	ok      bool
	loadErr error
	saveErr error
}

func (s *failingStore) LoadView() (View, bool, error) { return s.view, s.ok, s.loadErr }
func (s *failingStore) SaveView(View) error           { return s.saveErr }

func TestMembershipRejectsInvalidControlPlaneInputs(t *testing.T) {
	t.Parallel()
	setup := newFixture(t, 1, "api", "mobile")
	if _, err := SignView(View{}, setup.authorityPrivate); !errors.Is(err, ErrInvalidView) {
		t.Fatalf("empty view sign = %v", err)
	}
	if err := VerifyView(setup.view, ed25519.PublicKey{}); !errors.Is(err, ErrInvalidView) {
		t.Fatalf("short authority key = %v", err)
	}
	tampered := cloneView(setup.view)
	tampered.Signature[0] ^= 1
	if err := VerifyView(tampered, setup.authorityPublic); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered view = %v", err)
	}
	if _, err := NewManager[string](setup.view, setup.authorityPublic, nil); !errors.Is(err, ErrInvalidView) {
		t.Fatalf("nil store manager = %v", err)
	}
	if _, err := OpenManager[string](setup.authorityPublic, nil); !errors.Is(err, ErrMissingView) {
		t.Fatalf("nil store open = %v", err)
	}
	if _, err := OpenManager[string](setup.authorityPublic, &MemoryStore{}); !errors.Is(err, ErrMissingView) {
		t.Fatalf("empty store open = %v", err)
	}
	if _, err := OpenManager[string](setup.authorityPublic, &failingStore{loadErr: errors.New("load")}); err == nil {
		t.Fatal("load failure accepted")
	}
	if _, err := NewManager[string](setup.view, setup.authorityPublic, &failingStore{saveErr: errors.New("save")}); err == nil {
		t.Fatal("save failure accepted")
	}

	store := &MemoryStore{}
	manager, err := NewManager[string](setup.view, setup.authorityPublic, store)
	if err != nil {
		t.Fatal(err)
	}
	wrongGroup, err := SignView(View{GroupID: "other/v1", Epoch: 2, PreviousHash: setup.view.Hash(), ManifestHash: setup.view.ManifestHash, Members: setup.view.Members}, setup.authorityPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(wrongGroup); !errors.Is(err, ErrGroupMismatch) {
		t.Fatalf("wrong group install = %v", err)
	}
	gap, err := SignView(View{GroupID: setup.view.GroupID, Epoch: 3, PreviousHash: setup.view.Hash(), ManifestHash: setup.view.ManifestHash, Members: setup.view.Members}, setup.authorityPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(gap); !errors.Is(err, ErrViewFork) {
		t.Fatalf("epoch gap install = %v", err)
	}
	fork, err := SignView(View{GroupID: setup.view.GroupID, Epoch: 2, ManifestHash: setup.view.ManifestHash, Members: setup.view.Members}, setup.authorityPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(fork); !errors.Is(err, ErrViewFork) {
		t.Fatalf("wrong predecessor install = %v", err)
	}
	var nilManager *Manager[string]
	if got := nilManager.View(); got.GroupID != "" || got.Epoch != 0 || nilManager.Coordinator() != nil {
		t.Fatal("nil manager access was not inert")
	}
}

func TestMembershipRejectsInvalidGossipAndReceipts(t *testing.T) {
	t.Parallel()
	setup := newFixture(t, 1, "api", "mobile")
	if _, err := NewGossip(setup.view, "missing", setup.members["api"], time.Second); !errors.Is(err, ErrInvalidGossip) {
		t.Fatalf("unknown gossip member = %v", err)
	}
	if _, err := NewGossip(setup.view, "api", setup.members["mobile"], time.Second); !errors.Is(err, ErrInvalidGossip) {
		t.Fatalf("wrong gossip key = %v", err)
	}
	if _, err := NewGossip(setup.view, "api", setup.members["api"], 0); !errors.Is(err, ErrInvalidGossip) {
		t.Fatalf("zero suspect duration = %v", err)
	}
	var nilGossip *Gossip
	if _, err := nilGossip.Heartbeat(); !errors.Is(err, ErrInvalidGossip) || nilGossip.Peers(1) != nil || nilGossip.Suspects(time.Now()) != nil {
		t.Fatal("nil gossip behavior")
	}
	gossip, err := NewGossip(setup.view, "api", setup.members["api"], time.Second)
	if err != nil {
		t.Fatal(err)
	}
	message, err := gossip.Heartbeat()
	if err != nil {
		t.Fatal(err)
	}
	message.GroupID = "other/v1"
	if _, _, err := gossip.Observe(message, time.Now()); !errors.Is(err, ErrInvalidGossip) {
		t.Fatalf("wrong group gossip = %v", err)
	}
	if _, _, err := gossip.Observe(GossipMessage{}, time.Time{}); !errors.Is(err, ErrInvalidGossip) {
		t.Fatalf("invalid observe = %v", err)
	}

	manager, err := NewManager[string](setup.view, setup.authorityPublic, &MemoryStore{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewGCBridge[string](nil); !errors.Is(err, ErrInvalidView) {
		t.Fatalf("nil bridge manager = %v", err)
	}
	bridge, err := NewGCBridge(manager)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Apply(Receipt{}, nil); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("nil receipt target = %v", err)
	}
	if _, err := SortedTags([]crdt.Tag{{}}); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("invalid sorted tag = %v", err)
	}
	if _, err := SortedTags([]crdt.Tag{{ReplicaID: "api", WallTime: 1}, {ReplicaID: "api", WallTime: 1}}); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("duplicate sorted tag = %v", err)
	}
	if _, err := SignReceipt(Receipt{}, setup.members["api"]); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("empty receipt sign = %v", err)
	}
}
