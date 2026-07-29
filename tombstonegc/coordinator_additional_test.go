package tombstonegc

import (
	"testing"

	"github.com/DarkInno/crdt/tree"
)

func TestCoordinatorPreventsResurrectionWhenRemoteHasOldAdd(t *testing.T) {
	t.Parallel()
	codec := stringCodec{}
	source := mustSet(t, "source", codec)
	remote := mustSet(t, "remote", codec)

	oldAdd, err := source.Add("old")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyDelta(oldAdd); err != nil {
		t.Fatal(err)
	}
	oldTag := source.Frontier()["source"]
	removeOld, err := source.Remove("old")
	if err != nil {
		t.Fatal(err)
	}
	laterAdd, err := source.Add("later")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyDelta(laterAdd); err != nil {
		t.Fatal(err)
	}
	if got := remote.Frontier()["source"]; got.Compare(oldTag) <= 0 {
		t.Fatalf("remote frontier = %#v, want a tag after %#v", got, oldTag)
	}
	if !remote.Contains("old") {
		t.Fatal("remote unexpectedly observed the remove delta")
	}

	const groupID = "orders/v1"
	coordinator, err := NewCoordinator[string](groupID, []string{"source", "remote"})
	if err != nil {
		t.Fatal(err)
	}
	epoch := coordinator.Membership().Epoch
	if removed, err := coordinator.AcknowledgeAndCompact(groupID, "source", epoch, source.TombstoneTags(), source); err != nil || removed != 0 {
		t.Fatalf("source acknowledgement compacted = %d, %v; want 0, nil", removed, err)
	}
	if removed, err := coordinator.AcknowledgeAndCompact(groupID, "remote", epoch, remote.TombstoneTags(), source); err != nil || removed != 0 {
		t.Fatalf("remote acknowledgement compacted = %d, %v; want 0, nil", removed, err)
	}
	if err := source.Merge(remote); err != nil {
		t.Fatal(err)
	}
	if source.Contains("old") {
		t.Fatal("old element resurrected after an out-of-order acknowledgement")
	}

	if err := remote.ApplyDelta(removeOld); err != nil {
		t.Fatal(err)
	}
	if removed, err := coordinator.AcknowledgeAndCompact(groupID, "remote", epoch, remote.TombstoneTags(), source); err != nil || removed != 1 {
		t.Fatalf("exact acknowledgement compacted = %d, %v; want 1, nil", removed, err)
	}
}

func TestCoordinatorCompactsORTreeLeafOnlyAfterExactAcknowledgements(t *testing.T) {
	source, err := tree.New("source")
	if err != nil {
		t.Fatal(err)
	}
	leaf, add, err := source.Add(tree.NodeID{}, []byte("leaf"))
	if err != nil {
		t.Fatal(err)
	}
	remove, err := source.Remove(leaf)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := tree.New("remote")
	if err != nil {
		t.Fatal(err)
	}
	// The remove is deliberately delivered first to model an unreliable link.
	if err := remote.ApplyDelta(remove); err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyDelta(add); err != nil {
		t.Fatal(err)
	}

	const groupID = "document/tree/v1"
	coordinator, err := NewCoordinator[struct{}](groupID, []string{"source", "remote"})
	if err != nil {
		t.Fatal(err)
	}
	epoch := coordinator.Membership().Epoch
	if removed, err := coordinator.AcknowledgeAndCompactTarget(groupID, "source", epoch, source.TombstoneTags(), source); err != nil || removed != 0 {
		t.Fatalf("source acknowledgement compacted = %d, %v; want 0, nil", removed, err)
	}
	if removed, err := coordinator.AcknowledgeAndCompactTarget(groupID, "remote", epoch, remote.TombstoneTags(), source); err != nil || removed != 1 {
		t.Fatalf("remote acknowledgement compacted = %d, %v; want 1, nil", removed, err)
	}
	if state := source.State(); state.ElementCount != 0 || state.TombstoneCount != 0 {
		t.Fatalf("compacted tree state = %#v", state)
	}
	if removed, err := coordinator.AcknowledgeAndCompactTarget(groupID, "remote", epoch, nil, nil); err != ErrNilTarget || removed != 0 {
		t.Fatalf("nil target acknowledgement = %d, %v; want 0, %v", removed, err, ErrNilTarget)
	}
}
