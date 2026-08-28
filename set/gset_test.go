package set

import (
	"bytes"
	"errors"
	"testing"

	"github.com/darkinno-tech/crdt"
	frame "github.com/darkinno-tech/crdt/encoding"
)

func TestGSetConvergesAndDeltaIsIdempotent(t *testing.T) {
	codec := stringCodec{id: "example.com/gset-string/v1"}
	left, err := NewGSet("left", codec)
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewGSet("right", codec)
	if err != nil {
		t.Fatal(err)
	}
	leftDelta, err := left.Add("left")
	if err != nil {
		t.Fatal(err)
	}
	rightDelta, err := right.Add("right")
	if err != nil {
		t.Fatal(err)
	}
	if err := left.ApplyDelta(rightDelta); err != nil {
		t.Fatal(err)
	}
	if err := left.ApplyDelta(rightDelta); err != nil {
		t.Fatal(err)
	}
	if err := right.ApplyDelta(leftDelta); err != nil {
		t.Fatal(err)
	}
	if err := left.Merge(right); err != nil {
		t.Fatal(err)
	}
	if err := right.Merge(left); err != nil {
		t.Fatal(err)
	}
	for _, element := range []string{"left", "right"} {
		if !left.Contains(element) || !right.Contains(element) {
			t.Fatalf("missing merged element %q", element)
		}
	}
	if left.State().ElementCount != 2 || left.State().ReplicaID != "left" {
		t.Fatalf("state = %#v", left.State())
	}
}

func TestGSetWireSnapshotAndTypeIsolation(t *testing.T) {
	codec := stringCodec{id: "example.com/gset-wire/v1"}
	value, err := NewGSet("writer", codec)
	if err != nil {
		t.Fatal(err)
	}
	for _, element := range []string{"z", "a", "middle"} {
		if _, err := value.Add(element); err != nil {
			t.Fatal(err)
		}
	}
	state, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewGSet("restored", codec)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.UnmarshalBinary(state); err != nil {
		t.Fatal(err)
	}
	encoded, err := restored.MarshalBinary()
	if err != nil || !bytes.Equal(encoded, state) {
		t.Fatalf("canonical state = %x, %v", encoded, err)
	}
	saved, err := value.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	fromSnapshot, err := NewGSetFromSnapshot("new-owner", saved, codec)
	if err != nil || !fromSnapshot.Contains("middle") {
		t.Fatalf("snapshot = %v, contains=%v", err, fromSnapshot != nil && fromSnapshot.Contains("middle"))
	}
	delta, err := value.Add("delta")
	if err != nil {
		t.Fatal(err)
	}
	encodedDelta, err := delta.MarshalBinary(codec)
	if err != nil {
		t.Fatal(err)
	}
	decodedDelta, err := UnmarshalGSetDelta(encodedDelta, codec)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.ApplyDelta(decodedDelta); err != nil || !restored.Contains("delta") {
		t.Fatalf("delta = %v", err)
	}
	if _, err := UnmarshalGSetDelta(state, codec); !errors.Is(err, ErrCodecMismatch) {
		t.Fatalf("state as delta = %v", err)
	}
	wrong, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDGSetState, CodecID: "wrong", Payload: []byte{0}})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := restored.MarshalBinary()
	if err := restored.UnmarshalBinary(wrong); !errors.Is(err, ErrCodecMismatch) {
		t.Fatalf("wrong codec = %v", err)
	}
	after, _ := restored.MarshalBinary()
	if !bytes.Equal(before, after) {
		t.Fatal("failed unmarshal mutated G-Set")
	}
}

func TestGSetRejectsNonCanonicalAndInvalidInputs(t *testing.T) {
	codec := stringCodec{id: "example.com/gset-invalid/v1"}
	value, err := NewGSet("local", codec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewGSet(" ", codec); !errors.Is(err, ErrInvalidReplicaID) {
		t.Fatalf("invalid replica = %v", err)
	}
	if _, err := NewGSet[string]("local", nil); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("invalid codec = %v", err)
	}
	var nilCodec *stringCodec
	if _, err := NewGSet("local", nilCodec); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("typed nil codec = %v", err)
	}
	if _, err := UnmarshalGSetDelta([]byte("bad"), nilCodec); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("typed nil delta codec = %v", err)
	}
	if err := value.ApplyDelta(GSetDelta[string]{}); !errors.Is(err, ErrInvalidGSet) {
		t.Fatalf("invalid delta = %v", err)
	}
	// This payload orders "z" before "a", which a canonical decoder rejects.
	payload := frame.AppendUvarint(nil, 2)
	payload = appendBytes(payload, []byte("z"))
	payload = appendBytes(payload, []byte("a"))
	bad, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDGSetState, CodecID: codec.ID(), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := value.UnmarshalBinary(bad); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("unsorted state = %v", err)
	}
}

func FuzzGSetUnmarshalBinary(f *testing.F) {
	codec := stringCodec{id: "example.com/gset-fuzz/v1"}
	value, err := NewGSet("seed", codec)
	if err != nil {
		f.Fatal(err)
	}
	if _, err := value.Add("seed"); err != nil {
		f.Fatal(err)
	}
	seed, err := value.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte("not a frame"))
	f.Fuzz(func(t *testing.T, data []byte) {
		target, err := NewGSet("target", codec)
		if err != nil {
			t.Fatal(err)
		}
		if err := target.UnmarshalBinary(data); err == nil && target.State().ElementCount < 0 {
			t.Fatal("successful decode produced impossible count")
		}
	})
}
