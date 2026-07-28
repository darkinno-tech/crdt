package set

import (
	"bytes"
	"errors"
	"testing"

	"github.com/darkinno/crdt"
	"github.com/darkinno/crdt/counter"
	frame "github.com/darkinno/crdt/encoding"
	"github.com/darkinno/crdt/snapshot"
)

type failingStringCodec struct{ stringCodec }

func (failingStringCodec) Marshal(string) ([]byte, error) { return nil, errors.New("encode failure") }

type rejectingStringCodec struct{ stringCodec }

func (rejectingStringCodec) Unmarshal([]byte) (string, error) {
	return "", errors.New("decode failure")
}

type collidingStringCodec struct{ stringCodec }

func (collidingStringCodec) Marshal(string) ([]byte, error) { return []byte("same"), nil }

func TestORSetStateSnapshotAndMergeErrorPaths(t *testing.T) {
	codec := stringCodec{id: "example.com/more-coverage/v1"}
	value := mustNewORSet(t, "replica", codec)
	if _, err := value.Add("item"); err != nil {
		t.Fatal(err)
	}
	if state := value.State(); state.Type != "orset" || state.ReplicaID != "replica" || state.ElementCount != 1 {
		t.Fatalf("State() = %#v", state)
	}
	if err := value.Merge(nil); !errors.Is(err, ErrNilORSet) {
		t.Fatalf("Merge(nil) error = %v", err)
	}
	if err := value.Merge(value); err != nil {
		t.Fatalf("Merge(self) error = %v", err)
	}
	encoded, savedClock, err := value.MarshalBinaryWithClockState()
	if err != nil || len(encoded) == 0 || savedClock.ReplicaID != "replica" {
		t.Fatalf("MarshalBinaryWithClockState() = %d bytes, %#v, %v", len(encoded), savedClock, err)
	}
	if _, err := marshalORSetWithLimits(crdt.TypeIDORSetState, failingStringCodec{stringCodec{id: "failing"}}, map[string]map[crdt.Tag]struct{}{
		"item": {
			crdt.Tag{ReplicaID: "r"}: {},
		},
	}, nil, defaultLimits()); err == nil {
		t.Fatal("marshalORSet accepted codec marshal failure")
	}

	counterValue, err := counter.NewGCounter("counter")
	if err != nil {
		t.Fatal(err)
	}
	counterState, err := counterValue.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewORSetFromSnapshot(mustSnapshot(t, counterState), codec); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("counter snapshot error = %v", err)
	}
}

func TestORSetRejectsTypedNilAndSnapshotDecodeFailure(t *testing.T) {
	var nilCodec *stringCodec
	if _, err := NewORSet("replica", nilCodec); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("typed nil codec error = %v", err)
	}
	codec := stringCodec{id: "example.com/restore/v1"}
	value := mustNewORSet(t, "replica", codec)
	if _, err := value.Add("item"); err != nil {
		t.Fatal(err)
	}
	saved, err := value.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewORSetFromSnapshot(saved, rejectingStringCodec{stringCodec{id: codec.id}}); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("snapshot decoder failure error = %v", err)
	}
	if _, err := UnmarshalORSetDelta([]byte("bad"), nilCodec); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("typed nil delta codec error = %v", err)
	}
}

func TestORSetRejectsNonCanonicalTagsAndCodecCollisions(t *testing.T) {
	validTag := crdt.Tag{ReplicaID: "replica", WallTime: 2, Logical: 3}
	encoded := appendTag(nil, validTag)
	decoded, next, ok := readTag(encoded, 0, len(validTag.ReplicaID))
	if !ok || next != len(encoded) || decoded != validTag {
		t.Fatalf("readTag() = %#v, %d, %v", decoded, next, ok)
	}
	if _, _, ok := readTag(encoded, 0, len(validTag.ReplicaID)-1); ok {
		t.Fatal("readTag() accepted oversized replica ID")
	}
	if _, _, ok := readTag(appendTag(nil, crdt.Tag{}), 0, 16); ok {
		t.Fatal("readTag() accepted empty replica ID")
	}
	if _, _, ok := readTag([]byte{0x80}, 0, 16); ok {
		t.Fatal("readTag() accepted truncated length")
	}

	codec := collidingStringCodec{stringCodec{id: "example.com/collision/v1"}}
	adds := map[string]map[crdt.Tag]struct{}{
		"left":  {crdt.Tag{ReplicaID: "a"}: {}},
		"right": {crdt.Tag{ReplicaID: "b"}: {}},
	}
	if _, err := marshalORSetWithLimits(crdt.TypeIDORSetState, codec, adds, nil, defaultLimits()); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("colliding codec error = %v", err)
	}

	value := mustNewORSet(t, "replica", stringCodec{id: "example.com/merge-invalid/v1"})
	valid, err := value.Add("item")
	if err != nil {
		t.Fatal(err)
	}
	invalid := ORSetDelta[string]{
		adds: map[string]map[crdt.Tag]struct{}{
			"bad": {
				crdt.Tag{}: {},
			},
		},
	}
	if _, err := invalid.Merge(valid); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("invalid delta merge error = %v", err)
	}
	if saved, err := value.Snapshot(value.Frontier()); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	} else if _, ok := saved.ClockState(); !ok {
		t.Fatal("Snapshot() did not preserve clock state")
	}
}

