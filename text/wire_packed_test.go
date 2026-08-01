package text

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/replica"
)

func TestRGAPackedFramesPreservePositionsAndCloseInitialSyncByteGap(t *testing.T) {
	const runes = 4_096
	source := mustRGA(t, "packed-source")
	delta, err := source.Insert(0, strings.Repeat("x", runes))
	if err != nil {
		t.Fatal(err)
	}
	runV2, err := delta.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	packed, err := delta.MarshalPackedBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(packed) >= len(runV2)/2 {
		t.Fatalf("packed delta size = %d, run-v2 = %d; expected dense HLC packing", len(packed), len(runV2))
	}
	decodedFrame, err := frame.UnmarshalFrame(packed, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if decodedFrame.TypeID != crdt.TypeIDRGAPackedDelta {
		t.Fatalf("packed delta type = %d, want %d", decodedFrame.TypeID, crdt.TypeIDRGAPackedDelta)
	}
	decoded, err := UnmarshalRGAPackedDelta(packed)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := decoded.NodePositions(), delta.NodePositions(); !equalPositions(got, want) {
		t.Fatalf("packed positions changed: got %d positions, want %d", len(got), len(want))
	}
	target := mustRGA(t, "packed-target")
	if err := target.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
	if got, want := target.String(), source.String(); got != want {
		t.Fatalf("packed delta text = %q, want %q", got, want)
	}

	state, err := source.MarshalPackedBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(state) >= len(runV2)/2 {
		t.Fatalf("packed state size = %d, run-v2 delta = %d; expected dense HLC packing", len(state), len(runV2))
	}
	recovered := mustRGA(t, "packed-recovered")
	if err := recovered.UnmarshalPackedBinary(state); err != nil {
		t.Fatal(err)
	}
	if got, want := recovered.String(), source.String(); got != want {
		t.Fatalf("packed state text = %q, want %q", got, want)
	}

	saved, err := source.SnapshotPackedCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := saved.TypeID, crdt.TypeIDRGAPackedState; got != want {
		t.Fatalf("packed snapshot type = %d, want %d", got, want)
	}
	fromSnapshot, err := NewFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fromSnapshot.String(), source.String(); got != want {
		t.Fatalf("packed snapshot text = %q, want %q", got, want)
	}
}

func TestRGAPackedFramesFallbackForNonDenseChainAndRejectNonCanonicalBitmap(t *testing.T) {
	first := Position{ReplicaID: "writer", WallTime: 10, Logical: 4}
	second := Position{ReplicaID: "writer", WallTime: 12, Logical: 2}
	delta := Delta{nodes: map[Position]node{
		first:  {rune: 'a'},
		second: {parent: first, rune: 'b'},
	}, tombstones: make(map[Position]struct{})}
	encoded, err := delta.MarshalPackedBinary()
	if err != nil {
		t.Fatal(err)
	}
	decodedFrame, err := frame.UnmarshalFrame(encoded, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if kind, _, ok := packedFirstBlock(decodedFrame.Payload); !ok || kind != runBlockChain {
		t.Fatalf("non-dense chain block = %d, ok=%v; want ordinary chain", kind, ok)
	}
	decoded, err := UnmarshalRGAPackedDelta(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := decoded.NodePositions(), delta.NodePositions(); !equalPositions(got, want) {
		t.Fatalf("ordinary fallback changed positions")
	}

	source := mustRGA(t, "source")
	if _, err := source.Insert(0, strings.Repeat("x", 4_096)); err != nil {
		t.Fatal(err)
	}
	state, err := source.MarshalPackedBinary()
	if err != nil {
		t.Fatal(err)
	}
	frameValue, err := frame.UnmarshalFrame(state, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	payload := append([]byte(nil), frameValue.Payload...)
	transitionOffset, ok := packedTransitionOffset(payload)
	if !ok {
		t.Fatal("packed state did not contain a dense chain")
	}
	payload[transitionOffset] |= 0x80 // The last transition byte has one unused high bit for 4,096 nodes.
	malformed, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRGAPackedState, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	receiver := mustRGA(t, "receiver")
	if _, err := receiver.Insert(0, "retained"); err != nil {
		t.Fatal(err)
	}
	if err := receiver.UnmarshalPackedBinary(malformed); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("non-canonical bitmap error = %v, want %v", err, frame.ErrInvalidFrame)
	}
	if got := receiver.String(); got != "retained" {
		t.Fatalf("rejected packed state changed receiver to %q", got)
	}
}

func TestRGAPackedCanonicalChoiceUsesConfiguredLimits(t *testing.T) {
	// The packed-versus-regular choice is part of the canonical payload, so it
	// must use the caller's policy rather than frame.DefaultLimits().
	limits := frame.DefaultLimits()
	replicaID := strings.Repeat("r", limits.MaxStringBytes+1)
	limits.MaxStringBytes = len(replicaID)
	limits.MaxFrameBytes = len(replicaID) + 4_096
	limits.MaxPayload = len(replicaID) + 2_048

	source := mustRGA(t, replicaID)
	delta, err := source.Insert(0, "xy")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalPackedBinaryWithLimits(limits)
	if err != nil {
		t.Fatal(err)
	}
	decodedFrame, err := frame.UnmarshalFrame(encoded, limits)
	if err != nil {
		t.Fatal(err)
	}
	if kind, _, ok := packedFirstBlock(decodedFrame.Payload); !ok || kind != packedRunBlockChain {
		t.Fatalf("configured-limit chain block = %d, ok=%v; want packed chain", kind, ok)
	}
	decoded, err := UnmarshalRGAPackedDeltaWithLimits(encoded, limits)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := decoded.NodePositions(), delta.NodePositions(); !equalPositions(got, want) {
		t.Fatalf("configured-limit packed positions changed")
	}
}

func TestRGAPackedManifestDeliversDuplicateReorderedEditsAndSnapshotDelta(t *testing.T) {
	manifest, err := replica.NewManifest("document", "example.com/text/packed-v3", 1, replica.Protocol{
		StateID: crdt.TypeIDRGAPackedState, DeltaID: crdt.TypeIDRGAPackedDelta, SemanticsVersion: PackedV3SemanticsVersion,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	left, right, observer := mustRGA(t, "left"), mustRGA(t, "right"), mustRGA(t, "observer")
	base := mustInsertRGA(t, left, 0, "shared")
	baseFrame, err := base.MarshalPackedBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := applyPackedFrame(right, baseFrame); err != nil {
		t.Fatal(err)
	}
	if err := applyPackedFrame(observer, baseFrame); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := left.SnapshotPackedCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	leftFrame, err := left.ReplacePackedBinaryWithLimits(3, 1, "L", frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	rightFrame, err := right.ReplacePackedBinaryWithLimits(3, 1, "R", frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range []struct {
		target *RGA
		frame  []byte
	}{
		{left, rightFrame}, {left, rightFrame}, {right, leftFrame}, {right, leftFrame},
		{observer, rightFrame}, {observer, rightFrame}, {observer, leftFrame}, {observer, leftFrame},
	} {
		if err := applyPackedFrame(delivery.target, delivery.frame); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := right.String(), left.String(); got != want {
		t.Fatalf("packed replicas diverged: %q != %q", got, want)
	}
	if got, want := observer.String(), left.String(); got != want || observer.PendingCount() != 0 {
		t.Fatalf("packed observer = %q pending=%d, want %q", got, observer.PendingCount(), want)
	}

	antiEntropy, err := left.MarshalDeltaSince(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := frame.UnmarshalFrame(antiEntropy, frame.DefaultLimits()); err != nil || decoded.TypeID != crdt.TypeIDRGAPackedDelta {
		t.Fatalf("packed snapshot delta = %#v, %v", decoded, err)
	}
	change, err := replica.NewChange(manifest, replica.Dot{Actor: "left", Counter: 1}, antiEntropy)
	if err != nil {
		t.Fatal(err)
	}
	frontier, err := replica.NewFrontier(nil)
	if err != nil {
		t.Fatal(err)
	}
	inboxTarget := mustRGA(t, "inbox-target")
	inbox, err := replica.NewInbox(manifest, frontier, 8, frame.DefaultLimits().MaxFrameBytes, func(encoded []byte) error {
		return applyPackedFrame(inboxTarget, encoded)
	})
	if err != nil {
		t.Fatal(err)
	}
	if delivery, err := inbox.Receive(change); err != nil || len(delivery.Applied) != 1 {
		t.Fatalf("packed inbox receive = %#v, %v", delivery, err)
	}
	if delivery, err := inbox.Receive(change); err != nil || !delivery.Duplicate {
		t.Fatalf("packed inbox duplicate = %#v, %v", delivery, err)
	}
}

func FuzzRGAPackedUnmarshal(f *testing.F) {
	value, err := New("packed-seed")
	if err != nil {
		f.Fatal(err)
	}
	if _, err := value.Insert(0, "seed packed RGA"); err != nil {
		f.Fatal(err)
	}
	state, err := value.MarshalPackedBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(state)
	f.Fuzz(func(t *testing.T, data []byte) {
		target := mustRGA(t, "target")
		if err := target.UnmarshalPackedBinary(data); err == nil && target.State().ElementCount < 0 {
			t.Fatal("negative visible count")
		}
		if delta, err := UnmarshalRGAPackedDelta(data); err == nil {
			if err := target.ApplyDelta(delta); err != nil {
				t.Fatalf("decoded packed delta rejected: %v", err)
			}
		}
	})
}

func applyPackedFrame(target *RGA, encoded []byte) error {
	delta, err := UnmarshalRGAPackedDelta(encoded)
	if err != nil {
		return err
	}
	return target.ApplyDelta(delta)
}

func equalPositions(left, right []Position) bool {
	return bytes.Equal(positionsBytes(left), positionsBytes(right))
}

func positionsBytes(positions []Position) []byte {
	result := make([]byte, 0, len(positions)*16)
	for _, position := range positions {
		result = append(result, position.ReplicaID...)
		result = frame.AppendUvarint(result, position.WallTime)
		result = frame.AppendUvarint(result, position.Logical)
	}
	return result
}

func packedFirstBlock(payload []byte) (uint64, int, bool) {
	_, position, ok := frame.ReadUvarint(payload, 0)
	if !ok {
		return 0, 0, false
	}
	kind, position, ok := frame.ReadUvarint(payload, position)
	return kind, position, ok
}

func packedTransitionOffset(payload []byte) (int, bool) {
	kind, position, ok := packedFirstBlock(payload)
	if !ok || kind != packedRunBlockChain {
		return 0, false
	}
	_, position, ok = frame.ReadUvarint(payload, position)
	if !ok {
		return 0, false
	}
	_, position, ok = frame.ReadBytes(payload, position, frame.DefaultLimits().MaxStringBytes)
	if !ok {
		return 0, false
	}
	parentFlag, position, ok := frame.ReadUvarint(payload, position)
	if !ok || parentFlag > 1 {
		return 0, false
	}
	if parentFlag == 1 {
		_, position, ok = frame.ReadTag(payload, position, frame.DefaultLimits().MaxStringBytes)
		if !ok {
			return 0, false
		}
	}
	_, position, ok = frame.ReadUvarint(payload, position)
	if !ok {
		return 0, false
	}
	_, position, ok = frame.ReadUvarint(payload, position)
	if !ok {
		return 0, false
	}
	length, position, ok := frame.ReadUvarint(payload, position)
	if !ok || length == 0 || int(length) > len(payload)-position {
		return 0, false
	}
	return position + int(length) - 1, true
}
