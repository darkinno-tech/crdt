package register

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

func TestMVRegisterGoldenDeltaConflictAndNilPaths(t *testing.T) {
	// Independently fix the delta layout: context count, context entries,
	// value count, then dot/value entries.
	payload := frame.AppendUvarint(nil, 1)
	payload = appendMVBytes(payload, []byte("a"))
	payload = frame.AppendUvarint(payload, 2)
	payload = frame.AppendUvarint(payload, 1)
	payload = appendMVBytes(payload, []byte("a"))
	payload = frame.AppendUvarint(payload, 2)
	payload = appendMVBytes(payload, []byte("value"))
	golden, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDMVRegisterDelta, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	delta, err := UnmarshalMVRegisterDelta(golden)
	if err != nil {
		t.Fatal(err)
	}
	if encoded, err := delta.MarshalBinary(); err != nil || !bytes.Equal(encoded, golden) {
		t.Fatalf("golden re-encoding = %x, %v", encoded, err)
	}
	if values := delta.Values(); len(values) != 1 || values[0].ReplicaID != "a" || values[0].Counter != 2 || string(values[0].Value) != "value" {
		t.Fatalf("delta values = %#v", values)
	}
	value, err := NewMVRegister("local")
	if err != nil {
		t.Fatal(err)
	}
	if err := value.ApplyDelta(delta); err != nil {
		t.Fatal(err)
	}
	if got, ok := value.Value(); !ok || string(got) != "value" {
		t.Fatalf("golden apply = %q, %v", got, ok)
	}
	before, _ := value.MarshalBinary()
	conflicting := MVRegisterDelta{context: map[string]uint64{"a": 2}, values: map[mvDot][]byte{{replicaID: "a", counter: 2}: []byte("other")}}
	if err := value.ApplyDelta(conflicting); !errors.Is(err, ErrTagConflict) {
		t.Fatalf("same dot conflict = %v", err)
	}
	after, _ := value.MarshalBinary()
	if !bytes.Equal(before, after) {
		t.Fatal("conflicting delta mutated receiver")
	}
	if _, err := delta.Merge(MVRegisterDelta{}); !errors.Is(err, ErrInvalidMVRegister) {
		t.Fatalf("invalid delta merge = %v", err)
	}
	if err := value.ApplyDelta(MVRegisterDelta{}); !errors.Is(err, ErrInvalidMVRegister) {
		t.Fatalf("invalid apply = %v", err)
	}
	if err := value.Merge(nil); !errors.Is(err, ErrNilMVRegister) {
		t.Fatalf("nil merge = %v", err)
	}
	if _, err := NewMVRegisterFromSnapshot("local", snapshot.Snapshot{}); !errors.Is(err, ErrInvalidMVSnapshot) {
		t.Fatalf("invalid snapshot = %v", err)
	}

	var nilRegister *MVRegister
	if nilRegister.Values() != nil || nilRegister.State().Type != "mv-register" {
		t.Fatal("nil MV-Register accessors")
	}
	if got, ok := nilRegister.Value(); got != nil || ok {
		t.Fatalf("nil Value = %q, %v", got, ok)
	}
	if _, err := nilRegister.MarshalBinary(); !errors.Is(err, ErrNilMVRegister) {
		t.Fatalf("nil MarshalBinary = %v", err)
	}
	if _, err := nilRegister.Snapshot(); !errors.Is(err, ErrNilMVRegister) {
		t.Fatalf("nil Snapshot = %v", err)
	}
	if err := nilRegister.UnmarshalBinary(nil); !errors.Is(err, ErrNilMVRegister) {
		t.Fatalf("nil UnmarshalBinary = %v", err)
	}
}