func TestORSetFastJoinPreservesTombstonesAndRejectsConflictingTags(t *testing.T) {
	codec := stringCodec{id: "example.com/join-invariants/v1"}
	source := mustNewORSet(t, "source", codec)
	oldAdd, err := source.Add("old")
	if err != nil {
		t.Fatal(err)
	}
	remove, err := source.Remove("old")
	if err != nil {
		t.Fatal(err)
	}
	target := mustNewORSet(t, "target", codec)
	if err := target.ApplyDelta(remove); err != nil {
		t.Fatal(err)
	}
	clockAfterRemove := target.ClockState()
	if err := target.ApplyDelta(oldAdd); err != nil {
		t.Fatal(err)
	}
	if target.Contains("old") || target.State().ElementCount != 0 || target.State().TombstoneCount != 1 {
		t.Fatalf("tombstone-first delivery state = %#v", target.State())
	}
	if target.ClockState() != clockAfterRemove {
		t.Fatal("tombstone-covered add advanced receiver clock")
	}

	before, err := target.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	beforeClock := target.ClockState()
	tag := crdt.Tag{ReplicaID: "conflict", WallTime: 1, Logical: 1}
	invalidDeltas := []ORSetDelta[string]{
		{adds: map[string]map[crdt.Tag]struct{}{"left": {tag: {}}, "right": {tag: {}}}},
		{adds: map[string]map[crdt.Tag]struct{}{"left": {tag: {}}}, tombstones: map[crdt.Tag]struct{}{tag: {}}},
	}
	for _, invalid := range invalidDeltas {
		if err := target.ApplyDelta(invalid); !errors.Is(err, ErrInvalidDelta) {
			t.Fatalf("ApplyDelta(conflicting tags) error = %v, want %v", err, ErrInvalidDelta)
		}
		after, err := target.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before) || target.ClockState() != beforeClock {
			t.Fatal("invalid delta modified receiver state or clock")
		}
	}
}

func TestORSetDuplicateDeltaDoesNotAdvanceClock(t *testing.T) {
	codec := stringCodec{id: "example.com/duplicate-clock/v1"}
	source := mustNewORSet(t, "source", codec)
	delta, err := source.Add("item")
	if err != nil {
		t.Fatal(err)
	}
	target := mustNewORSet(t, "target", codec)
	if err := target.ApplyDelta(delta); err != nil {
		t.Fatal(err)
	}
	clockAfterFirstApply := target.ClockState()
	if err := target.ApplyDelta(delta); err != nil {
		t.Fatal(err)
	}
	if target.ClockState() != clockAfterFirstApply {
		t.Fatal("duplicate delta advanced receiver clock")
	}
	if !target.Contains("item") {
		t.Fatal("duplicate delta lost the original add")
	}
}

func TestORSetMarshalBinaryMatchesCanonicalFrameFixture(t *testing.T) {
	codec := stringCodec{id: "test"}
	adds := map[string]map[crdt.Tag]struct{}{
		"a": {{ReplicaID: "r", WallTime: 1, Logical: 2}: {}},
	}
	tombstones := map[crdt.Tag]struct{}{{ReplicaID: "r", WallTime: 3, Logical: 4}: {}}
	got, err := marshalORSetWithLimits(crdt.TypeIDORSetState, codec, adds, tombstones, defaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte{1, 1, 'a', 1, 1, 'r', 1, 2, 1, 1, 'r', 3, 4}
	want, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDORSetState, CodecID: codec.ID(), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("MarshalBinary fixture = %x, want %x", got, want)
	}
}

func TestORSetMarshalBinaryPreservesCanonicalStateFrame(t *testing.T) {
	codec := stringCodec{id: "test"}
	value := mustNewORSet(t, "local", codec)
	payload := []byte{1, 1, 'a', 1, 1, 'r', 1, 2, 1, 1, 'r', 3, 4}
	want, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDORSetState, CodecID: codec.ID(), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := value.UnmarshalBinary(want); err != nil {
		t.Fatal(err)
	}
	got, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("MarshalBinary canonical frame = %x, want %x", got, want)
	}
}

func defaultLimits() frame.DecoderLimits { return frame.DefaultLimits() }

func mustSnapshot(t testing.TB, state []byte) snapshot.Snapshot {
	t.Helper()
	saved, err := snapshot.New(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}
