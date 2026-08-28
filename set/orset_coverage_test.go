package set

import (
	"errors"
	"testing"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/clock"
)

func TestORSetRejectsInvalidConstructionAndNilReceivers(t *testing.T) {
	var nilSet *ORSet[string]
	if _, err := nilSet.Add("x"); !errors.Is(err, ErrNilORSet) {
		t.Fatalf("nil Add() error = %v", err)
	}
	if _, err := nilSet.Remove("x"); !errors.Is(err, ErrNilORSet) {
		t.Fatalf("nil Remove() error = %v", err)
	}
	if _, err := nilSet.Compact(nil); !errors.Is(err, ErrNilORSet) {
		t.Fatalf("nil Compact() error = %v", err)
	}
	if err := nilSet.ApplyDelta(ORSetDelta[string]{}); !errors.Is(err, ErrNilORSet) {
		t.Fatalf("nil ApplyDelta() error = %v", err)
	}
	if err := nilSet.Merge(nil); !errors.Is(err, ErrNilORSet) {
		t.Fatalf("nil Merge() error = %v", err)
	}
	if err := nilSet.UnmarshalBinary(nil); !errors.Is(err, ErrNilORSet) {
		t.Fatalf("nil UnmarshalBinary() error = %v", err)
	}
	if _, err := nilSet.MarshalBinary(); !errors.Is(err, ErrNilORSet) {
		t.Fatalf("nil MarshalBinary() error = %v", err)
	}
	if _, _, err := nilSet.MarshalBinaryWithClockState(); !errors.Is(err, ErrNilORSet) {
		t.Fatalf("nil MarshalBinaryWithClockState() error = %v", err)
	}
	if _, err := nilSet.Snapshot(nil); !errors.Is(err, ErrNilORSet) {
		t.Fatalf("nil Snapshot() error = %v", err)
	}
	if _, err := nilSet.SnapshotCurrentState(); !errors.Is(err, ErrNilORSet) {
		t.Fatalf("nil SnapshotCurrentState() error = %v", err)
	}
	if state := nilSet.State(); state.Type != "orset" {
		t.Fatalf("nil State() = %#v", state)
	}
	if nilSet.Frontier() != nil || nilSet.ClockState() != (clock.State{}) || nilSet.Contains("x") || nilSet.Elements() != nil {
		t.Fatal("nil OR-Set accessors returned non-empty state")
	}
	if _, err := NewORSet("replica", stringCodec{}); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("empty codec ID error = %v", err)
	}
	if _, err := NewORSet[string]("replica", nil); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("nil codec error = %v", err)
	}
	if _, err := NewORSetFromClock(clock.State{}, stringCodec{id: "codec"}); err == nil {
		t.Fatal("invalid clock state accepted")
	}
}

func TestORSetDeltaMergeSnapshotAndCodecFailuresAreAtomic(t *testing.T) {
	codec := stringCodec{id: "example.com/coverage/v1"}
	left := mustNewORSet(t, "left", codec)
	right := mustNewORSet(t, "right", codec)
	leftDelta, err := left.Add("left")
	if err != nil {
		t.Fatal(err)
	}
	rightDelta, err := right.Add("right")
	if err != nil {
		t.Fatal(err)
	}
	merged, err := leftDelta.Merge(rightDelta)
	if err != nil {
		t.Fatal(err)
	}
	if err := right.ApplyDelta(merged); err != nil {
		t.Fatal(err)
	}
	if !right.Contains("left") || !right.Contains("right") {
		t.Fatal("merged delta did not carry both adds")
	}
	if _, err := merged.MarshalBinary(stringCodec{}); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("MarshalBinary(empty codec) error = %v", err)
	}
	if _, err := left.Snapshot(map[string]crdt.Tag{"wrong": {ReplicaID: "other"}}); err == nil {
		t.Fatal("Snapshot() accepted invalid frontier")
	}
	saved, err := left.Snapshot(left.Frontier())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewORSetFromSnapshot(saved, stringCodec{id: "other"}); !errors.Is(err, ErrCodecMismatch) {
		t.Fatalf("snapshot codec mismatch error = %v", err)
	}

	invalid := ORSetDelta[string]{
		adds: map[string]map[crdt.Tag]struct{}{
			"x": {
				crdt.Tag{}: {},
			},
		},
	}
	if err := left.ApplyDelta(invalid); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("invalid delta error = %v", err)
	}
}
