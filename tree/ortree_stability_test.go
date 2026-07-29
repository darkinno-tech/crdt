package tree

import (
	"errors"
	"testing"
)

func TestORTreeDuplicateDeliveryAndRejectedAddLeaveClockUnchanged(t *testing.T) {
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	root, delta, err := source.Add(NodeID{}, []byte("root"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(delta); err != nil {
		t.Fatal(err)
	}
	beforeDuplicate := target.ClockState()
	if err := target.ApplyDelta(delta); err != nil {
		t.Fatal(err)
	}
	if afterDuplicate := target.ClockState(); afterDuplicate != beforeDuplicate {
		t.Fatalf("duplicate delivery advanced clock: before=%#v after=%#v", beforeDuplicate, afterDuplicate)
	}

	limited, err := NewWithOptions("limited", Options{MaxNodes: 1, MaxTombstones: 1, MaxValueBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := limited.Add(NodeID{}, []byte("root")); err != nil {
		t.Fatal(err)
	}
	beforeLimit := limited.ClockState()
	if _, _, err := limited.Add(root, []byte("child")); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("over-limit add = %v", err)
	}
	if afterLimit := limited.ClockState(); afterLimit != beforeLimit {
		t.Fatalf("rejected capacity add advanced clock: before=%#v after=%#v", beforeLimit, afterLimit)
	}
	parentChecked, err := NewWithOptions("parent-checked", Options{MaxNodes: 2, MaxTombstones: 2, MaxValueBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	beforeParent := parentChecked.ClockState()
	if _, _, err := parentChecked.Add(NodeID{ReplicaID: "missing", WallTime: 1}, []byte("child")); !errors.Is(err, ErrUnknownParent) {
		t.Fatalf("unknown-parent add = %v", err)
	}
	if afterParent := parentChecked.ClockState(); afterParent != beforeParent {
		t.Fatalf("rejected parent add advanced clock: before=%#v after=%#v", beforeParent, afterParent)
	}
}

// TestORTreeAppliesLongLinearDelta exercises the parent-chain shape that used
// to repeatedly retraverse the same ancestors during cycle validation.
func TestORTreeAppliesLongLinearDelta(t *testing.T) {
	const count = 16 << 10
	delta := linearTreeDelta(count)
	value, err := NewWithOptions("target", Options{MaxNodes: count, MaxTombstones: count, MaxValueBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := value.ApplyDelta(delta); err != nil {
		t.Fatal(err)
	}
	if state := value.State(); state.ElementCount != count || state.TombstoneCount != 0 {
		t.Fatalf("long-chain state = %#v", state)
	}

	limited, err := NewWithOptions("limited", Options{MaxNodes: 1, MaxTombstones: 1, MaxValueBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	before := limited.ClockState()
	if err := limited.ApplyDelta(delta); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("over-limit long chain = %v", err)
	}
	if state := limited.State(); state.ElementCount != 0 || state.TombstoneCount != 0 || limited.ClockState() != before {
		t.Fatalf("rejected long chain changed receiver: state=%#v clock=%#v", state, limited.ClockState())
	}
}

func linearTreeDelta(count int) Delta {
	nodes := make(map[NodeID]storedNode, count)
	parent := NodeID{}
	for index := 1; index <= count; index++ {
		id := NodeID{ReplicaID: "linear", WallTime: uint64(index)}
		nodes[id] = storedNode{parent: parent, value: []byte{'x'}}
		parent = id
	}
	return Delta{nodes: nodes, tombstones: make(map[NodeID]struct{})}
}
