package lww

import (
	"math/rand"
	"reflect"
	"testing"

	"github.com/im10furry/crdt/tombstonegc"
)

// TestLWWMapThreeReplicaUnreliableDeliveryAndRecovery exercises the public
// framed receive path: independent writes, an observed delete, duplicated and
// shuffled deltas, then same-ID snapshot recovery.
func TestLWWMapThreeReplicaUnreliableDeliveryAndRecovery(t *testing.T) {
	alice, err := NewMap("alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := NewMap("bob")
	if err != nil {
		t.Fatal(err)
	}
	carol, err := NewMap("carol")
	if err != nil {
		t.Fatal(err)
	}
	title, err := alice.SetWithDelta("title", []byte("draft"))
	if err != nil {
		t.Fatal(err)
	}
	if err := bob.ApplyDelta(title); err != nil {
		t.Fatal(err)
	}
	status, err := bob.SetWithDelta("status", []byte("review"))
	if err != nil {
		t.Fatal(err)
	}
	cover, err := carol.SetWithDelta("cover", []byte("object-42"))
	if err != nil {
		t.Fatal(err)
	}
	removeTitle, err := alice.DeleteWithDelta("title")
	if err != nil {
		t.Fatal(err)
	}

	changes := []MapDelta{title, status, cover, removeTitle}
	for index, target := range []*Map{alice, bob, carol} {
		deliverMapChanges(t, target, changes, int64(20260729+index))
	}
	wantKeys := []string{"cover", "status"}
	for _, target := range []*Map{alice, bob, carol} {
		if keys := target.Keys(); !reflect.DeepEqual(keys, wantKeys) {
			t.Fatalf("keys = %#v, want %#v", keys, wantKeys)
		}
		if _, ok := target.Get("title"); ok {
			t.Fatal("observed delete did not win after duplicate delivery")
		}
	}

	saved, err := bob.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewMapFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	deliverMapChanges(t, recovered, changes, 20260801)
	if keys := recovered.Keys(); !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("recovered keys = %#v, want %#v", keys, wantKeys)
	}
}

func deliverMapChanges(t testing.TB, target *Map, changes []MapDelta, seed int64) {
	t.Helper()
	frames := make([][]byte, 0, len(changes)*2)
	for _, change := range changes {
		encoded, err := change.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, encoded, encoded)
	}
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(frames), func(left, right int) { frames[left], frames[right] = frames[right], frames[left] })
	for _, encoded := range frames {
		change, err := UnmarshalMapDelta(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if err := target.ApplyDelta(change); err != nil {
			t.Fatal(err)
		}
	}
}

// TestLWWMapExactAcknowledgementCompaction models the application-owned GC
// hand-off: a later delivery cannot stand in for the exact delete, and only a
// complete current-membership acknowledgement permits removal.
func TestLWWMapExactAcknowledgementCompaction(t *testing.T) {
	source, err := NewMap("source")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := NewMap("remote")
	if err != nil {
		t.Fatal(err)
	}
	old, err := source.SetWithDelta("old", []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	remove, err := source.DeleteWithDelta("old")
	if err != nil {
		t.Fatal(err)
	}
	later, err := source.SetWithDelta("later", []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyDelta(old); err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyDelta(later); err != nil {
		t.Fatal(err)
	}

	const groupID = "document/lww-map/v1"
	coordinator, err := tombstonegc.NewCoordinator[struct{}](groupID, []string{"source", "remote"})
	if err != nil {
		t.Fatal(err)
	}
	epoch := coordinator.Membership().Epoch
	if removed, err := coordinator.AcknowledgeAndCompactTarget(groupID, "source", epoch, source.TombstoneTags(), source); err != nil || removed != 0 {
		t.Fatalf("source acknowledgement = %d, %v", removed, err)
	}
	if removed, err := coordinator.AcknowledgeAndCompactTarget(groupID, "remote", epoch, remote.TombstoneTags(), source); err != nil || removed != 0 {
		t.Fatalf("later tag treated as delete acknowledgement = %d, %v", removed, err)
	}
	if err := remote.ApplyDelta(remove); err != nil {
		t.Fatal(err)
	}
	if removed, err := coordinator.AcknowledgeAndCompactTarget(groupID, "remote", epoch, remote.TombstoneTags(), source); err != nil || removed != 1 {
		t.Fatalf("exact acknowledgement = %d, %v", removed, err)
	}
	if state := source.State(); state.TombstoneCount != 0 {
		t.Fatalf("compacted source state = %#v", state)
	}
}
