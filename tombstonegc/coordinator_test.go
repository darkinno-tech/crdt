package tombstonegc

import (
	"errors"
	"testing"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/set"
)

type stringCodec struct{}

func (stringCodec) ID() string                            { return "example.com/tombstone-gc-string/v1" }
func (stringCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (stringCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }

func TestCoordinatorWaitsForExactAcknowledgementDespiteOutOfOrderDelivery(t *testing.T) {
	t.Parallel()
	codec := stringCodec{}
	source := mustSet(t, "source", codec)
	remote := mustSet(t, "remote", codec)

	_, err := source.Add("old")
	if err != nil {
		t.Fatal(err)
	}
	oldTag := source.Frontier()["source"]
	removeOld, err := source.Remove("old")
	if err != nil {
		t.Fatal(err)
	}
	newAdd, err := source.Add("new")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyDelta(newAdd); err != nil {
		t.Fatal(err)
	}
	if got := remote.Frontier()["source"]; !got.Valid() || got.Compare(oldTag) <= 0 {
		t.Fatalf("remote frontier = %#v; test did not create a later out-of-order tag", got)
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
		t.Fatalf("out-of-order acknowledgement compacted = %d, %v; want 0, nil", removed, err)
	}
	if got := source.State().TombstoneCount; got != 1 {
		t.Fatalf("tombstones after incomplete acknowledgement = %d, want 1", got)
	}
	if err := remote.ApplyDelta(removeOld); err != nil {
		t.Fatal(err)
	}
	if removed, err := coordinator.AcknowledgeAndCompact(groupID, "remote", epoch, remote.TombstoneTags(), source); err != nil || removed != 1 {
		t.Fatalf("exact acknowledgement compacted = %d, %v; want 1, nil", removed, err)
	}
	if got := source.State().TombstoneCount; got != 0 {
		t.Fatalf("tombstones after exact acknowledgement = %d, want 0", got)
	}
}

func TestCoordinatorMembershipChangeClearsAcknowledgementsAndRejectsOldEpoch(t *testing.T) {
	t.Parallel()
	codec := stringCodec{}
	value := mustSet(t, "source", codec)
	if _, err := value.Add("item"); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Remove("item"); err != nil {
		t.Fatal(err)
	}
	tags := value.TombstoneTags()
	const groupID = "orders/v1"
	coordinator, err := NewCoordinator[string](groupID, []string{"source", "remote"})
	if err != nil {
		t.Fatal(err)
	}
	oldEpoch := coordinator.Membership().Epoch
	if err := coordinator.Acknowledge(groupID, "source", oldEpoch, tags); err != nil {
		t.Fatal(err)
	}
	updated, err := coordinator.ReplaceMembership([]string{"source", "replacement"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Epoch == oldEpoch {
		t.Fatal("membership epoch did not advance")
	}
	if err := coordinator.Acknowledge(groupID, "source", oldEpoch, tags); !errors.Is(err, ErrStaleMembership) {
		t.Fatalf("old epoch acknowledgement error = %v, want %v", err, ErrStaleMembership)
	}
	if removed, err := coordinator.AcknowledgeAndCompact(groupID, "replacement", updated.Epoch, tags, value); err != nil || removed != 0 {
		t.Fatalf("replacement acknowledgement compacted = %d, %v; want 0, nil", removed, err)
	}
	if removed, err := coordinator.AcknowledgeAndCompact(groupID, "source", updated.Epoch, tags, value); err != nil || removed != 1 {
		t.Fatalf("new epoch acknowledgements compacted = %d, %v; want 1, nil", removed, err)
	}
}

func TestCoordinatorRejectsInvalidInputsWithoutCompaction(t *testing.T) {
	t.Parallel()
	codec := stringCodec{}
	value := mustSet(t, "source", codec)
	if _, err := value.Add("item"); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Remove("item"); err != nil {
		t.Fatal(err)
	}
	const groupID = "orders/v1"
	coordinator, err := NewCoordinator[string](groupID, []string{"source"})
	if err != nil {
		t.Fatal(err)
	}
	epoch := coordinator.Membership().Epoch
	if got := coordinator.Membership().GroupID; got != groupID {
		t.Fatalf("Membership().GroupID = %q, want %q", got, groupID)
	}
	if err := coordinator.Acknowledge("other-group", "source", epoch, nil); !errors.Is(err, ErrGroupMismatch) {
		t.Fatalf("mismatched group error = %v, want %v", err, ErrGroupMismatch)
	}
	if err := coordinator.Acknowledge(groupID, "unknown", epoch, nil); !errors.Is(err, ErrUnknownMember) {
		t.Fatalf("unknown member error = %v, want %v", err, ErrUnknownMember)
	}
	if err := coordinator.Acknowledge(groupID, "source", epoch, []crdt.Tag{{}}); !errors.Is(err, ErrInvalidTag) {
		t.Fatalf("invalid tag error = %v, want %v", err, ErrInvalidTag)
	}
	if got := value.State().TombstoneCount; got != 1 {
		t.Fatalf("invalid acknowledgement changed tombstones: %d", got)
	}
	if removed, err := coordinator.AcknowledgeAndCompact(groupID, "unknown", epoch, nil, value); !errors.Is(err, ErrUnknownMember) || removed != 0 {
		t.Fatalf("unknown compact acknowledgement = %d, %v; want 0, %v", removed, err, ErrUnknownMember)
	}
	if removed, err := coordinator.AcknowledgeAndCompact(groupID, "source", epoch, nil, nil); !errors.Is(err, set.ErrNilORSet) || removed != 0 {
		t.Fatalf("nil target acknowledgement = %d, %v; want 0, %v", removed, err, set.ErrNilORSet)
	}
}

func TestCoordinatorValidatesMembership(t *testing.T) {
	t.Parallel()
	for _, members := range [][]string{nil, {""}, {"one", "one"}} {
		if _, err := NewCoordinator[string]("orders/v1", members); !errors.Is(err, ErrInvalidMembership) {
			t.Fatalf("NewCoordinator(%#v) error = %v, want %v", members, err, ErrInvalidMembership)
		}
	}
	if _, err := NewCoordinator[string](" ", []string{"one"}); !errors.Is(err, ErrInvalidGroup) {
		t.Fatalf("invalid group error = %v, want %v", err, ErrInvalidGroup)
	}
}

func TestCoordinatorAcknowledgesSetAndNilCoordinatorFailsClosed(t *testing.T) {
	t.Parallel()
	codec := stringCodec{}
	value := mustSet(t, "source", codec)
	if _, err := value.Add("item"); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Remove("item"); err != nil {
		t.Fatal(err)
	}
	const groupID = "orders/v1"
	coordinator, err := NewCoordinator[string](groupID, []string{"source"})
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := coordinator.AcknowledgeSetAndCompact(groupID, "source", coordinator.Membership().Epoch, value, value); err != nil || removed != 1 {
		t.Fatalf("AcknowledgeSetAndCompact() = %d, %v; want 1, nil", removed, err)
	}
	if removed, err := coordinator.AcknowledgeSetAndCompact(groupID, "source", coordinator.Membership().Epoch, nil, value); !errors.Is(err, set.ErrNilORSet) || removed != 0 {
		t.Fatalf("nil acknowledged set = %d, %v; want 0, %v", removed, err, set.ErrNilORSet)
	}

	var nilCoordinator *Coordinator[string]
	if got := nilCoordinator.Membership(); got.Epoch != 0 || len(got.Members) != 0 {
		t.Fatalf("nil Membership() = %#v", got)
	}
	if got := nilCoordinator.AcknowledgementStats(); got != (AcknowledgementStats{}) {
		t.Fatalf("nil AcknowledgementStats() = %#v", got)
	}
	if _, err := nilCoordinator.ReplaceMembership([]string{"source"}); !errors.Is(err, ErrInvalidMembership) {
		t.Fatalf("nil ReplaceMembership() error = %v, want %v", err, ErrInvalidMembership)
	}
	if err := nilCoordinator.Acknowledge(groupID, "source", 1, nil); !errors.Is(err, ErrInvalidMembership) {
		t.Fatalf("nil Acknowledge() error = %v, want %v", err, ErrInvalidMembership)
	}
	if _, err := nilCoordinator.PruneAcknowledgements(groupID, 1, nil); !errors.Is(err, ErrInvalidMembership) {
		t.Fatalf("nil PruneAcknowledgements() error = %v, want %v", err, ErrInvalidMembership)
	}
}

func TestCoordinatorCountsOnlyNewAcknowledgementsAndResetsForNewEpoch(t *testing.T) {
	t.Parallel()
	const groupID = "orders/v1"
	tag := crdt.Tag{ReplicaID: "source", WallTime: 1}
	coordinator, err := NewCoordinator[string](groupID, []string{"source", "remote"})
	if err != nil {
		t.Fatal(err)
	}
	membership := coordinator.Membership()
	if err := coordinator.Acknowledge(groupID, "source", membership.Epoch, []crdt.Tag{tag, tag}); err != nil {
		t.Fatal(err)
	}

	coordinator.membershipMu.RLock()
	coordinator.acknowledgementMu.Lock()
	if got := coordinator.acknowledgementCounts[tag]; got != 1 {
		coordinator.acknowledgementMu.Unlock()
		coordinator.membershipMu.RUnlock()
		t.Fatalf("source acknowledgement count = %d, want 1", got)
	}
	if got := coordinator.acknowledgementEntries; got != 1 {
		coordinator.acknowledgementMu.Unlock()
		coordinator.membershipMu.RUnlock()
		t.Fatalf("source acknowledgement entries = %d, want 1", got)
	}
	if stable := coordinator.stableTombstonesLocked([]crdt.Tag{tag}); len(stable) != 0 {
		coordinator.acknowledgementMu.Unlock()
		coordinator.membershipMu.RUnlock()
		t.Fatalf("stable tombstones after one member = %#v, want none", stable)
	}
	coordinator.acknowledgementMu.Unlock()
	coordinator.membershipMu.RUnlock()

	if err := coordinator.Acknowledge(groupID, "remote", membership.Epoch, []crdt.Tag{tag, tag}); err != nil {
		t.Fatal(err)
	}
	coordinator.membershipMu.RLock()
	coordinator.acknowledgementMu.Lock()
	if got := coordinator.acknowledgementCounts[tag]; got != 2 {
		coordinator.acknowledgementMu.Unlock()
		coordinator.membershipMu.RUnlock()
		t.Fatalf("all-member acknowledgement count = %d, want 2", got)
	}
	if got := coordinator.acknowledgementEntries; got != 2 {
		coordinator.acknowledgementMu.Unlock()
		coordinator.membershipMu.RUnlock()
		t.Fatalf("all-member acknowledgement entries = %d, want 2", got)
	}
	if stable := coordinator.stableTombstonesLocked([]crdt.Tag{tag}); len(stable) != 1 || stable[0] != tag {
		coordinator.acknowledgementMu.Unlock()
		coordinator.membershipMu.RUnlock()
		t.Fatalf("stable tombstones = %#v, want %#v", stable, []crdt.Tag{tag})
	}
	coordinator.acknowledgementMu.Unlock()
	coordinator.membershipMu.RUnlock()

	if _, err := coordinator.ReplaceMembership([]string{"source", "replacement"}); err != nil {
		t.Fatal(err)
	}
	coordinator.membershipMu.RLock()
	coordinator.acknowledgementMu.Lock()
	if got := len(coordinator.acknowledgementCounts); got != 0 {
		coordinator.acknowledgementMu.Unlock()
		coordinator.membershipMu.RUnlock()
		t.Fatalf("acknowledgement counts after membership replacement = %d, want 0", got)
	}
	if got := coordinator.acknowledgementEntries; got != 0 {
		coordinator.acknowledgementMu.Unlock()
		coordinator.membershipMu.RUnlock()
		t.Fatalf("acknowledgement entries after membership replacement = %d, want 0", got)
	}
	coordinator.acknowledgementMu.Unlock()
	coordinator.membershipMu.RUnlock()
}

func TestCoordinatorPrunesAcknowledgementsFailClosed(t *testing.T) {
	t.Parallel()
	codec := stringCodec{}
	target := mustSet(t, "source", codec)
	if _, err := target.Add("item"); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Remove("item"); err != nil {
		t.Fatal(err)
	}
	tags := target.TombstoneTags()
	const groupID = "orders/v1"
	coordinator, err := NewCoordinator[string](groupID, []string{"source", "remote"})
	if err != nil {
		t.Fatal(err)
	}
	membership := coordinator.Membership()
	for _, member := range membership.Members {
		if err := coordinator.Acknowledge(groupID, member, membership.Epoch, tags); err != nil {
			t.Fatal(err)
		}
	}
	if stats := coordinator.AcknowledgementStats(); stats.Tags != 1 || stats.Entries != 2 || stats.Members != 2 {
		t.Fatalf("acknowledgement stats before prune = %#v", stats)
	}
	if removed, err := coordinator.PruneAcknowledgements(groupID, membership.Epoch, tags); err != nil || removed != 2 {
		t.Fatalf("PruneAcknowledgements() = %d, %v; want 2, nil", removed, err)
	}
	if stats := coordinator.AcknowledgementStats(); stats.Tags != 0 || stats.Entries != 0 {
		t.Fatalf("acknowledgement stats after prune = %#v", stats)
	}
	coordinator.membershipMu.RLock()
	coordinator.acknowledgementMu.Lock()
	if got := len(coordinator.acknowledgements); got != 0 {
		coordinator.acknowledgementMu.Unlock()
		coordinator.membershipMu.RUnlock()
		t.Fatalf("full prune retained %d member acknowledgement maps", got)
	}
	coordinator.acknowledgementMu.Unlock()
	coordinator.membershipMu.RUnlock()

	if removed, err := coordinator.AcknowledgeAndCompact(groupID, "source", membership.Epoch, tags, target); err != nil || removed != 0 {
		t.Fatalf("first post-prune acknowledgement = %d, %v; want 0, nil", removed, err)
	}
	if removed, err := coordinator.AcknowledgeAndCompact(groupID, "remote", membership.Epoch, tags, target); err != nil || removed != 1 {
		t.Fatalf("second post-prune acknowledgement = %d, %v; want 1, nil", removed, err)
	}

	if _, err := coordinator.PruneAcknowledgements("other-group", membership.Epoch, tags); !errors.Is(err, ErrGroupMismatch) {
		t.Fatalf("wrong-group prune error = %v, want %v", err, ErrGroupMismatch)
	}
	if _, err := coordinator.PruneAcknowledgements(groupID, membership.Epoch+1, tags); !errors.Is(err, ErrStaleMembership) {
		t.Fatalf("stale prune error = %v, want %v", err, ErrStaleMembership)
	}
	if _, err := coordinator.PruneAcknowledgements(groupID, membership.Epoch, []crdt.Tag{{}}); !errors.Is(err, ErrInvalidTag) {
		t.Fatalf("invalid-tag prune error = %v, want %v", err, ErrInvalidTag)
	}
}

func TestCoordinatorPartialPruneRetainsOtherAcknowledgements(t *testing.T) {
	t.Parallel()
	const groupID = "orders/v1"
	first := crdt.Tag{ReplicaID: "source", WallTime: 1}
	second := crdt.Tag{ReplicaID: "source", WallTime: 2}
	coordinator, err := NewCoordinator[string](groupID, []string{"source", "remote"})
	if err != nil {
		t.Fatal(err)
	}
	membership := coordinator.Membership()
	for _, member := range membership.Members {
		if err := coordinator.Acknowledge(groupID, member, membership.Epoch, []crdt.Tag{first, second}); err != nil {
			t.Fatal(err)
		}
	}
	if removed, err := coordinator.PruneAcknowledgements(groupID, membership.Epoch, []crdt.Tag{first}); err != nil || removed != 2 {
		t.Fatalf("PruneAcknowledgements(first) = %d, %v; want 2, nil", removed, err)
	}
	if stats := coordinator.AcknowledgementStats(); stats.Tags != 1 || stats.Entries != 2 {
		t.Fatalf("stats after partial prune = %#v", stats)
	}
	coordinator.membershipMu.RLock()
	coordinator.acknowledgementMu.Lock()
	if stable := coordinator.stableTombstonesLocked([]crdt.Tag{first}); len(stable) != 0 {
		coordinator.acknowledgementMu.Unlock()
		coordinator.membershipMu.RUnlock()
		t.Fatalf("pruned tag remained stable: %#v", stable)
	}
	if stable := coordinator.stableTombstonesLocked([]crdt.Tag{second}); len(stable) != 1 || stable[0] != second {
		coordinator.acknowledgementMu.Unlock()
		coordinator.membershipMu.RUnlock()
		t.Fatalf("unrelated tag lost acknowledgement: %#v", stable)
	}
	coordinator.acknowledgementMu.Unlock()
	coordinator.membershipMu.RUnlock()
}

func TestCoordinatorRejectsInvalidReplacementAndEpochOverflow(t *testing.T) {
	coordinator, err := NewCoordinator[string]("orders/v1", []string{"source"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.ReplaceMembership([]string{"", "replacement"}); !errors.Is(err, ErrInvalidMembership) {
		t.Fatalf("invalid replacement error = %v, want %v", err, ErrInvalidMembership)
	}
	coordinator.epoch = ^uint64(0)
	if _, err := coordinator.ReplaceMembership([]string{"replacement"}); !errors.Is(err, ErrInvalidMembership) {
		t.Fatalf("overflow replacement error = %v, want %v", err, ErrInvalidMembership)
	}
}

func mustSet(t testing.TB, replicaID string, codec stringCodec) *set.ORSet[string] {
	t.Helper()
	value, err := set.NewORSet(replicaID, codec)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
