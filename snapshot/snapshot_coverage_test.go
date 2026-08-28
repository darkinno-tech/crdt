package snapshot

import (
	"errors"
	"testing"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/counter"
)

func TestSnapshotRejectsInvalidFrontierAndRecoveryLimits(t *testing.T) {
	value, err := counter.NewGCounter("source")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := value.Increment(1)
	if err != nil {
		t.Fatal(err)
	}
	state, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(state, map[string]crdt.Tag{"wrong": {ReplicaID: "other"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("New() invalid frontier error = %v", err)
	}
	saved, err := New(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewRecoveryPlan(Snapshot{}, nil, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty snapshot error = %v", err)
	}
	if _, err := NewRecoveryPlan(saved, nil, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero budget error = %v", err)
	}
	if _, err := NewRecoveryPlan(saved, [][]byte{encoded, encoded}, len(encoded)); !errors.Is(err, ErrLimit) {
		t.Fatalf("over-budget deltas error = %v", err)
	}
}
