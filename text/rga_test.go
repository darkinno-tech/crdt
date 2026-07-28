package text

import (
	"testing"

	"github.com/darkinno/crdt/clock"
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
