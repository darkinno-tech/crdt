package text

import (
	"errors"
	"reflect"
	"testing"

	"github.com/darkinno/crdt"
	frame "github.com/darkinno/crdt/encoding"
	"github.com/darkinno/crdt/snapshot"
)

func TestRGABinaryStateDeltaAndSnapshotRoundTrip(t *testing.T) {
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	insert, err := source.Insert(0, "ab")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Delete(0, 1); err != nil {
		t.Fatal(err)
	}
	first, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.MarshalBinary()
	if err != nil || string(first) != string(second) {
		t.Fatalf("state encoding is not deterministic: %v", err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.UnmarshalBinary(first); err != nil {
		t.Fatal(err)
	}
	if got := target.String(); got != "b" {
		t.Fatalf("state round trip = %q", got)
	}
	encodedDelta, err := insert.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decodedDelta, err := UnmarshalRGADelta(encodedDelta)
	if err != nil {
		t.Fatal(err)
	}
	third, err := New("third")
	if err != nil {
		t.Fatal(err)
	}
	if err := third.ApplyDelta(decodedDelta); err != nil {
		t.Fatal(err)
	}
	if got := third.String(); got != "ab" {
		t.Fatalf("delta round trip = %q", got)
	}
	saved, err := source.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.String(); got != "b" {
		t.Fatalf("snapshot round trip = %q", got)
	}
	nodes, tombstones, err := unmarshalRGA(saved.Bytes(), crdt.TypeIDRGAState, frame.DefaultLimits(), true)
	if err != nil {
		t.Fatalf("snapshot state decode = %v", err)
	}
	if got, want := saved.Frontier(), frontierForState(nodes, tombstones); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot frontier = %#v, want %#v", got, want)
	}
}

func TestRGAWireRejectsWrongTypeLimitsAndMalformedState(t *testing.T) {
	value, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Insert(0, "a"); err != nil {
		t.Fatal(err)
	}
	encoded, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalRGADelta(encoded); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("wrong type = %v", err)
	}
	limits := frame.DefaultLimits()
	limits.MaxElements = 0
	if err := value.UnmarshalBinaryWithLimits(encoded, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("limit = %v", err)
	}
	bad, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRGAState, Payload: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := value.UnmarshalBinary(bad); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("malformed = %v", err)
	}
}

func TestRGAMarshalRejectsIncompleteStateUntilParentArrives(t *testing.T) {
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := source.Insert(0, "a")
	if err != nil {
		t.Fatal(err)
	}
	child, err := source.Insert(1, "b")
	if err != nil {
		t.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(child); err != nil {
		t.Fatal(err)
	}
	if _, err := target.MarshalBinary(); !errors.Is(err, ErrIncompleteState) {
		t.Fatalf("MarshalBinary() error = %v, want %v", err, ErrIncompleteState)
	}
	if _, err := target.SnapshotCurrentState(); !errors.Is(err, ErrIncompleteState) {
		t.Fatalf("SnapshotCurrentState() error = %v, want %v", err, ErrIncompleteState)
	}
	if err := target.ApplyDelta(parent); err != nil {
		t.Fatal(err)
	}
	state, err := target.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := New("restored")
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.UnmarshalBinary(state); err != nil {
		t.Fatalf("complete state could not be restored: %v", err)
	}
	if got := restored.String(); got != "ab" {
		t.Fatalf("restored text = %q, want ab", got)
	}
}

func TestRGAMarshalChecksTagStringLimit(t *testing.T) {
	value, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Insert(0, "a"); err != nil {
		t.Fatal(err)
	}
	limits := frame.DefaultLimits()
	limits.MaxStringBytes = 1
	if _, err := marshalRGAWithLimits(crdt.TypeIDRGAState, value.nodes, value.tombstones, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("marshal limit error = %v, want %v", err, frame.ErrFrameLimit)
	}
}

func TestRGAWireNilAndSnapshotValidationPaths(t *testing.T) {
	var nilValue *RGA
	if _, err := nilValue.MarshalBinary(); err != ErrNilText {
		t.Fatalf("nil MarshalBinary = %v", err)
	}
	if _, _, err := nilValue.MarshalBinaryWithClockState(); err != ErrNilText {
		t.Fatalf("nil MarshalBinaryWithClockState = %v", err)
	}
	if _, err := nilValue.SnapshotCurrentState(); err != ErrNilText {
		t.Fatalf("nil SnapshotCurrentState = %v", err)
	}
	if _, err := NewFromSnapshot(snapshot.Snapshot{TypeID: crdt.TypeIDGCounterState}); err != ErrInvalidDelta {
		t.Fatalf("wrong snapshot type = %v", err)
	}
	invalid := Delta{nodes: map[Position]node{Position{}: {rune: 'x'}}}
	if _, err := invalid.MarshalBinary(); err != ErrInvalidDelta {
		t.Fatalf("invalid delta marshal = %v", err)
	}
	value, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Snapshot(map[string]crdt.Tag{"wrong": {ReplicaID: "other"}}); err == nil {
		t.Fatal("invalid frontier accepted")
	}
}

func FuzzRGAUnmarshal(f *testing.F) {
	value, err := New("seed")
	if err != nil {
		f.Fatal(err)
	}
	if _, err := value.Insert(0, "seed"); err != nil {
		f.Fatal(err)
	}
	state, err := value.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(state)
	f.Fuzz(func(t *testing.T, data []byte) {
		target, err := New("target")
		if err != nil {
			t.Fatal(err)
		}
		if err := target.UnmarshalBinary(data); err == nil && target.State().ElementCount < 0 {
			t.Fatal("negative visible count")
		}
		if delta, err := UnmarshalRGADelta(data); err == nil {
			if err := target.ApplyDelta(delta); err != nil {
				t.Fatalf("decoded delta rejected: %v", err)
			}
		}
	})
}
