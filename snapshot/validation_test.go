package snapshot_test

import (
	"errors"
	"testing"

	"github.com/darkinno/crdt"
	"github.com/darkinno/crdt/clock"
	"github.com/darkinno/crdt/counter"
	frame "github.com/darkinno/crdt/encoding"
	"github.com/darkinno/crdt/set"
	"github.com/darkinno/crdt/snapshot"
)

func TestValidatedSnapshotRejectsInvalidConcreteState(t *testing.T) {
	counterState, err := encodedCounterState()
	if err != nil {
		t.Fatal(err)
	}
	frontier := map[string]crdt.Tag{"counter": {ReplicaID: "counter", WallTime: 1}}
	validator := func(data []byte) error {
		value, err := counter.NewGCounter("validator")
		if err != nil {
			return err
		}
		return value.UnmarshalBinary(data)
	}
	validated, err := snapshot.NewValidated(counterState, frontier, validator)
	if err != nil {
		t.Fatalf("NewValidated() error = %v", err)
	}
	if _, err := snapshot.NewRecoveryPlan(validated, nil, 1); err != nil {
		t.Fatalf("NewRecoveryPlan(validated) error = %v", err)
	}
	if _, err := snapshot.NewValidated(counterState, frontier, nil); !errors.Is(err, snapshot.ErrInvalid) {
		t.Fatalf("nil validator error = %v, want %v", err, snapshot.ErrInvalid)
	}
	if _, err := snapshot.NewValidated(counterState, frontier, func([]byte) error { return errors.New("invalid state") }); !errors.Is(err, snapshot.ErrInvalid) {
		t.Fatalf("rejecting validator error = %v, want %v", err, snapshot.ErrInvalid)
	}
	plain, err := snapshot.New(counterState, frontier)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.ClockState(); ok {
		t.Fatal("plain snapshot unexpectedly carries clock state")
	}
}

func TestSnapshotSupportsPNCounterRecoveryPlan(t *testing.T) {
	value, err := counter.NewPNCounter("counter")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := value.Increment(3)
	if err != nil {
		t.Fatal(err)
	}
	state, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	encodedDelta, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := snapshot.New(state, nil)
	if err != nil {
		t.Fatalf("snapshot.New() error = %v", err)
	}
	if _, err := snapshot.NewRecoveryPlan(saved, [][]byte{encodedDelta}, len(encodedDelta)); err != nil {
		t.Fatalf("NewRecoveryPlan() error = %v", err)
	}
}

func TestSnapshotRejectsReservedStateTypesWithoutConcreteWireSupport(t *testing.T) {
	state, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDLWWMapState})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.New(state, nil); !errors.Is(err, snapshot.ErrInvalid) {
		t.Fatalf("snapshot.New() error = %v, want %v", err, snapshot.ErrInvalid)
	}
}

func TestValidatedSnapshotFreezesValidationAndRejectsPanics(t *testing.T) {
	counterState, err := encodedCounterState()
	if err != nil {
		t.Fatal(err)
	}
	frontier := map[string]crdt.Tag{"counter": {ReplicaID: "counter", WallTime: 1}}
	calls := 0
	validated, err := snapshot.NewValidated(counterState, frontier, func([]byte) error {
		calls++
		if calls > 1 {
			return errors.New("validator called after construction")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.NewRecoveryPlan(validated, nil, 1); err != nil {
		t.Fatalf("NewRecoveryPlan() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("validator calls = %d, want 1", calls)
	}
	if _, err := snapshot.NewValidated(counterState, frontier, func([]byte) error { panic("invalid validator") }); !errors.Is(err, snapshot.ErrInvalid) {
		t.Fatalf("panicking validator error = %v, want %v", err, snapshot.ErrInvalid)
	}
}

func TestValidatedORSetSnapshotRequiresClockAndConcreteValidation(t *testing.T) {
	codec := snapshotStringCodec{}
	value, err := set.NewORSet("replica", codec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Add("item"); err != nil {
		t.Fatal(err)
	}
	state, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	frontier := value.Frontier()
	clockState := value.ClockState()
	validator := func(data []byte) error {
		decoded, err := set.NewORSet("validator", codec)
		if err != nil {
			return err
		}
		return decoded.UnmarshalBinary(data)
	}
	validated, err := snapshot.NewValidatedWithClockState(state, frontier, clockState, validator)
	if err != nil {
		t.Fatalf("NewValidatedWithClockState() error = %v", err)
	}
	if got, ok := validated.ClockState(); !ok || got != clockState {
		t.Fatalf("ClockState() = %#v, %v; want %#v, true", got, ok, clockState)
	}
	if _, err := snapshot.NewValidatedWithClockState(state, frontier, clock.State{}, validator); !errors.Is(err, snapshot.ErrInvalid) {
		t.Fatalf("invalid clock state error = %v, want %v", err, snapshot.ErrInvalid)
	}
	counterState, err := encodedCounterState()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.NewWithClockState(counterState, nil, clockState); !errors.Is(err, snapshot.ErrInvalid) {
		t.Fatalf("counter clock snapshot error = %v, want %v", err, snapshot.ErrInvalid)
	}
}

func encodedCounterState() ([]byte, error) {
	value, err := counter.NewGCounter("counter")
	if err != nil {
		return nil, err
	}
	if _, err := value.Increment(1); err != nil {
		return nil, err
	}
	return value.MarshalBinary()
}