func TestMVRegisterDecodeLimitsOverflowAndCanonicalRejection(t *testing.T) {
	value, err := NewMVRegister("local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Set([]byte("safe")); err != nil {
		t.Fatal(err)
	}
	before, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	limits := frame.DefaultLimits()
	limits.MaxElements = 0
	if err := value.UnmarshalBinaryWithLimits(before, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("element limit = %v", err)
	}
	after, _ := value.MarshalBinary()
	if !bytes.Equal(before, after) {
		t.Fatal("limit rejection mutated register")
	}

	// A value dot must be included in the context; this frame claims a:2 while
	// the context only proves a:1.
	payload := frame.AppendUvarint(nil, 1)
	payload = appendMVBytes(payload, []byte("a"))
	payload = frame.AppendUvarint(payload, 1)
	payload = frame.AppendUvarint(payload, 1)
	payload = appendMVBytes(payload, []byte("a"))
	payload = frame.AppendUvarint(payload, 2)
	payload = appendMVBytes(payload, []byte("bad"))
	bad, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDMVRegisterState, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := value.UnmarshalBinary(bad); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("uncovered dot = %v", err)
	}
	if _, err := UnmarshalMVRegisterDelta(before); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("state accepted as delta = %v", err)
	}

	overflow, err := NewMVRegister("overflow")
	if err != nil {
		t.Fatal(err)
	}
	overflow.context["overflow"] = math.MaxUint64
	if _, err := overflow.Set([]byte("next")); !errors.Is(err, ErrMVRegisterOverflow) {
		t.Fatalf("counter overflow = %v", err)
	}
	if _, err := marshalMVRegister(crdt.TypeIDMVRegisterState, map[string]uint64{"a": 1}, map[mvDot][]byte{{replicaID: "a", counter: 1}: []byte("x")}, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("marshal element limit = %v", err)
	}
	if compareMVDot(mvDot{replicaID: "a", counter: 1}, mvDot{replicaID: "a", counter: 2}) >= 0 || compareMVDot(mvDot{replicaID: "a", counter: 2}, mvDot{replicaID: "b", counter: 1}) >= 0 {
		t.Fatal("dot ordering is not canonical")
	}
}

func TestMVRegisterDuplicateAndInternalValidationPaths(t *testing.T) {
	value, err := NewMVRegister("local")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := value.Set([]byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	if err := value.ApplyDelta(delta); err != nil { // duplicate read-only fast path
		t.Fatal(err)
	}
	if _, err := (MVRegisterDelta{}).Merge(delta); !errors.Is(err, ErrInvalidMVRegister) {
		t.Fatalf("invalid left delta = %v", err)
	}
	invalidOther := &MVRegister{context: map[string]uint64{"": 1}, values: map[mvDot][]byte{}}
	if err := value.Merge(invalidOther); !errors.Is(err, ErrInvalidMVRegister) {
		t.Fatalf("invalid merge source = %v", err)
	}
	invalidReceiver := &MVRegister{}
	if err := invalidReceiver.ApplyDelta(delta); !errors.Is(err, ErrInvalidMVRegister) {
		t.Fatalf("invalid receiver state = %v", err)
	}
	var nilRegister *MVRegister
	if err := nilRegister.ApplyDelta(delta); !errors.Is(err, ErrNilMVRegister) {
		t.Fatalf("nil ApplyDelta = %v", err)
	}

	saved, err := value.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewMVRegisterFromSnapshot(" ", saved); !errors.Is(err, ErrInvalidReplicaID) {
		t.Fatalf("snapshot invalid replica = %v", err)
	}
	malformed, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDMVRegisterState, Payload: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	badSnapshot, err := snapshot.New(malformed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewMVRegisterFromSnapshot("restored", badSnapshot); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("malformed snapshot = %v", err)
	}

	if _, err := marshalMVRegister(crdt.TypeIDMVRegisterState, map[string]uint64{"a": 1}, map[mvDot][]byte{{replicaID: "a", counter: 2}: []byte("bad")}, frame.DefaultLimits()); !errors.Is(err, ErrInvalidMVRegister) {
		t.Fatalf("uncovered value marshal = %v", err)
	}
	limits := frame.DefaultLimits()
	limits.MaxStringBytes = 1
	if _, err := marshalMVRegister(crdt.TypeIDMVRegisterState, map[string]uint64{"long": 1}, map[mvDot][]byte{}, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("context string limit = %v", err)
	}
	if compareMVDot(mvDot{replicaID: "b", counter: 1}, mvDot{replicaID: "a", counter: 1}) <= 0 || compareMVDot(mvDot{replicaID: "a", counter: 3}, mvDot{replicaID: "a", counter: 2}) <= 0 || compareMVDot(mvDot{replicaID: "a", counter: 2}, mvDot{replicaID: "a", counter: 2}) != 0 {
		t.Fatal("full dot comparator ordering failed")
	}
}
