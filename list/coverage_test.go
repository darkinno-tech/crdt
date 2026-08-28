package list

import (
	"errors"
	"testing"

	"github.com/darkinno-tech/crdt/clock"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/snapshot"
)

type blankCodec struct{}

func (blankCodec) ID() string                       { return " \t" }
func (blankCodec) Marshal(string) ([]byte, error)   { return nil, nil }
func (blankCodec) Unmarshal([]byte) (string, error) { return "", nil }

type unstableCodec struct{}

func (unstableCodec) ID() string                           { return "list-test-string/v1" }
func (unstableCodec) Marshal(value string) ([]byte, error) { return []byte(value), nil }
func (unstableCodec) Unmarshal([]byte) (string, error)     { return "", nil }

func TestRGAPublicLifecycleMergeAndOutOfOrderRecovery(t *testing.T) {
	source, err := NewFromClock(clock.State{ReplicaID: "source"}, stringCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if got := source.ClockState().ReplicaID; got != "source" {
		t.Fatalf("ClockState().ReplicaID = %q", got)
	}
	parent, err := source.Append([]string{"parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := source.Append([]string{"child"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := source.At(1); err != nil || got != "child" {
		t.Fatalf("At(1) = %q, %v", got, err)
	}
	if _, err := source.At(2); !errors.Is(err, ErrRange) {
		t.Fatalf("At out of range = %v", err)
	}
	positions := source.Positions()
	if len(positions) != 2 || positions[0] == positions[1] {
		t.Fatalf("Positions() = %#v", positions)
	}

	target := mustList(t, "target")
	if err := target.ApplyDelta(child); err != nil {
		t.Fatal(err)
	}
	if target.PendingCount() != 1 || len(target.MissingParents()) != 1 {
		t.Fatalf("pending=%d missing=%#v", target.PendingCount(), target.MissingParents())
	}
	if _, err := target.MarshalBinary(); !errors.Is(err, ErrIncompleteState) {
		t.Fatalf("MarshalBinary incomplete = %v", err)
	}
	if err := target.ApplyDelta(parent); err != nil {
		t.Fatal(err)
	}
	if got, want := mustValues(t, target), []string{"parent", "child"}; !sameStrings(got, want) {
		t.Fatalf("resolved values = %q, want %q", got, want)
	}

	state, stateClock, err := target.MarshalBinaryWithClockState()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewFromClock(stateClock, stringCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.UnmarshalBinary(state); err != nil {
		t.Fatal(err)
	}
	if got, want := mustValues(t, restored), []string{"parent", "child"}; !sameStrings(got, want) {
		t.Fatalf("restored values = %q, want %q", got, want)
	}

	other := mustList(t, "other")
	if _, err := other.Append([]string{"other"}); err != nil {
		t.Fatal(err)
	}
	if err := restored.Merge(other); err != nil {
		t.Fatal(err)
	}
	if got := mustValues(t, restored); len(got) != 3 {
		t.Fatalf("merged values = %q", got)
	}
}

func TestSequenceSiblingOrderingAndLeafRemoval(t *testing.T) {
	parent := newSequencePair(Position{ReplicaID: "parent", WallTime: 1}, false)
	children := newChildIndex()
	high := newSequencePair(Position{ReplicaID: "writer", WallTime: 3}, true)
	middle := newSequencePair(Position{ReplicaID: "writer", WallTime: 2}, true)
	low := newSequencePair(Position{ReplicaID: "writer", WallTime: 1}, true)
	if previous, hasPrevious := children.insert(parent, high); previous != nil || hasPrevious {
		t.Fatal("first child reported a predecessor")
	}
	if previous, hasPrevious := children.insert(parent, low); previous != high || !hasPrevious {
		t.Fatalf("low child predecessor = %#v, %t", previous, hasPrevious)
	}
	if previous, hasPrevious := children.insert(parent, middle); previous != high || !hasPrevious {
		t.Fatalf("middle child predecessor = %#v, %t", previous, hasPrevious)
	}
	if children.count(parent) != 3 || sortSearchDescending(children.branches[parent.position], middle.position) != 2 ||
		sortSearchDescendingOrEqual(children.branches[parent.position], middle.position) != 1 {
		t.Fatalf("unexpected sibling order: %#v", children.branches[parent.position])
	}
	if children.remove(parent, newSequencePair(Position{ReplicaID: "writer", WallTime: 4}, true)) {
		t.Fatal("removed a missing sibling")
	}
	if !children.remove(parent, high) || !children.remove(parent, low) || !children.remove(parent, middle) || children.count(parent) != 0 {
		t.Fatal("sibling removal did not restore the compact representation")
	}

	index := newSequenceIndex()
	pair := newSequencePair(Position{ReplicaID: "writer", WallTime: 9}, true)
	index.insertPairAfter(&index.pair(Position{}).entry, pair)
	if position, ok := index.visibleAt(0); !ok || position != pair.position {
		t.Fatalf("visibleAt(0) = %#v, %t", position, ok)
	}
	if _, ok := index.visibleAt(-1); ok {
		t.Fatal("negative visible offset succeeded")
	}
	if !index.removeLeaf(pair.position) || index.removeLeaf(pair.position) {
		t.Fatal("leaf removal result was not idempotent")
	}
}

func TestRGABoundariesAndSnapshotLimits(t *testing.T) {
	if _, err := NewFromClock(clock.State{}, stringCodec{}); !errors.Is(err, ErrInvalidReplica) {
		t.Fatalf("empty clock state = %v", err)
	}
	if _, err := New[string]("writer", nil); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("nil codec = %v", err)
	}
	if _, err := New("writer", blankCodec{}); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("blank codec ID = %v", err)
	}
	invalidOptions := DefaultOptions()
	invalidOptions.MaxNodes = 0
	if _, err := NewWithOptions("writer", stringCodec{}, invalidOptions); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid options = %v", err)
	}

	value := mustList(t, "writer")
	if _, err := value.Insert(0, []string{"one"}); err != nil {
		t.Fatal(err)
	}
	saved, err := value.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewFromSnapshotWithOptions(saved, stringCodec{}, DefaultOptions(), frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := mustValues(t, recovered), []string{"one"}; !sameStrings(got, want) {
		t.Fatalf("snapshot values = %q", got)
	}
	legacySnapshot, err := snapshot.New(saved.Bytes(), saved.Frontier())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewFromSnapshot(legacySnapshot, stringCodec{}); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("snapshot without clock = %v", err)
	}
	if _, err := NewFromSnapshotWithOptions(saved, stringCodec{}, Options{}, frame.DefaultLimits()); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("snapshot with invalid options = %v", err)
	}
	wrongType := saved
	wrongType.TypeID++
	if _, err := NewFromSnapshot(wrongType, stringCodec{}); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("snapshot with wrong type = %v", err)
	}

	tight := frame.DefaultLimits()
	tight.MaxPayload = 1
	if _, err := value.MarshalBinaryWithLimits(tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tight state limit = %v", err)
	}
	change, err := value.Insert(1, []string{"two"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := change.MarshalBinaryWithLimits(tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tight delta limit = %v", err)
	}
	encoded, err := change.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalDelta(encoded, nonCanonicalCodec{}); err == nil {
		t.Fatal("delta accepted an incompatible codec")
	}
	if _, err := UnmarshalDelta[string](encoded, nil); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("delta with nil codec = %v", err)
	}
	if err := recovered.Merge(nil); !errors.Is(err, ErrNilList) {
		t.Fatalf("Merge(nil) = %v", err)
	}
	if _, err := recovered.CompactTombstones([]Position{{}}); !errors.Is(err, ErrUnsafeCompaction) {
		t.Fatalf("CompactTombstones(invalid) = %v", err)
	}

	var nilList *RGA[string]
	if _, err := nilList.Append([]string{"x"}); !errors.Is(err, ErrNilList) {
		t.Fatalf("nil append = %v", err)
	}
	if _, err := nilList.Delete(0, 1); !errors.Is(err, ErrNilList) {
		t.Fatalf("nil delete = %v", err)
	}
	if _, err := nilList.At(0); !errors.Is(err, ErrNilList) {
		t.Fatalf("nil at = %v", err)
	}
	if nilList.Positions() != nil || nilList.MissingParents() != nil || nilList.PendingCount() != 0 {
		t.Fatal("nil list accessors did not fail closed")
	}
	if _, err := nilList.MarshalBinary(); !errors.Is(err, ErrNilList) {
		t.Fatalf("nil MarshalBinary = %v", err)
	}
	if _, err := nilList.SnapshotCurrentState(); !errors.Is(err, ErrNilList) {
		t.Fatalf("nil SnapshotCurrentState = %v", err)
	}
	if nilList.State().ElementCount != 0 || nilList.ClockState().ReplicaID != "" {
		t.Fatal("nil diagnostics did not fail closed")
	}
}

