package tree

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/replica"
	"github.com/DarkInno/crdt/tombstonegc"
)

func TestStableFrameTypeUsesObservedRemoveTreeV1(t *testing.T) {
	if got, want := StableFrameType(), (crdt.FrameType{StateID: crdt.TypeIDORTreeState, DeltaID: crdt.TypeIDORTreeDelta, UsesHLC: true}); got != want {
		t.Fatalf("StableFrameType() = %#v, want %#v", got, want)
	}
	if SemanticsVersion != 1 {
		t.Fatalf("SemanticsVersion = %d, want 1", SemanticsVersion)
	}
}

func TestORTreeEligibleCompactionRemovesDeletedBranchLeafFirst(t *testing.T) {
	value, err := New("writer")
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := value.Add(NodeID{}, []byte("root"))
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := value.Add(root, []byte("child"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, _, err := value.Add(child, []byte("leaf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []NodeID{root, child, leaf} {
		if _, err := value.Remove(id); err != nil {
			t.Fatal(err)
		}
	}
	tags := value.TombstoneTags()
	if removed, err := value.CompactTombstones(tags); !errors.Is(err, ErrUnsafeCompaction) || removed != 0 {
		t.Fatalf("CompactTombstones() = %d, %v; want 0, %v", removed, err, ErrUnsafeCompaction)
	}
	unknown := NodeID{ReplicaID: "other", WallTime: 1}
	batch := append([]NodeID{unknown, tags[0]}, tags...)
	if removed, err := value.CompactEligibleTombstones(batch); err != nil || removed != 3 {
		t.Fatalf("CompactEligibleTombstones() = %d, %v; want 3, nil", removed, err)
	}
	if state := value.State(); state.ElementCount != 0 || state.TombstoneCount != 0 {
		t.Fatalf("compacted state = %#v", state)
	}
	if removed, err := value.CompactEligibleTombstones(nil); err != nil || removed != 0 {
		t.Fatalf("empty CompactEligibleTombstones() = %d, %v", removed, err)
	}
	if _, err := value.CompactEligibleTombstones([]NodeID{{}}); !errors.Is(err, ErrUnsafeCompaction) {
		t.Fatalf("invalid CompactEligibleTombstones() = %v", err)
	}

	var nilTree *ORTree
	if _, err := nilTree.CompactEligibleTombstones(nil); !errors.Is(err, ErrNilTree) {
		t.Fatalf("nil CompactEligibleTombstones() = %v", err)
	}
}

func TestORTreeEligibleCompactionRetainsUnselectedStructuralChild(t *testing.T) {
	value, err := New("writer")
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := value.Add(NodeID{}, []byte("root"))
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := value.Add(root, []byte("child"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Remove(root); err != nil {
		t.Fatal(err)
	}
	if removed, err := value.CompactEligibleTombstones(value.TombstoneTags()); err != nil || removed != 0 {
		t.Fatalf("root-only CompactEligibleTombstones() = %d, %v", removed, err)
	}
	if _, err := value.Remove(child); err != nil {
		t.Fatal(err)
	}
	if removed, err := value.CompactEligibleTombstones(value.TombstoneTags()); err != nil || removed != 2 {
		t.Fatalf("complete branch CompactEligibleTombstones() = %d, %v", removed, err)
	}
}

func TestORTreeStableWireLimitsAndRecovery(t *testing.T) {
	value, err := New("writer")
	if err != nil {
		t.Fatal(err)
	}
	_, delta, err := value.Add(NodeID{}, []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	limits := frame.DefaultLimits()
	limits.MaxFrameBytes = 1
	if _, err := value.MarshalBinaryWithLimits(limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("MarshalBinaryWithLimits() = %v", err)
	}
	if _, err := delta.MarshalBinaryWithLimits(limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("Delta.MarshalBinaryWithLimits() = %v", err)
	}
	if _, _, err := value.MarshalBinaryWithClockStateAndLimits(limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("MarshalBinaryWithClockStateAndLimits() = %v", err)
	}
	if _, err := value.SnapshotCurrentStateWithLimits(limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("SnapshotCurrentStateWithLimits() = %v", err)
	}

	saved, err := value.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewFromSnapshotWithOptionsAndLimits(saved, DefaultOptions(), frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := recovered.Nodes(), value.Nodes(); len(got) != len(want) || string(got[0].Value) != string(want[0].Value) {
		t.Fatalf("recovered nodes = %#v, want %#v", got, want)
	}
	if _, err := NewFromSnapshotWithOptionsAndLimits(saved, DefaultOptions(), limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tight recovery = %v", err)
	}
	if err := validateTreeState([]byte("invalid")); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("validateTreeState(invalid) = %v", err)
	}
	if _, err := json.Marshal(value); err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(delta); err != nil {
		t.Fatal(err)
	}
}

func TestORTreeStableManifestDeliversCanonicalDelta(t *testing.T) {
	manifest, err := replica.NewManifest("outline", "example.com/outline/node-v1", 1, replica.Protocol{
		StateID: crdt.TypeIDORTreeState, DeltaID: crdt.TypeIDORTreeDelta, SemanticsVersion: SemanticsVersion,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	frontier, err := replica.NewFrontier(nil)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := replica.NewInbox(manifest, frontier, 8, frame.DefaultLimits().MaxFrameBytes, func(encoded []byte) error {
		delta, err := UnmarshalDelta(encoded)
		if err != nil {
			return err
		}
		return target.ApplyDelta(delta)
	})
	if err != nil {
		t.Fatal(err)
	}

	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	_, delta, err := source.Add(NodeID{}, []byte("stable"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	change, err := replica.NewChange(manifest, replica.Dot{Actor: "source", Counter: 1}, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if delivery, err := inbox.Receive(change); err != nil || len(delivery.Applied) != 1 {
		t.Fatalf("Receive() = %#v, %v", delivery, err)
	}
	if got, want := target.Nodes(), source.Nodes(); len(got) != len(want) || string(got[0].Value) != string(want[0].Value) {
		t.Fatalf("target nodes = %#v, want %#v", got, want)
	}
}

// TestORTreeThreeReplicaCompactionSimulation models unreliable delta delivery
// followed by authenticated exact acknowledgements. The coordinator supplies
// eligibility only; tree v1 frees the deleted branch in leaf-to-root order.
func TestORTreeThreeReplicaCompactionSimulation(t *testing.T) {
	source, err := New("author")
	if err != nil {
		t.Fatal(err)
	}
	root, addRoot, err := source.Add(NodeID{}, []byte("document"))
	if err != nil {
		t.Fatal(err)
	}
	child, addChild, err := source.Add(root, []byte("section"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, addLeaf, err := source.Add(child, []byte("paragraph"))
	if err != nil {
		t.Fatal(err)
	}
	removeRoot, err := source.Remove(root)
	if err != nil {
		t.Fatal(err)
	}
	removeChild, err := source.Remove(child)
	if err != nil {
		t.Fatal(err)
	}
	removeLeaf, err := source.Remove(leaf)
	if err != nil {
		t.Fatal(err)
	}
	changes := []Delta{addRoot, addChild, addLeaf, removeRoot, removeChild, removeLeaf}

	replicas := map[string]*ORTree{"author": source}
	for replicaID, seed := range map[string]int64{"mobile": 20260730, "desktop": 20260731} {
		target, err := New(replicaID)
		if err != nil {
			t.Fatal(err)
		}
		deliverTreeDeltas(t, target, changes, seed)
		if state := target.State(); state.ElementCount != 0 || state.TombstoneCount != 3 {
			t.Fatalf("%s state after reordered delivery = %#v", replicaID, state)
		}
		replicas[replicaID] = target
	}

	coordinator, err := tombstonegc.NewCoordinator[struct{}]("documents/outline/v1", []string{"author", "desktop", "mobile"})
	if err != nil {
		t.Fatal(err)
	}
	membership := coordinator.Membership()
	for index, member := range []string{"mobile", "author", "desktop"} {
		removed, err := coordinator.AcknowledgeAndCompactTarget(membership.GroupID, member, membership.Epoch, replicas[member].TombstoneTags(), source)
		if err != nil {
			t.Fatal(err)
		}
		if index < 2 && removed != 0 {
			t.Fatalf("early compaction after %s acknowledgement = %d", member, removed)
		}
		if index == 2 && removed != 3 {
			t.Fatalf("final compaction = %d, want 3", removed)
		}
	}
	if state := source.State(); state.ElementCount != 0 || state.TombstoneCount != 0 {
		t.Fatalf("source state after compaction = %#v", state)
	}
}
