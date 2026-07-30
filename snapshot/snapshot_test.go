package snapshot

import (
	"errors"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/counter"
	frame "github.com/DarkInno/crdt/encoding"
)

func TestSnapshotCopiesStateAndFrontier(t *testing.T) {
	counter, err := counter.NewGCounter("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := counter.Increment(1); err != nil {
		t.Fatal(err)
	}
	state, err := counter.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	frontier := map[string]crdt.Tag{"a": {ReplicaID: "a", WallTime: 2}}
	snap, err := New(state, frontier)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	state[len(state)-1] ^= 1
	frontier["a"] = crdt.Tag{ReplicaID: "a", WallTime: 3}
	gotBytes := snap.Bytes()
	gotBytes[len(gotBytes)-1] ^= 1
	gotFrontier := snap.Frontier()
	gotFrontier["a"] = crdt.Tag{ReplicaID: "a", WallTime: 4}
	if snap.Frontier()["a"].WallTime != 2 {
		t.Fatal("snapshot aliases caller frontier")
	}
	if _, err := New(snap.Bytes(), snap.Frontier()); err != nil {
		t.Fatalf("snapshot bytes were mutated: %v", err)
	}
}

func TestRecoveryPlanRejectsMismatchedDelta(t *testing.T) {
	source, err := counter.NewGCounter("source")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := source.Increment(1)
	if err != nil {
		t.Fatal(err)
	}
	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := New(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	encodedDelta, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewRecoveryPlan(snapshot, [][]byte{encodedDelta}, len(encodedDelta))
	if err != nil {
		t.Fatal(err)
	}
	returned := plan.Deltas()
	returned[0][0] ^= 1
	if _, err := NewRecoveryPlan(plan.Snapshot, plan.Deltas(), len(encodedDelta)); err != nil {
		t.Fatalf("plan aliases output: %v", err)
	}
	if _, err := NewRecoveryPlan(snapshot, [][]byte{state}, len(state)); err == nil {
		t.Fatal("state frame accepted as recovery delta")
	}
}

func TestSnapshotAndRecoveryPlanBindOuterFrameVersion(t *testing.T) {
	state, err := frame.MarshalFrameV2(frame.Frame{TypeID: crdt.TypeIDGCounterState, Payload: []byte{0}})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := New(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if saved.FormatVersion != frame.FormatVersionV2 {
		t.Fatalf("snapshot format = %d, want %d", saved.FormatVersion, frame.FormatVersionV2)
	}
	delta, err := frame.MarshalFrameV2(frame.Frame{TypeID: crdt.TypeIDGCounterDelta, Payload: []byte{0}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewRecoveryPlan(saved, [][]byte{delta}, len(delta)); err != nil {
		t.Fatalf("v2 recovery plan: %v", err)
	}
	legacy, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDGCounterDelta, Payload: []byte{0}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewRecoveryPlan(saved, [][]byte{legacy}, len(legacy)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mixed frame recovery plan error = %v, want ErrInvalid", err)
	}
}

func TestNewValidatedRejectsInvalidConcreteState(t *testing.T) {
	t.Parallel()
	malformed, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDGCounterState, Payload: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(malformed, nil); err != nil {
		t.Fatalf("New() unexpectedly rejected a valid envelope: %v", err)
	}
	if _, err := NewValidated(malformed, nil, validateGCounterState); err == nil {
		t.Fatal("NewValidated() accepted malformed G-Counter state")
	}

	counter, err := counter.NewGCounter("validator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := counter.Increment(1); err != nil {
		t.Fatal(err)
	}
	state, err := counter.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewValidated(state, nil, validateGCounterState)
	if err != nil {
		t.Fatalf("NewValidated() error = %v", err)
	}
	if _, err := NewRecoveryPlan(snapshot, nil, 1); err != nil {
		t.Fatalf("NewRecoveryPlan() error = %v", err)
	}
}

func validateGCounterState(data []byte) error {
	validator, err := counter.NewGCounter("snapshot-validator")
	if err != nil {
		return err
	}
	return validator.UnmarshalBinary(data)
}
