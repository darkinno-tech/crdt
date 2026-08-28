package lww

import (
	"errors"
	"reflect"
	"testing"

	frame "github.com/darkinno-tech/crdt/encoding"
)

func TestMapDecodeOptionsRejectBeforeReceiverMutation(t *testing.T) {
	source, err := NewMap("source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.SetWithDelta("one", []byte("value")); err != nil {
		t.Fatal(err)
	}
	if _, err := source.SetWithDelta("two", []byte("value")); err != nil {
		t.Fatal(err)
	}
	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}

	options := MapOptions{MaxEntries: 1, MaxKeyBytes: 4, MaxValueBytes: 4}
	target, err := NewMapWithOptions("target", options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.SetWithDelta("keep", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	before, err := target.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	beforeClock := target.ClockState()
	if err := target.UnmarshalBinary(state); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("over-limit state = %v", err)
	}
	after, err := target.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) || target.ClockState() != beforeClock {
		t.Fatalf("rejected state changed target: state=%x clock=%#v", after, target.ClockState())
	}

	change, err := source.SetWithDelta("longer", []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := change.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalMapDeltaWithOptions(encoded, frame.DefaultLimits(), options); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("over-limit delta = %v", err)
	}

	valueDelta, err := source.SetWithDelta("key", []byte("longer"))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = valueDelta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalMapDeltaWithOptions(encoded, frame.DefaultLimits(), options); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("over-limit value delta = %v", err)
	}
}
