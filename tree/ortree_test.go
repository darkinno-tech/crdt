package tree

import (
	"errors"
	"reflect"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

func TestORTreeConvergesWithRemoveAndOutOfOrderDelivery(t *testing.T) {
	left, err := New("left")
	if err != nil {
		t.Fatal(err)
	}
	right, err := New("right")
	if err != nil {
		t.Fatal(err)
	}
	root, rootDelta, err := left.Add(NodeID{}, []byte("root"))
	if err != nil {
		t.Fatal(err)
	}
	_, childDelta, err := left.Add(root, []byte("child"))
	if err != nil {
		t.Fatal(err)
	}
	remove, err := left.Remove(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := right.ApplyDelta(remove); err != nil {
		t.Fatal(err)
	}
	if err := right.ApplyDelta(childDelta); err != nil {
		t.Fatal(err)
	}
	if err := right.ApplyDelta(rootDelta); err != nil {
		t.Fatal(err)
	}
	if got := right.Nodes(); len(got) != 0 {
		t.Fatalf("nodes = %#v", got)
	}
	if got := right.State().ElementCount; got != 0 {
		t.Fatalf("visible element count = %d, want 0", got)
	}
	if err := left.Merge(right); err != nil {
		t.Fatal(err)
	}
	if len(left.Nodes()) != len(right.Nodes()) {
		t.Fatal("replicas diverged")
	}
}

// TestORTreeTombstoneLifecycleSurvivesOutOfOrderRecovery ensures a parent
// tombstone remains an observed-remove barrier across the full lifecycle:
// delayed child/parent frames, a persisted checkpoint, and duplicate replay.
// Before a rebase, retaining this tombstone is required to prevent an old
// anchor from making its hidden subtree visible again.
func TestORTreeTombstoneLifecycleSurvivesOutOfOrderRecovery(t *testing.T) {
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	parent, parentDelta, err := source.Add(NodeID{}, []byte("parent"))
	if err != nil {
		t.Fatal(err)
	}
	child, childDelta, err := source.Add(parent, []byte("child"))
	if err != nil {
		t.Fatal(err)
	}
	removeDelta, err := source.Remove(parent)
	if err != nil {
		t.Fatal(err)
	}

	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []Delta{removeDelta, childDelta, parentDelta, childDelta, parentDelta, removeDelta} {
		if err := target.ApplyDelta(delta); err != nil {
			t.Fatal(err)
		}
	}
	if nodes := target.Nodes(); len(nodes) != 0 {
		t.Fatalf("delayed frames resurrected hidden subtree: %#v", nodes)
	}
	if state := target.State(); state.ElementCount != 0 || state.TombstoneCount != 1 {
		t.Fatalf("tombstone state = %#v", state)
	}

	saved, err := target.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []Delta{parentDelta, childDelta, removeDelta} {
		if err := recovered.ApplyDelta(delta); err != nil {
			t.Fatal(err)
		}
	}
	if nodes := recovered.Nodes(); len(nodes) != 0 {
		t.Fatalf("replayed frames resurrected checkpointed subtree: %#v", nodes)
	}
	if state := recovered.State(); state.ElementCount != 0 || state.TombstoneCount != 1 {
		t.Fatalf("recovered tombstone state = %#v", state)
	}
	if _, _, err := recovered.Add(child, []byte("grandchild")); !errors.Is(err, ErrUnknownParent) {
		t.Fatalf("Add below tombstoned ancestor = %v, want %v", err, ErrUnknownParent)
	}
}

func TestORTreeRejectsUnknownParentsAndCycles(t *testing.T) {
	value, err := New("local")
	if err != nil {
		t.Fatal(err)
	}
	unknown := NodeID{ReplicaID: "other", WallTime: 1}
	if _, _, err := value.Add(unknown, nil); err != ErrUnknownParent {
		t.Fatalf("unknown parent = %v", err)
	}
	first := NodeID{ReplicaID: "other", WallTime: 1}
	second := NodeID{ReplicaID: "other", WallTime: 2}
	cycle := Delta{nodes: map[NodeID]storedNode{first: {parent: second}, second: {parent: first}}, tombstones: map[NodeID]struct{}{}}
	if err := value.ApplyDelta(cycle); err != ErrInvalidDelta {
		t.Fatalf("cycle = %v", err)
	}
	if _, err := value.Remove(first); err != ErrUnknownNode {
		t.Fatalf("unknown remove = %v", err)
	}
}

func TestORTreeCopiesValuesAndNilPaths(t *testing.T) {
	value, err := New("local")
	if err != nil {
		t.Fatal(err)
	}
	input := []byte("node")
	_, _, err = value.Add(NodeID{}, input)
	if err != nil {
		t.Fatal(err)
	}
	input[0] = 'X'
	nodes := value.Nodes()
	if string(nodes[0].Value) != "node" {
		t.Fatalf("value = %q", nodes[0].Value)
	}
	nodes[0].Value[0] = 'Y'
	if string(value.Nodes()[0].Value) != "node" {
		t.Fatal("Nodes aliases state")
	}
	var nilTree *ORTree
	if _, _, err := nilTree.Add(NodeID{}, nil); err != ErrNilTree {
		t.Fatalf("nil Add = %v", err)
	}
	if nilTree.Nodes() != nil || nilTree.State().Type != "ortree" {
		t.Fatal("nil accessors")
	}
}

func TestORTreeBinaryRoundTrip(t *testing.T) {
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := source.Add(NodeID{}, []byte("root"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := source.Add(root, []byte("child")); err != nil {
		t.Fatal(err)
	}
	first, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.MarshalBinary()
	if err != nil || string(first) != string(second) {
		t.Fatalf("noncanonical state: %v", err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.UnmarshalBinary(first); err != nil {
		t.Fatal(err)
	}
	if len(target.Nodes()) != 2 {
		t.Fatalf("nodes = %#v", target.Nodes())
	}
	if got := target.State().ElementCount; got != 2 {
		t.Fatalf("state element count = %d, want 2", got)
	}
	saved, err := source.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Nodes()) != 2 {
		t.Fatalf("restored nodes = %#v", restored.Nodes())
	}
	decoded, err := New("decoded")
	if err != nil {
		t.Fatal(err)
	}
	if err := decoded.UnmarshalBinary(saved.Bytes()); err != nil {
		t.Fatal(err)
	}
	decoded.mu.RLock()
	wantFrontier := treeFrontier(decoded.nodes, decoded.tombstones)
	decoded.mu.RUnlock()
	if got := saved.Frontier(); !reflect.DeepEqual(got, wantFrontier) {
		t.Fatalf("snapshot frontier = %#v, want %#v", got, wantFrontier)
	}
	if saved.TypeID != crdt.TypeIDORTreeState || saved.FormatVersion != frame.FormatVersion {
		t.Fatalf("snapshot metadata = type %d, format %d", saved.TypeID, saved.FormatVersion)
	}
}

func TestORTreeStateMarshalRejectsUnresolvedParent(t *testing.T) {
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	root, rootDelta, err := source.Add(NodeID{}, []byte("root"))
	if err != nil {
		t.Fatal(err)
	}
	_, childDelta, err := source.Add(root, []byte("child"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(childDelta); err != nil {
		t.Fatal(err)
	}
	if _, err := target.MarshalBinary(); err != ErrIncompleteState {
		t.Fatalf("MarshalBinary() error = %v, want %v", err, ErrIncompleteState)
	}
	if err := target.ApplyDelta(rootDelta); err != nil {
		t.Fatal(err)
	}
	if _, err := target.MarshalBinary(); err != nil {
		t.Fatalf("complete MarshalBinary() error = %v", err)
	}
}

func TestORTreeRejectsAddBelowHiddenAncestor(t *testing.T) {
	value, err := New("local")
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
	if _, _, err := value.Add(child, []byte("hidden")); !errors.Is(err, ErrUnknownParent) {
		t.Fatalf("Add below hidden ancestor = %v, want %v", err, ErrUnknownParent)
	}
}

func TestORTreeOptionsRejectResourceLimitsAtomically(t *testing.T) {
	limits := Options{MaxNodes: 1, MaxTombstones: 1, MaxValueBytes: 2}
	if _, err := NewWithOptions("invalid", Options{}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("NewWithOptions(invalid) = %v", err)
	}

	value, err := NewWithOptions("limited", limits)
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := value.Add(NodeID{}, []byte("ok"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := value.Add(root, nil); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("Add over node limit = %v", err)
	}
	if _, _, err := value.Add(NodeID{}, []byte("long")); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("Add over value limit = %v", err)
	}
	if nodes := value.Nodes(); len(nodes) != 1 || nodes[0].ID != root {
		t.Fatalf("rejected Add changed state: %#v", nodes)
	}
	if _, err := value.Remove(root); err != nil {
		t.Fatal(err)
	}
	extraTombstone := NodeID{ReplicaID: "remote", WallTime: 1}
	if err := value.ApplyDelta(Delta{nodes: map[NodeID]storedNode{}, tombstones: map[NodeID]struct{}{extraTombstone: {}}}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("ApplyDelta over tombstone limit = %v", err)
	}
	if tags := value.TombstoneTags(); len(tags) != 1 || tags[0] != root {
		t.Fatalf("rejected tombstone delta changed state: %#v", tags)
	}

	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot, sourceRootDelta, err := source.Add(NodeID{}, []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	_, sourceChildDelta, err := source.Add(sourceRoot, []byte("b"))
	if err != nil {
		t.Fatal(err)
	}
	combined, err := sourceRootDelta.Merge(sourceChildDelta)
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewWithOptions("target", limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(combined); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("ApplyDelta over node limit = %v", err)
	}
	if nodes := target.Nodes(); len(nodes) != 0 {
		t.Fatalf("rejected node delta changed state: %#v", nodes)
	}
	oversized := Delta{nodes: map[NodeID]storedNode{{ReplicaID: "remote", WallTime: 2}: {value: []byte("long")}}, tombstones: map[NodeID]struct{}{}}
	if err := target.ApplyDelta(oversized); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("ApplyDelta over value limit = %v", err)
	}

	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := target.UnmarshalBinary(state); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("UnmarshalBinary over node limit = %v", err)
	}
	if nodes := target.Nodes(); len(nodes) != 0 {
		t.Fatalf("rejected state changed receiver: %#v", nodes)
	}
	saved, err := source.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewFromSnapshotWithOptions(saved, limits); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("NewFromSnapshotWithOptions over node limit = %v", err)
	}
}

func TestORTreeCompactTombstonesOnlyRemovesExactLeaves(t *testing.T) {
	value, err := New("local")
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
	if removed, err := value.CompactTombstones([]NodeID{root}); !errors.Is(err, ErrUnsafeCompaction) || removed != 0 {
		t.Fatalf("CompactTombstones(parent) = %d, %v", removed, err)
	}
	if _, err := value.Remove(child); err != nil {
		t.Fatal(err)
	}
	if removed, err := value.CompactTombstones([]NodeID{root, child}); !errors.Is(err, ErrUnsafeCompaction) || removed != 0 {
		t.Fatalf("CompactTombstones(parent and child) = %d, %v", removed, err)
	}
	if state := value.State(); state.TombstoneCount != 2 {
		t.Fatalf("failed compact changed tombstones: %#v", state)
	}
	if removed, err := value.CompactTombstones([]NodeID{child, child}); err != nil || removed != 1 {
		t.Fatalf("CompactTombstones(child) = %d, %v", removed, err)
	}
	if removed, err := value.CompactTombstones([]NodeID{root}); err != nil || removed != 1 {
		t.Fatalf("CompactTombstones(root) = %d, %v", removed, err)
	}
	if state := value.State(); state.ElementCount != 0 || state.TombstoneCount != 0 {
		t.Fatalf("compacted state = %#v", state)
	}
	if removed, err := value.CompactTombstones([]NodeID{{ReplicaID: "unknown", WallTime: 1}}); err != nil || removed != 0 {
		t.Fatalf("CompactTombstones(unknown) = %d, %v", removed, err)
	}
	if removed, err := value.CompactTombstones([]NodeID{{}}); !errors.Is(err, ErrUnsafeCompaction) || removed != 0 {
		t.Fatalf("CompactTombstones(invalid) = %d, %v", removed, err)
	}
}

func TestORTreeDeltaWireAndErrorPaths(t *testing.T) {
	if _, err := NewFromClock(clock.State{}); !errors.Is(err, ErrInvalidReplicaID) {
		t.Fatalf("NewFromClock() = %v", err)
	}
	var nilTree *ORTree
	if nilTree.ClockState() != (clock.State{}) || nilTree.Nodes() != nil {
		t.Fatal("nil accessors")
	}
	if _, err := nilTree.MarshalBinary(); !errors.Is(err, ErrNilTree) {
		t.Fatalf("nil MarshalBinary = %v", err)
	}
	if _, _, err := nilTree.MarshalBinaryWithClockState(); !errors.Is(err, ErrNilTree) {
		t.Fatalf("nil MarshalBinaryWithClockState = %v", err)
	}
	if err := nilTree.UnmarshalBinary(nil); !errors.Is(err, ErrNilTree) {
		t.Fatalf("nil UnmarshalBinary = %v", err)
	}

	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	root, delta, err := source.Add(NodeID{}, []byte("root"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalDelta(encoded)
	if err != nil {
		t.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
	if len(target.Nodes()) != 1 {
		t.Fatalf("decoded delta nodes = %#v", target.Nodes())
	}
	if err := target.Merge(target); err != nil {
		t.Fatal(err)
	}
	if err := target.Merge(nil); !errors.Is(err, ErrNilTree) {
		t.Fatalf("Merge(nil) = %v", err)
	}
	if _, err := target.Remove(NodeID{}); !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("Remove(zero) = %v", err)
	}
	if _, err := target.Remove(root); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Remove(root); !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("repeat Remove = %v", err)
	}

	if _, err := (Delta{nodes: map[NodeID]storedNode{}}).MarshalBinary(); err != nil {
		t.Fatalf("empty delta marshal = %v", err)
	}
	bad := Delta{nodes: map[NodeID]storedNode{NodeID{}: {}}}
	if _, err := bad.MarshalBinary(); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("bad delta marshal = %v", err)
	}
	conflict := Delta{nodes: map[NodeID]storedNode{root: {value: []byte("different")}}, tombstones: map[NodeID]struct{}{}}
	if err := target.ApplyDelta(conflict); !errors.Is(err, ErrNodeConflict) {
		t.Fatalf("conflicting ApplyDelta = %v", err)
	}
	if _, err := delta.Merge(conflict); !errors.Is(err, ErrNodeConflict) {
		t.Fatalf("conflicting Delta.Merge = %v", err)
	}
	if err := target.ApplyDelta(Delta{nodes: map[NodeID]storedNode{NodeID{}: {}}, tombstones: map[NodeID]struct{}{}}); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("invalid ApplyDelta = %v", err)
	}

	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalDelta(state); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("state accepted as delta = %v", err)
	}
	limits := frame.DefaultLimits()
	limits.MaxElements = 0
	if _, err := UnmarshalDeltaWithLimits(encoded, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("delta limits = %v", err)
	}
	malformed, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDORTreeState, Payload: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.UnmarshalBinary(malformed); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("malformed state = %v", err)
	}
	if _, err := NewFromSnapshot(snapshot.Snapshot{TypeID: crdt.TypeIDGCounterState}); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("wrong snapshot type = %v", err)
	}
	clockState := source.ClockState()
	withClock, restoredClock, err := source.MarshalBinaryWithClockState()
	if err != nil || restoredClock != clockState || string(withClock) != string(state) {
		t.Fatalf("MarshalBinaryWithClockState = %v, %#v", err, restoredClock)
	}
	saved, err := source.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewFromSnapshot(saved); err != nil {
		t.Fatalf("NewFromSnapshot(valid) = %v", err)
	}
	withoutClock, err := snapshot.New(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewFromSnapshot(withoutClock); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("NewFromSnapshot(without clock) = %v", err)
	}
	wrongCodec, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDORTreeDelta, CodecID: "unexpected", Payload: []byte{0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalDelta(wrongCodec); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("delta wrong codec = %v", err)
	}
}

func TestORTreeWireRejectsNonCanonicalAndIncompleteStates(t *testing.T) {
	childID := NodeID{ReplicaID: "source", WallTime: 2}
	parentID := NodeID{ReplicaID: "source", WallTime: 1}
	payload := frame.AppendUvarint(nil, 1)
	payload = frame.AppendTag(payload, childID)
	payload = frame.AppendUvarint(payload, 1)
	payload = frame.AppendTag(payload, parentID)
	payload = frame.AppendUvarint(payload, 1)
	payload = append(payload, 'x')
	payload = frame.AppendUvarint(payload, 0)

	deltaFrame, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDORTreeDelta, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalDelta(deltaFrame); err != nil {
		t.Fatalf("partial delta = %v", err)
	}
	stateFrame, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDORTreeState, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.UnmarshalBinary(stateFrame); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("incomplete state = %v", err)
	}

	duplicate := frame.AppendUvarint(nil, 2)
	for index := 0; index < 2; index++ {
		duplicate = frame.AppendTag(duplicate, childID)
		duplicate = frame.AppendUvarint(duplicate, 0)
		duplicate = frame.AppendUvarint(duplicate, 0)
	}
	duplicate = frame.AppendUvarint(duplicate, 0)
	duplicateFrame, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDORTreeDelta, Payload: duplicate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalDelta(duplicateFrame); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("duplicate tags = %v", err)
	}

	badParentFlag := frame.AppendUvarint(nil, 1)
	badParentFlag = frame.AppendTag(badParentFlag, childID)
	badParentFlag = frame.AppendUvarint(badParentFlag, 2)
	badParentFlagFrame, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDORTreeDelta, Payload: badParentFlag})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalDelta(badParentFlagFrame); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("invalid parent flag = %v", err)
	}
}