func TestRGARejectsInvalidMutationsAndRetainsPriorState(t *testing.T) {
	value := mustList(t, "writer")
	if _, err := value.Insert(-1, []string{"x"}); !errors.Is(err, ErrRange) {
		t.Fatalf("negative insert = %v", err)
	}
	if _, err := value.Insert(1, []string{"x"}); !errors.Is(err, ErrRange) {
		t.Fatalf("out-of-range insert = %v", err)
	}
	if empty, err := value.Insert(0, nil); err != nil || len(empty.nodes) != 0 || len(empty.tombstones) != 0 {
		t.Fatalf("empty insert = %#v, %v", empty, err)
	}
	if _, err := value.Delete(-1, 0); !errors.Is(err, ErrRange) {
		t.Fatalf("negative delete = %v", err)
	}
	if _, err := value.Delete(1, 0); !errors.Is(err, ErrRange) {
		t.Fatalf("out-of-range delete = %v", err)
	}
	if empty, err := value.Delete(0, 0); err != nil || len(empty.tombstones) != 0 {
		t.Fatalf("empty delete = %#v, %v", empty, err)
	}
	nonCanonical := mustList(t, "noncanonical")
	nonCanonical.codec = unstableCodec{}
	if _, err := nonCanonical.Insert(0, []string{"UPPER"}); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("non-canonical insert = %v", err)
	}

	inserted := mustInsert(t, value, 0, []string{"safe"})
	var id Position
	for position := range inserted.nodes {
		id = position
	}
	before, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := value.ApplyDelta(Delta{}); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("empty delta = %v", err)
	}
	conflicting := Delta{
		codecID: value.codecID,
		nodes: map[Position]node{
			id: {value: []byte("other")},
		},
		tombstones: map[Position]struct{}{},
	}
	if err := value.ApplyDelta(conflicting); !errors.Is(err, ErrTagConflict) {
		t.Fatalf("conflicting delta = %v", err)
	}
	first := Position{ReplicaID: "remote", WallTime: 1}
	second := Position{ReplicaID: "remote", WallTime: 2}
	cycle := Delta{
		codecID: value.codecID,
		nodes: map[Position]node{
			first:  {parent: second, value: []byte("a")},
			second: {parent: first, value: []byte("b")},
		},
		tombstones: map[Position]struct{}{},
	}
	if err := value.ApplyDelta(cycle); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("cyclic delta = %v", err)
	}
	after, err := value.MarshalBinary()
	if err != nil || string(before) != string(after) {
		t.Fatalf("rejected mutation changed state: %v", err)
	}

	tight := DefaultOptions()
	tight.MaxNodes = 1
	limited, err := NewWithOptions("limited", stringCodec{}, tight)
	if err != nil {
		t.Fatal(err)
	}
	source := mustList(t, "source")
	change := mustInsert(t, source, 0, []string{"one", "two"})
	if err := limited.ApplyDelta(change); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("node limit = %v", err)
	}
	if err := value.Merge(value); err != nil {
		t.Fatalf("self merge = %v", err)
	}
	other, err := New("other", nonCanonicalCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := value.Merge(other); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("incompatible merge = %v", err)
	}
	if removed, err := value.CompactTombstones([]Position{id}); err != nil || removed != 0 {
		t.Fatalf("compact non-tombstone = %d, %v", removed, err)
	}
}
