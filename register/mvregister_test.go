package register

import (
	"bytes"
	"errors"
	"testing"

	"github.com/im10furry/crdt"
	frame "github.com/im10furry/crdt/encoding"
)

func TestMVRegisterRetainsConcurrentWritesAndOverwritesObservedValues(t *testing.T) {
	left, err := NewMVRegister("left")
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewMVRegister("right")
	if err != nil {
		t.Fatal(err)
	}
	leftWrite, err := left.Set([]byte("left"))
	if err != nil {
		t.Fatal(err)
	}
	rightWrite, err := right.Set([]byte("right"))
	if err != nil {
		t.Fatal(err)
	}
	if err := left.ApplyDelta(rightWrite); err != nil {
		t.Fatal(err)
	}
	if err := right.ApplyDelta(leftWrite); err != nil {
		t.Fatal(err)
	}
	if _, ok := left.Value(); ok {
		t.Fatal("concurrent register unexpectedly has one value")
	}
	entries := left.Values()
	if len(entries) != 2 || string(entries[0].Value) != "left" || string(entries[1].Value) != "right" {
		t.Fatalf("concurrent values = %#v", entries)
	}
	overwrite, err := left.Set([]byte("resolved"))
	if err != nil {
		t.Fatal(err)
	}
	if err := right.ApplyDelta(overwrite); err != nil {
		t.Fatal(err)
	}
	for _, value := range []*MVRegister{left, right} {
		got, ok := value.Value()
		if !ok || !bytes.Equal(got, []byte("resolved")) || len(value.Values()) != 1 {
			t.Fatalf("resolved value = %q, %v, %#v", got, ok, value.Values())
		}
	}
}

func TestMVRegisterMergeAlgebraAndDeltaCoalescing(t *testing.T) {
	makeRegister := func(t *testing.T, id string, value string) *MVRegister {
		t.Helper()
		register, err := NewMVRegister(id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := register.Set([]byte(value)); err != nil {
			t.Fatal(err)
		}
		return register
	}
	left := makeRegister(t, "a", "a")
	middle := makeRegister(t, "b", "b")
	right := makeRegister(t, "c", "c")
	join := func(first, second *MVRegister) []byte {
		t.Helper()
		copy, err := NewMVRegister("copy")
		if err != nil {
			t.Fatal(err)
		}
		if err := copy.Merge(first); err != nil {
			t.Fatal(err)
		}
		if err := copy.Merge(second); err != nil {
			t.Fatal(err)
		}
		encoded, err := copy.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	if !bytes.Equal(join(left, middle), join(middle, left)) {
		t.Fatal("merge is not commutative")
	}
	mergeIntoNew := func(t *testing.T, id string, registers ...*MVRegister) *MVRegister {
		t.Helper()
		result, err := NewMVRegister(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, register := range registers {
			if err := result.Merge(register); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	leftIntermediate := mergeIntoNew(t, "left-intermediate", left, middle)
	joinedLeft := mergeIntoNew(t, "joined-left", leftIntermediate, right)
	rightIntermediate := mergeIntoNew(t, "right-intermediate", middle, right)
	joinedRight := mergeIntoNew(t, "joined-right", left, rightIntermediate)
	leftBytes, _ := joinedLeft.MarshalBinary()
	rightBytes, _ := joinedRight.MarshalBinary()
	if !bytes.Equal(leftBytes, rightBytes) {
		t.Fatal("merge is not associative")
	}
	before := append([]byte(nil), leftBytes...)
	if err := joinedLeft.Merge(joinedLeft); err != nil {
		t.Fatal(err)
	}
	after, _ := joinedLeft.MarshalBinary()
	if !bytes.Equal(before, after) {
		t.Fatal("merge is not idempotent")
	}
	first, err := left.Set([]byte("next"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := left.Set([]byte("final"))
	if err != nil {
		t.Fatal(err)
	}
	combined, err := first.Merge(second)
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewMVRegister("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(combined); err != nil {
		t.Fatal(err)
	}
	if got, ok := target.Value(); !ok || string(got) != "final" {
		t.Fatalf("coalesced value = %q, %v", got, ok)
	}
}

func TestMVRegisterWireSnapshotCopiesAndErrors(t *testing.T) {
	value, err := NewMVRegister("local")
	if err != nil {
		t.Fatal(err)
	}
	input := []byte("value")
	delta, err := value.Set(input)
	if err != nil {
		t.Fatal(err)
	}
	input[0] = 'X'
	if got, _ := value.Value(); string(got) != "value" {
		t.Fatalf("input aliased state: %q", got)
	}
	encodedDelta, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decodedDelta, err := UnmarshalMVRegisterDelta(encodedDelta)
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewMVRegister("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(decodedDelta); err != nil {
		t.Fatal(err)
	}
	state, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewMVRegister("restored")
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.UnmarshalBinary(state); err != nil {
		t.Fatal(err)
	}
	reencoded, _ := restored.MarshalBinary()
	if !bytes.Equal(state, reencoded) {
		t.Fatal("state did not re-encode canonically")
	}
	saved, err := value.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	fromSnapshot, err := NewMVRegisterFromSnapshot("again", saved)
	if err != nil || fromSnapshot.State().ElementCount != 1 {
		t.Fatalf("snapshot = %v, %#v", err, fromSnapshot)
	}
	if _, err := UnmarshalMVRegisterDelta(state); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("state as delta = %v", err)
	}
	wrong, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDMVRegisterState, CodecID: "wrong", Payload: []byte{0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), state...)
	if err := restored.UnmarshalBinary(wrong); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("wrong codec = %v", err)
	}
	after, _ := restored.MarshalBinary()
	if !bytes.Equal(before, after) {
		t.Fatal("failed unmarshal mutated MV-Register")
	}
	if _, err := NewMVRegister(" "); !errors.Is(err, ErrInvalidReplicaID) {
		t.Fatalf("invalid replica = %v", err)
	}
	var nilRegister *MVRegister
	if _, err := nilRegister.Set(nil); !errors.Is(err, ErrNilMVRegister) {
		t.Fatalf("nil Set = %v", err)
	}
}

func FuzzMVRegisterUnmarshal(f *testing.F) {
	value, err := NewMVRegister("seed")
	if err != nil {
		f.Fatal(err)
	}
	if _, err := value.Set([]byte("seed")); err != nil {
		f.Fatal(err)
	}
	seed, err := value.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte("not a frame"))
	f.Fuzz(func(t *testing.T, data []byte) {
		target, err := NewMVRegister("target")
		if err != nil {
			t.Fatal(err)
		}
		if err := target.UnmarshalBinary(data); err == nil && target.State().ElementCount < 0 {
			t.Fatal("successful decode produced impossible count")
		}
	})
}
