package clock

import (
	"errors"
	"testing"
	"time"

	"github.com/darkinno/crdt"
)

func TestHLCNilAndPhysicalClockFailures(t *testing.T) {
	t.Parallel()
	var nilClock *HLC
	if got := nilClock.ReplicaID(); got != "" {
		t.Fatalf("nil ReplicaID() = %q", got)
	}
	if got := nilClock.Snapshot(); got != (State{}) {
		t.Fatalf("nil Snapshot() = %#v", got)
	}
	if _, err := nilClock.Now(); !errors.Is(err, ErrClockExhausted) {
		t.Fatalf("nil Now() error = %v", err)
	}
	if err := nilClock.Witness(crdt.Tag{ReplicaID: "remote"}); !errors.Is(err, ErrClockExhausted) {
		t.Fatalf("nil Witness() error = %v", err)
	}

	clock, err := NewHLC("local")
	if err != nil {
		t.Fatal(err)
	}
	clock.now = func() time.Time { return time.UnixMilli(-1) }
	if _, err := clock.Now(); !errors.Is(err, ErrClockExhausted) {
		t.Fatalf("negative physical time error = %v", err)
	}
}

func TestHLCWitnessCoversLocalAndPhysicalDominance(t *testing.T) {
	t.Parallel()
	local, err := NewHLCFromState(State{ReplicaID: "local", WallTime: 100, Logical: 4})
	if err != nil {
		t.Fatal(err)
	}
	local.now = func() time.Time { return time.UnixMilli(90) }
	if err := local.Witness(crdt.Tag{ReplicaID: "remote", WallTime: 80, Logical: 9}); err != nil {
		t.Fatal(err)
	}
	if got, want := local.Snapshot(), (State{ReplicaID: "local", WallTime: 100, Logical: 5}); got != want {
		t.Fatalf("local-dominant Witness() = %#v, want %#v", got, want)
	}

	physical, err := NewHLC("local")
	if err != nil {
		t.Fatal(err)
	}
	physical.now = func() time.Time { return time.UnixMilli(200) }
	if err := physical.Witness(crdt.Tag{ReplicaID: "remote", WallTime: 150}); err != nil {
		t.Fatal(err)
	}
	if got, want := physical.Snapshot(), (State{ReplicaID: "local", WallTime: 200}); got != want {
		t.Fatalf("physical-dominant Witness() = %#v, want %#v", got, want)
	}
}
