package membership

import (
	"crypto/sha256"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/set"
)

// TestRetireFenceCompactAndBootstrap is a production-shaped three-replica
// exercise: one replica misses a remove during a partition, an authority
// retires it, current members compact only after a new epoch, and the retired
// replica rebuilds from a post-compaction state before it can rejoin.
func TestRetireFenceCompactAndBootstrap(t *testing.T) {
	setup := newFixture(t, 1, "api", "mobile", "warehouse")
	manager, err := NewManager[string](setup.view, setup.authorityPublic, &MemoryStore{})
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := NewGCBridge(manager)
	if err != nil {
		t.Fatal(err)
	}
	api := mustORSet(t, "api")
	mobile := mustORSet(t, "mobile")
	warehousePartitioned := mustORSet(t, "warehouse")

	add, err := api.Add("order-42")
	if err != nil {
		t.Fatal(err)
	}
	if err := mobile.ApplyDelta(add); err != nil {
		t.Fatal(err)
	}
	if err := warehousePartitioned.ApplyDelta(add); err != nil {
		t.Fatal(err)
	}
	remove, err := api.Remove("order-42")
	if err != nil {
		t.Fatal(err)
	}
	if err := mobile.ApplyDelta(remove); err != nil {
		t.Fatal(err)
	}
	tags, err := SortedTags(api.TombstoneTags())
	if err != nil || len(tags) != 1 {
		t.Fatalf("remove tags = %#v, %v", tags, err)
	}

	// The partitioned warehouse cannot acknowledge. Exact receipts from only two
	// of three active members deliberately leave the tombstone retained.
	for sequence, memberID := range []string{"api", "mobile"} {
		receipt := signedReceipt(t, setup, setup.view, memberID, uint64(sequence+1), tags)
		if removed, err := bridge.Apply(receipt, api); err != nil || removed != 0 {
			t.Fatalf("pre-retirement apply = %d, %v", removed, err)
		}
	}
	if got := api.TombstoneTags(); len(got) != 1 {
		t.Fatalf("partition compacted early: %#v", got)
	}

	// An authority, not gossip, publishes a new fenced view excluding warehouse.
	nextManifestHash := sha256.Sum256([]byte("orders/or-set/v1/epoch-2"))
	next, err := SignView(View{
		GroupID:      setup.view.GroupID,
		Epoch:        2,
		PreviousHash: setup.view.Hash(),
		ManifestHash: nextManifestHash,
		Members:      setup.view.Members[:2],
	}, setup.authorityPrivate)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(next); err != nil {
		t.Fatal(err)
	}
	for sequence, memberID := range []string{"api", "mobile"} {
		receipt := signedReceipt(t, setup, next, memberID, uint64(sequence+10), tags)
		removed, err := bridge.Apply(receipt, api)
		if err != nil {
			t.Fatal(err)
		}
		if memberID == "api" && removed != 0 {
			t.Fatalf("first epoch-2 receipt removed %d", removed)
		}
		if memberID == "mobile" && removed != 1 {
			t.Fatalf("last epoch-2 receipt removed %d", removed)
		}
	}
	if got := api.TombstoneTags(); len(got) != 0 {
		t.Fatalf("tombstone was not compacted: %#v", got)
	}

	// An old member cannot cross the new data-plane fence. If it did merge its
	// old live add, it could resurrect the element; the manifest gate forbids it.
	oldManifestHash := sha256.Sum256([]byte("orders/or-set/v1/epoch-1"))
	if next.ManifestHash == oldManifestHash {
		t.Fatal("test manifest hashes unexpectedly equal")
	}
	if members := manager.Coordinator().Membership().Members; len(members) != 2 || members[0] != "api" || members[1] != "mobile" {
		t.Fatalf("fenced membership = %#v", members)
	}

	// Bootstrap discards the fenced replica's old state and retains its HLC
	// identity. It must occur before a new view admits that replica again.
	state, err := api.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	rejoined, err := set.NewORSetFromClock(warehousePartitioned.ClockState(), stringCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := rejoined.UnmarshalBinary(state); err != nil {
		t.Fatal(err)
	}
	if rejoined.Contains("order-42") {
		t.Fatal("bootstrap retained fenced replica's old add")
	}
	if rejoined.ClockState().ReplicaID != "warehouse" {
		t.Fatalf("bootstrap changed logical replica id to %q", rejoined.ClockState().ReplicaID)
	}
}

func mustORSet(t testing.TB, replicaID string) *set.ORSet[string] {
	t.Helper()
	value, err := set.NewORSet(replicaID, stringCodec{})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func signedReceipt(t testing.TB, setup fixture, view View, memberID string, sequence uint64, tags []crdt.Tag) Receipt {
	t.Helper()
	receipt, err := SignReceipt(Receipt{
		GroupID:     view.GroupID,
		Epoch:       view.Epoch,
		ViewHash:    view.Hash(),
		MemberID:    memberID,
		Incarnation: 1,
		Sequence:    sequence,
		Tags:        tags,
	}, setup.members[memberID])
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}
