package counter

import (
	"errors"
	"reflect"
	"testing"
)

func TestGCounterNilAndDeltaValueBoundaries(t *testing.T) {
	t.Parallel()
	var nilCounter *GCounter
	if _, err := nilCounter.Increment(1); !errors.Is(err, ErrNilCounter) {
		t.Fatalf("nil Increment() error = %v", err)
	}
	if _, err := nilCounter.Value(); !errors.Is(err, ErrNilCounter) {
		t.Fatalf("nil Value() error = %v", err)
	}
	if got := nilCounter.Counts(); got != nil {
		t.Fatalf("nil Counts() = %#v", got)
	}
	if got := nilCounter.State(); got.Type != "gcounter" {
		t.Fatalf("nil State() = %#v", got)
	}
	if _, err := nilCounter.MarshalBinary(); !errors.Is(err, ErrNilCounter) {
		t.Fatalf("nil MarshalBinary() error = %v", err)
	}
	if err := nilCounter.UnmarshalBinary(nil); !errors.Is(err, ErrNilCounter) {
		t.Fatalf("nil UnmarshalBinary() error = %v", err)
	}
	if err := nilCounter.ApplyDelta(GCounterDelta{}); !errors.Is(err, ErrNilCounter) {
		t.Fatalf("nil ApplyDelta() error = %v", err)
	}

	left := GCounterDelta{counts: map[string]uint64{"a": 1}}
	right := GCounterDelta{counts: map[string]uint64{"a": 3, "b": 2}}
	merged, err := left.Merge(right)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := merged.Counts(), map[string]uint64{"a": 3, "b": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("delta merge = %#v, want %#v", got, want)
	}
	copy := merged.Counts()
	copy["a"] = 99
	if merged.Counts()["a"] != 3 {
		t.Fatal("delta Counts() aliases internal state")
	}
	if _, err := (GCounterDelta{counts: map[string]uint64{" ": 1}}).Merge(right); !errors.Is(err, ErrInvalidReplicaID) {
		t.Fatalf("invalid delta error = %v", err)
	}
}
