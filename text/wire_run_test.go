package text

import (
	"errors"
	"testing"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
)

func TestRGARunFramesRoundTripAndCompactLinearInsert(t *testing.T) {
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := source.Insert(0, "collaborative text")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	run, err := delta.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(run) >= len(v1) {
		t.Fatalf("run delta size = %d, v1 size = %d", len(run), len(v1))
	}
	decoded, err := UnmarshalRGARunDelta(run)
	if err != nil {
		t.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
	if got := target.String(); got != source.String() {
		t.Fatalf("run delta text = %q, want %q", got, source.String())
	}

	if _, err := source.Delete(3, 5); err != nil {
		t.Fatal(err)
	}
	state, err := source.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := New("recovered")
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.UnmarshalRunBinary(state); err != nil {
		t.Fatal(err)
	}
	if got := recovered.String(); got != source.String() {
		t.Fatalf("run state text = %q, want %q", got, source.String())
	}
	snapshot, err := source.SnapshotRunCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TypeID != crdt.TypeIDRGARunState {
		t.Fatalf("snapshot type = %d", snapshot.TypeID)
	}
	fromSnapshot, err := NewFromSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if got := fromSnapshot.String(); got != source.String() {
		t.Fatalf("run snapshot text = %q, want %q", got, source.String())
	}
}

func TestRGARunFramesRejectWrongTypeAndNonCanonicalPayload(t *testing.T) {
	value, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Insert(0, "abc"); err != nil {
		t.Fatal(err)
	}
	state, err := value.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalRGARunDelta(state); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("run wrong type = %v", err)
	}
	decoded, err := frame.UnmarshalFrame(state, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	// A valid envelope with a non-canonical run block must not be accepted.
	decoded.Payload[0] = 2
	malformed, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRGARunState, Payload: decoded.Payload})
	if err != nil {
		t.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.UnmarshalRunBinary(malformed); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("non-canonical run frame = %v", err)
	}
}

func FuzzRGARunUnmarshal(f *testing.F) {
	value, err := New("seed")
	if err != nil {
		f.Fatal(err)
	}
	if _, err := value.Insert(0, "seed"); err != nil {
		f.Fatal(err)
	}
	state, err := value.MarshalRunBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(state)
	f.Fuzz(func(t *testing.T, data []byte) {
		target, err := New("target")
		if err != nil {
			t.Fatal(err)
		}
		if err := target.UnmarshalRunBinary(data); err == nil && target.State().ElementCount < 0 {
			t.Fatal("negative visible count")
		}
		if delta, err := UnmarshalRGARunDelta(data); err == nil {
			if err := target.ApplyDelta(delta); err != nil {
				t.Fatalf("decoded run delta rejected: %v", err)
			}
		}
	})
}
