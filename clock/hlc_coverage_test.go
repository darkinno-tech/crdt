package clock

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/darkinno/crdt"
)

func TestHLCStateAccessAndExhaustionAreAtomic(t *testing.T) {
	if _, err := NewHLCFromState(State{}); !errors.Is(err, ErrInvalidReplicaID) {
		t.Fatalf("NewHLCFromState() error = %v", err)
	}
	h, err := NewHLCFromState(State{ReplicaID: "local", WallTime: math.MaxUint64, Logical: math.MaxUint64})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.ReplicaID(); got != "local" {
		t.Fatalf("ReplicaID() = %q", got)
	}
	h.now = func() time.Time { return time.UnixMilli(0) }
	before := h.Snapshot()
	if err := h.Witness(crdt.Tag{ReplicaID: "remote", WallTime: math.MaxUint64, Logical: math.MaxUint64}); !errors.Is(err, ErrClockExhausted) {
		t.Fatalf("Witness() error = %v", err)
	}
	if got := h.Snapshot(); got != before {
		t.Fatalf("failed Witness() mutated state: got %#v, want %#v", got, before)
	}
	h.now = nil
	if _, err := h.Now(); !errors.Is(err, ErrClockExhausted) {
		t.Fatalf("Now() with nil source error = %v", err)
	}
}
