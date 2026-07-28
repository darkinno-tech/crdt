package text

import (
	"testing"

	"github.com/DarkInno/crdt/clock"
)

func TestRGAConcurrentInsertsConverge(t *testing.T) {
	left, err := New("left")
	if err != nil {
		t.Fatal(err)
	}
	right, err := New("right")
	if err != nil {
		t.Fatal(err)
	}
	base, err := left.Insert(0, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := right.ApplyDelta(base); err != nil {
		t.Fatal(err)
	}
	leftDelta, err := left.Insert(1, "b")
	if err != nil {
		t.Fatal(err)
	}
	rightDelta, err := right.Insert(1, "c")
	if err != nil {
		t.Fatal(err)
	}
	if err := left.ApplyDelta(rightDelta); err != nil {
		t.Fatal(err)
	}
	if err := right.ApplyDelta(leftDelta); err != nil {
		t.Fatal(err)
	}
	if got, want := left.String(), right.String(); got != want {
		t.Fatalf("left = %q, right = %q", got, want)
	}
	if got := left.String(); len([]rune(got)) != 3 {
		t.Fatalf("text = %q", got)
	}
}

func TestRGADeleteBeforeInsertAndInputBounds(t *testing.T) {
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	insert, err := source.Insert(0, "ab")
	if err != nil {
		t.Fatal(err)
	}
	deleteDelta, err := source.Delete(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(deleteDelta); err != nil {
		t.Fatal(err)
	}
	if got := target.String(); got != "" {
		t.Fatalf("text before insertion = %q, want empty", got)
	}
	if err := target.ApplyDelta(insert); err != nil {
		t.Fatal(err)
	}
	if got := target.String(); got != "b" {
		t.Fatalf("text = %q, want b", got)
	}
	if _, err := target.Insert(2, "x"); err != ErrRange {
		t.Fatalf("Insert out of range = %v", err)
	}
	if _, err := target.Insert(0, string([]byte{0xff})); err != ErrInvalidText {
		t.Fatalf("Insert invalid utf8 = %v", err)
	}
}

func TestRGAStateCountsOnlyRootReachableNodes(t *testing.T) {
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := source.Insert(0, "a")
	if err != nil {
		t.Fatal(err)
	}
	child, err := source.Insert(1, "b")
	if err != nil {
		t.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(child); err != nil {
		t.Fatal(err)
	}
	if got := target.String(); got != "" {
		t.Fatalf("out-of-order text = %q, want empty", got)
	}
	if got := target.State().ElementCount; got != 0 {
		t.Fatalf("out-of-order visible count = %d, want 0", got)
	}
	if err := target.ApplyDelta(parent); err != nil {
		t.Fatal(err)
	}
	if got := target.State().ElementCount; got != 2 {
		t.Fatalf("resolved visible count = %d, want 2", got)
	}
}

func TestRGAQueuesMissingParentsAndReplaysWhenTheyArrive(t *testing.T) {
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := source.Insert(0, "a")
	if err != nil {
		t.Fatal(err)
	}
	child, err := source.Insert(1, "b")
	if err != nil {
		t.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(child); err != nil {
		t.Fatal(err)
	}
	if got := target.PendingCount(); got != 1 {
		t.Fatalf("PendingCount before parent = %d, want 1", got)
	}
	missing := target.MissingParents()
	if len(missing) != 1 || missing[0] != parentNodeID(parent) {
		t.Fatalf("MissingParents = %#v", missing)
	}
	if got := target.String(); got != "" {
		t.Fatalf("text before parent = %q", got)
	}
	if err := target.ApplyDelta(parent); err != nil {
		t.Fatal(err)
	}
	if got := target.PendingCount(); got != 0 {
		t.Fatalf("PendingCount after parent = %d, want 0", got)
	}
	if got := target.String(); got != "ab" {
		t.Fatalf("text after replay = %q, want ab", got)
	}
}

func TestRGAIntegratesNewChildAgainstExistingParent(t *testing.T) {
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	base, err := source.Insert(0, "a")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := New("remote")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyDelta(base); err != nil {
		t.Fatal(err)
	}
	child, err := remote.Insert(1, "b")
	if err != nil {
		t.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(base); err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(child); err != nil {
		t.Fatal(err)
	}
	if target.PendingCount() != 0 || target.String() != "ab" {
		t.Fatalf("existing-parent insert pending=%d text=%q", target.PendingCount(), target.String())
	}
}

func TestRGAPendingLimitsRejectAtomically(t *testing.T) {
	options := DefaultOptions()
	options.MaxPendingNodes = 1
	options.MaxPendingBytes = 1 << 20
	target, err := NewWithOptions("target", options)
	if err != nil {
		t.Fatal(err)
	}
	missing := Position{ReplicaID: "missing", WallTime: 1}
	first := Position{ReplicaID: "remote", WallTime: 2}
	second := Position{ReplicaID: "remote", WallTime: 3}
	delta := Delta{nodes: map[Position]node{
		first:  {parent: missing, rune: 'a'},
		second: {parent: missing, rune: 'b'},
	}, tombstones: map[Position]struct{}{}}
	if err := target.ApplyDelta(delta); err != ErrResourceLimit {
		t.Fatalf("ApplyDelta limit = %v, want %v", err, ErrResourceLimit)
	}
	if target.PendingCount() != 0 || target.String() != "" || len(target.MissingParents()) != 0 {
		t.Fatalf("limit failure mutated target: pending=%d text=%q missing=%#v", target.PendingCount(), target.String(), target.MissingParents())
	}

	// A single delta may contain a complete parent chain without consuming the
	// pending budget after deterministic integration.
	options.MaxPendingBytes = 128
	resolved, err := NewWithOptions("resolved", options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolved.Insert(0, "abc"); err != nil {
		t.Fatalf("complete local chain rejected: %v", err)
	}
	if got := resolved.String(); got != "abc" {
		t.Fatalf("resolved text = %q", got)
	}
}

func TestRGACompactsOnlyStableLeafTombstones(t *testing.T) {
	value, err := New("local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Insert(0, "ab"); err != nil {
		t.Fatal(err)
	}
	firstDelete, err := value.Delete(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	first := tombstoneNodeID(firstDelete)
	if _, err := value.CompactTombstones([]Position{first}); err != ErrUnsafeCompaction {
		t.Fatalf("compact structural parent = %v, want %v", err, ErrUnsafeCompaction)
	}
	secondDelete, err := value.Delete(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	second := tombstoneNodeID(secondDelete)
	if removed, err := value.CompactTombstones([]Position{second}); err != nil || removed != 1 {
		t.Fatalf("compact leaf = %d, %v", removed, err)
	}
	if removed, err := value.CompactTombstones([]Position{first}); err != nil || removed != 1 {
		t.Fatalf("compact former parent = %d, %v", removed, err)
	}
	if got := value.String(); got != "" || len(value.TombstoneTags()) != 0 || value.State().TombstoneCount != 0 {
		t.Fatalf("compacted state text=%q tombstones=%#v snapshot=%+v", got, value.TombstoneTags(), value.State())
	}
	if _, err := value.MarshalBinary(); err != nil {
		t.Fatalf("compacted state marshal: %v", err)
	}
}

func parentNodeID(delta Delta) Position {
	for id := range delta.nodes {
		return id
	}
	return Position{}
}

func tombstoneNodeID(delta Delta) Position {
	for id := range delta.tombstones {
		return id
	}
	return Position{}
}

func TestRGARejectsCycle(t *testing.T) {
	value, err := New("local")
	if err != nil {
		t.Fatal(err)
	}
	first := Position{ReplicaID: "other", WallTime: 1}
	second := Position{ReplicaID: "other", WallTime: 2}
	delta := Delta{nodes: map[Position]node{first: {parent: second, rune: 'a'}, second: {parent: first, rune: 'b'}}, tombstones: map[Position]struct{}{}}
	if err := value.ApplyDelta(delta); err != ErrInvalidDelta {
		t.Fatalf("ApplyDelta(cycle) = %v", err)
	}
}

func TestRGAProjectionInvalidatesAcrossEdits(t *testing.T) {
	value, err := New("local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Insert(0, "abc"); err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "abc" {
		t.Fatalf("first projection = %q", got)
	}
	if _, err := value.Delete(1, 1); err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "ac" {
		t.Fatalf("projection after delete = %q", got)
	}
	if _, err := value.Insert(1, "X"); err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "aXc" {
		t.Fatalf("projection after insert = %q", got)
	}
	if state := value.State(); state.ElementCount != 3 || state.TombstoneCount != 1 {
		t.Fatalf("State() = %+v", state)
	}
}

func TestRGAErrorPathsAndMetadata(t *testing.T) {
	if _, err := NewFromClock(clock.State{}); err != ErrInvalidReplicaID {
		t.Fatalf("NewFromClock() = %v", err)
	}
	var nilValue *RGA
	if _, err := nilValue.Insert(0, "x"); err != ErrNilText {
		t.Fatalf("nil Insert() = %v", err)
	}
	if _, err := nilValue.Delete(0, 0); err != ErrNilText {
		t.Fatalf("nil Delete() = %v", err)
	}
	if err := nilValue.ApplyDelta(Delta{}); err != ErrNilText {
		t.Fatalf("nil ApplyDelta() = %v", err)
	}
	if err := nilValue.Merge(nil); err != ErrNilText {
		t.Fatalf("nil Merge() = %v", err)
	}
	if nilValue.String() != "" || nilValue.Positions() != nil || nilValue.ClockState() != (clock.State{}) || nilValue.State().Type != "rga-text" {
		t.Fatal("nil accessors")
	}

	value, err := New("local")
	if err != nil {
		t.Fatal(err)
	}
	if value.ClockState().ReplicaID != "local" || value.State().ElementCount != 0 || len(value.Positions()) != 0 {
		t.Fatal("empty metadata")
	}
	if _, err := value.Insert(-1, "x"); err != ErrRange {
		t.Fatalf("negative Insert() = %v", err)
	}
	if _, err := value.Insert(0, ""); err != nil {
		t.Fatalf("empty Insert() = %v", err)
	}
	if _, err := value.Delete(-1, 0); err != ErrRange {
		t.Fatalf("negative Delete() = %v", err)
	}
	if _, err := value.Delete(1, 0); err != ErrRange {
		t.Fatalf("out of range Delete() = %v", err)
	}
	if _, err := value.Delete(0, 1); err != ErrRange {
		t.Fatalf("oversize Delete() = %v", err)
	}
	if err := value.Merge(value); err != nil {
		t.Fatal(err)
	}
	if err := value.Merge(nil); err != ErrNilText {
		t.Fatalf("Merge(nil) = %v", err)
	}
}

func TestRGADeltaMergeAndValidation(t *testing.T) {
	left, err := New("left")
	if err != nil {
		t.Fatal(err)
	}
	right, err := New("right")
	if err != nil {
		t.Fatal(err)
	}
	first, err := left.Insert(0, "a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := right.Insert(0, "b")
	if err != nil {
		t.Fatal(err)
	}
	merged, err := first.Merge(second)
	if err != nil {
		t.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(merged); err != nil {
		t.Fatal(err)
	}
	if err := target.Merge(left); err != nil {
		t.Fatal(err)
	}
	if got := target.String(); len([]rune(got)) != 2 {
		t.Fatalf("merged text = %q", got)
	}

	badID := Position{}
	if err := target.ApplyDelta(Delta{nodes: map[Position]node{badID: {rune: 'x'}}, tombstones: map[Position]struct{}{}}); err != ErrInvalidDelta {
		t.Fatalf("invalid ID = %v", err)
	}
	validID := Position{ReplicaID: "other", WallTime: 1}
	if err := target.ApplyDelta(Delta{nodes: map[Position]node{validID: {rune: rune(0xD800)}}, tombstones: map[Position]struct{}{}}); err != ErrInvalidDelta {
		t.Fatalf("invalid rune = %v", err)
	}
	if err := target.ApplyDelta(Delta{nodes: map[Position]node{}, tombstones: map[Position]struct{}{badID: {}}}); err != ErrInvalidDelta {
		t.Fatalf("invalid tombstone = %v", err)
	}

	conflict := Delta{nodes: map[Position]node{validID: {rune: 'x'}}, tombstones: map[Position]struct{}{}}
	if err := target.ApplyDelta(conflict); err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(Delta{nodes: map[Position]node{validID: {rune: 'y'}}, tombstones: map[Position]struct{}{}}); err != ErrTagConflict {
		t.Fatalf("tag conflict = %v", err)
	}
	if _, err := conflict.Merge(Delta{nodes: map[Position]node{validID: {rune: 'y'}}, tombstones: map[Position]struct{}{}}); err != ErrTagConflict {
		t.Fatalf("delta conflict = %v", err)
	}
}