func TestORTreeInternalHelpersCoverOrderingAndBounds(t *testing.T) {
	ids := []NodeID{{ReplicaID: "a", WallTime: 1}, {ReplicaID: "b", WallTime: 2}}
	reverse(ids)
	if ids[0].ReplicaID != "b" || ids[1].ReplicaID != "a" {
		t.Fatalf("reverse = %#v", ids)
	}
	if got := sortedTreeTombstoneIDs(map[NodeID]struct{}{ids[0]: {}, ids[1]: {}}); len(got) != 2 || got[0].Compare(got[1]) >= 0 {
		t.Fatalf("sorted tombstones = %#v", got)
	}
	limits := frame.DefaultLimits()
	payloadSize := 0
	if err := addTreeIDSize(&payloadSize, NodeID{ReplicaID: "toolong", WallTime: 1}, frame.DecoderLimits{MaxPayload: 64, MaxStringBytes: 1}); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("string limit = %v", err)
	}
	limits.MaxPayload = 1
	if err := addTreeIDSize(&payloadSize, NodeID{ReplicaID: "a", WallTime: 1}, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("payload limit = %v", err)
	}
	first := Delta{nodes: map[NodeID]storedNode{ids[0]: {value: []byte("a")}}, tombstones: map[NodeID]struct{}{}}
	second := Delta{nodes: map[NodeID]storedNode{ids[1]: {value: []byte("b")}}, tombstones: map[NodeID]struct{}{ids[0]: {}}}
	merged, err := first.Merge(second)
	if err != nil || len(merged.nodes) != 2 || len(merged.tombstones) != 1 {
		t.Fatalf("Delta.Merge = %#v, %v", merged, err)
	}
	if err := validate(Delta{nodes: map[NodeID]storedNode{ids[0]: {parent: ids[0]}}, tombstones: map[NodeID]struct{}{}}); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("self-parent validation = %v", err)
	}
	cycleNodes := map[NodeID]storedNode{ids[0]: {parent: ids[1]}, ids[1]: {parent: ids[0]}}
	if liveReachable(ids[0], cycleNodes, nil) {
		t.Fatal("cyclic parent chain is reachable")
	}
	if _, ok := greatest(Delta{}); ok {
		t.Fatal("empty delta has a greatest tag")
	}
	value, err := New("state")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := value.Add(NodeID{}, nil); err != nil {
		t.Fatal(err)
	}
	if firstState, secondState := value.State(), value.State(); firstState != secondState || firstState.ElementCount != 1 {
		t.Fatalf("cached State = %#v / %#v", firstState, secondState)
	}
	var nilTree *ORTree
	if _, err := nilTree.SnapshotCurrentState(); !errors.Is(err, ErrNilTree) {
		t.Fatalf("nil SnapshotCurrentState = %v", err)
	}
}

func FuzzORTreeUnmarshal(f *testing.F) {
	value, err := New("seed")
	if err != nil {
		f.Fatal(err)
	}
	if _, _, err := value.Add(NodeID{}, []byte("seed")); err != nil {
		f.Fatal(err)
	}
	state, err := value.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(state)
	f.Fuzz(func(t *testing.T, data []byte) {
		target, err := New("target")
		if err != nil {
			t.Fatal(err)
		}
		if err := target.UnmarshalBinary(data); err == nil && target.State().ElementCount < 0 {
			t.Fatal("negative visible count")
		}
		if delta, err := UnmarshalDelta(data); err == nil {
			if err := target.ApplyDelta(delta); err != nil {
				t.Fatalf("decoded delta rejected: %v", err)
			}
		}
	})
}
