package register

import (
	"bytes"
	"testing"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/clock"
)

func TestLWWConvergesAndCopies(t *testing.T) {
	left, err := NewLWW("left")
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewLWW("right")
	if err != nil {
		t.Fatal(err)
	}
	input := []byte("one")
	if err := left.Set(input); err != nil {
		t.Fatal(err)
	}
	input[0] = 'X'
	if got, _ := left.Get(); !bytes.Equal(got, []byte("one")) {
		t.Fatalf("copy = %q", got)
	}
	if err := right.Set([]byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := left.Merge(right); err != nil {
		t.Fatal(err)
	}
	if err := right.Merge(left); err != nil {
		t.Fatal(err)
	}
	leftValue, _ := left.Get()
	rightValue, _ := right.Get()
	if !bytes.Equal(leftValue, rightValue) {
		t.Fatalf("diverged: %q %q", leftValue, rightValue)
	}
}

func TestRegisterErrorsAndMaxProperties(t *testing.T) {
	if _, err := NewLWW(" "); err != ErrInvalidReplicaID {
		t.Fatalf("NewLWW = %v", err)
	}
	var nilLWW *LWW
	if err := nilLWW.Set(nil); err != ErrNilLWW {
		t.Fatalf("nil Set = %v", err)
	}
	if err := nilLWW.Merge(nil); err != ErrNilLWW {
		t.Fatalf("nil Merge = %v", err)
	}
	max := NewMax()
	if err := max.Set(4); err != nil {
		t.Fatal(err)
	}
	if err := max.Set(2); err != nil {
		t.Fatal(err)
	}
	if got, _ := max.Get(); got != 4 {
		t.Fatalf("max = %d", got)
	}
	other := NewMax()
	if err := other.Set(9); err != nil {
		t.Fatal(err)
	}
	if err := max.Merge(other); err != nil {
		t.Fatal(err)
	}
	if got, _ := max.Get(); got != 9 {
		t.Fatalf("merged max = %d", got)
	}
	var nilMax *Max
	if err := nilMax.Set(1); err != ErrNilMax {
		t.Fatalf("nil max = %v", err)
	}
}

func TestRegisterMetadataAndConflictPaths(t *testing.T) {
	if _, err := NewLWWFromClock(clock.State{}); err != ErrInvalidReplicaID {
		t.Fatalf("NewLWWFromClock = %v", err)
	}
	value, err := NewLWW("local")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := value.Get(); ok || value.ClockState().ReplicaID != "local" || value.State().ElementCount != 0 {
		t.Fatal("empty LWW metadata")
	}
	if err := value.Merge(value); err != nil {
		t.Fatal(err)
	}
	empty, err := NewLWW("empty")
	if err != nil {
		t.Fatal(err)
	}
	if err := value.Merge(empty); err != nil {
		t.Fatal(err)
	}
	tag := crdt.Tag{ReplicaID: "remote", WallTime: 1}
	value.tag, value.value, value.hasValue = tag, []byte("left"), true
	other, err := NewLWW("other")
	if err != nil {
		t.Fatal(err)
	}
	other.tag, other.value, other.hasValue = tag, []byte("right"), true
	if err := value.Merge(other); err != ErrTagConflict {
		t.Fatalf("conflict = %v", err)
	}
	var nilValue *LWW
	if _, ok := nilValue.Get(); ok || nilValue.ClockState() != (clock.State{}) || nilValue.State().Type != "lww-register" {
		t.Fatal("nil LWW")
	}

	max := NewMax()
	if _, ok := max.Get(); ok || max.State().ElementCount != 0 {
		t.Fatal("empty max")
	}
	if err := max.Merge(max); err != nil {
		t.Fatal(err)
	}
	if err := max.Merge(nil); err != ErrNilMax {
		t.Fatalf("nil merge = %v", err)
	}
	var nilMax *Max
	if _, ok := nilMax.Get(); ok || nilMax.State().Type != "max-register" || nilMax.Merge(nil) != ErrNilMax {
		t.Fatal("nil max metadata")
	}
}
