package text

import (
	"errors"
	"reflect"
	"testing"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
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

func TestRGAExplicitWireAndRecoveryLimits(t *testing.T) {
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := source.Insert(0, "ab")
	if err != nil {
		t.Fatal(err)
	}
	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}

	if encoded, err := source.MarshalBinaryWithLimits(frame.DefaultLimits()); err != nil || string(encoded) != string(state) {
		t.Fatalf("MarshalBinaryWithLimits() = %x, %v; want canonical state %x", encoded, err, state)
	}
	if encoded, err := delta.MarshalBinaryWithLimits(frame.DefaultLimits()); err != nil {
		t.Fatalf("Delta.MarshalBinaryWithLimits() error = %v", err)
	} else if _, err := UnmarshalRGADelta(encoded); err != nil {
		t.Fatalf("bounded delta was not decodable: %v", err)
	}
	if encoded, clockState, err := source.MarshalBinaryWithClockStateAndLimits(frame.DefaultLimits()); err != nil || string(encoded) != string(state) || clockState != source.ClockState() {
		t.Fatalf("MarshalBinaryWithClockStateAndLimits() = %x, %#v, %v", encoded, clockState, err)
	}

	tight := frame.DefaultLimits()
	tight.MaxPayload = 1
	if _, err := source.MarshalBinaryWithLimits(tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("bounded state encoding error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if _, err := delta.MarshalBinaryWithLimits(tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("bounded delta encoding error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if _, err := source.SnapshotCurrentStateWithLimits(tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("bounded snapshot encoding error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if _, err := source.MarshalRunBinaryWithLimits(tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("bounded run state encoding error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if _, err := delta.MarshalRunBinaryWithLimits(tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("bounded run delta encoding error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if _, err := source.SnapshotRunCurrentStateWithLimits(tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("bounded run snapshot encoding error = %v, want %v", err, frame.ErrFrameLimit)
	}

	saved, err := source.SnapshotCurrentStateWithLimits(frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	decodeTight := frame.DefaultLimits()
	decodeTight.MaxElements = 1
	if _, err := NewFromSnapshotWithOptions(saved, DefaultOptions(), decodeTight); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("bounded snapshot decode error = %v, want %v", err, frame.ErrInvalidFrame)
	}
	receiveTight := DefaultOptions()
	receiveTight.MaxNodes = 1
	if _, err := NewFromSnapshotWithOptions(saved, receiveTight, frame.DefaultLimits()); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("bounded snapshot retention error = %v, want %v", err, ErrResourceLimit)
	}

	restored, err := NewFromSnapshotWithOptions(saved, DefaultOptions(), frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.String(); got != source.String() {
		t.Fatalf("bounded snapshot recovery text = %q, want %q", got, source.String())
	}

	runSaved, err := source.SnapshotRunCurrentStateWithLimits(frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewFromSnapshotWithOptions(runSaved, DefaultOptions(), decodeTight); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("bounded run snapshot decode error = %v, want %v", err, frame.ErrInvalidFrame)
	}
	runRestored, err := NewFromSnapshotWithOptions(runSaved, DefaultOptions(), frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got := runRestored.String(); got != source.String() {
		t.Fatalf("bounded run snapshot recovery text = %q, want %q", got, source.String())
	}
}

func TestRGAMutationWithLimitsRejectsBeforeDocumentMutation(t *testing.T) {
	value, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	tight := frame.DefaultLimits()
	tight.MaxPayload = 1
	if _, err := value.InsertWithLimits(0, "a", tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("InsertWithLimits() error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if got := value.String(); got != "" || value.State().TombstoneCount != 0 {
		t.Fatalf("rejected insert mutated document: text=%q state=%#v", got, value.State())
	}
	if _, err := value.Insert(0, "a"); err != nil {
		t.Fatal(err)
	}
	before := value.String()
	beforeTombstones := value.State().TombstoneCount
	if _, err := value.DeleteWithLimits(0, 1, tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("DeleteWithLimits() error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if got := value.String(); got != before || value.State().TombstoneCount != beforeTombstones {
		t.Fatalf("rejected delete mutated document: text=%q state=%#v", got, value.State())
	}
}

func TestRGASnapshotRecoveryWitnessesSuppliedFrontier(t *testing.T) {
	value, err := New("local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Insert(0, "saved"); err != nil {
		t.Fatal(err)
	}
	future := crdt.Tag{ReplicaID: "remote", WallTime: 10_000_000_000_000, Logical: 3}
	v1, err := value.Snapshot(map[string]crdt.Tag{"remote": future})
	if err != nil {
		t.Fatal(err)
	}
	runState, err := value.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	run, err := snapshot.NewValidatedWithClockState(
		runState,
		map[string]crdt.Tag{"remote": future},
		value.ClockState(),
		validateRGARunState,
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, saved := range map[string]snapshot.Snapshot{"v1": v1, "run-v2": run} {
		t.Run(name, func(t *testing.T) {
			restored, err := NewFromSnapshot(saved)
			if err != nil {
				t.Fatal(err)
			}
			afterRecovery, err := restored.Insert(restored.State().ElementCount, "!")
			if err != nil {
				t.Fatal(err)
			}
			if got := parentNodeID(afterRecovery); got.Compare(future) <= 0 {
				t.Fatalf("post-recovery position = %#v, want greater than supplied frontier %#v", got, future)
			}
		})
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
