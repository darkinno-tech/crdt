package tombstonegc

import "testing"

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
