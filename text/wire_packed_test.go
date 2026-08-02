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

func TestRGAPackedOuterFrameV2PreservesPayloadAndShrinksInitialState(t *testing.T) {
	source := mustRGA(t, "packed-v2-source")
	delta, err := source.Insert(0, strings.Repeat("协", 4_096))
	if err != nil {
		t.Fatal(err)
	}
	v1, err := delta.MarshalPackedBinary()
	if err != nil {
		t.Fatal(err)
	}
	v2, err := delta.MarshalPackedFrameV2()
	if err != nil {
		t.Fatal(err)
	}
	if len(v2) >= len(v1) {
		t.Fatalf("outer v2 packed delta size = %d, v1 size = %d", len(v2), len(v1))
	}
	v1Frame, err := frame.UnmarshalFrame(v1, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	v2Frame, err := frame.UnmarshalFrame(v2, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if v2Frame.Version() != frame.FormatVersionV2 || !bytes.Equal(v2Frame.Payload, v1Frame.Payload) {
		t.Fatal("outer v2 changed the canonical packed-v3 delta payload")
	}

	tight := frame.DefaultLimits()
	tight.MaxFrameBytes = len(v2)
	if _, err := delta.MarshalPackedFrameV2WithLimits(tight); err != nil {
		t.Fatalf("direct packed outer v2 at final size: %v", err)
	}
	if _, err := delta.MarshalPackedBinaryWithLimits(tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("packed v1 at compressed budget = %v, want %v", err, frame.ErrFrameLimit)
	}
	decoded, err := UnmarshalRGAPackedDeltaWithLimits(v2, tight)
	if err != nil {
		t.Fatal(err)
	}
	target := mustRGA(t, "packed-v2-target")
	if err := target.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
	if got, want := target.String(), source.String(); got != want {
		t.Fatalf("outer v2 target text = %q, want %q", got, want)
	}

	stateV1, err := source.MarshalPackedBinary()
	if err != nil {
		t.Fatal(err)
	}
	stateV2, err := source.MarshalPackedFrameV2()
	if err != nil {
		t.Fatal(err)
	}
	if len(stateV2) >= len(stateV1) {
		t.Fatalf("outer v2 packed state size = %d, v1 state = %d", len(stateV2), len(stateV1))
	}
	stateV1Frame, err := frame.UnmarshalFrame(stateV1, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	stateV2Frame, err := frame.UnmarshalFrame(stateV2, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if stateV2Frame.Version() != frame.FormatVersionV2 || !bytes.Equal(stateV2Frame.Payload, stateV1Frame.Payload) {
		t.Fatal("outer v2 changed the canonical packed-v3 state payload")
	}
	receiver := mustRGA(t, "packed-v2-receiver")
	if err := receiver.UnmarshalPackedBinary(stateV2); err != nil {
		t.Fatal(err)
	}
	if got, want := receiver.String(), source.String(); got != want {
		t.Fatalf("outer v2 recovered text = %q, want %q", got, want)
	}
	saved, err := source.SnapshotPackedFrameV2CurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := recovered.String(), source.String(); got != want {
		t.Fatalf("outer v2 snapshot text = %q, want %q", got, want)
	}
}

func TestRGAPackedOuterFrameV2MutatorsPreflightAndReplicate(t *testing.T) {
	value := mustRGA(t, "packed-v2-local")
	tooSmall := frame.DefaultLimits()
	tooSmall.MaxFrameBytes = 1
	if _, err := value.InsertPackedFrameV2WithLimits(0, "cannot commit", tooSmall); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("preflight packed outer-v2 insert error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if got := value.String(); got != "" {
		t.Fatalf("failed packed outer-v2 preflight changed text to %q", got)
	}

	limits := frame.DefaultLimits()
	insert, err := value.InsertPackedFrameV2WithLimits(0, "draft", limits)
	if err != nil {
		t.Fatal(err)
	}
	deleteFrame, err := value.DeletePackedFrameV2WithLimits(1, 2, limits)
	if err != nil {
		t.Fatal(err)
	}
	replaceFrame, err := value.ReplacePackedFrameV2WithLimits(1, 1, " finalized", limits)
	if err != nil {
		t.Fatal(err)
	}
	target := mustRGA(t, "packed-v2-observer")
	for _, encoded := range [][]byte{insert, deleteFrame, replaceFrame} {
		decoded, err := UnmarshalRGAPackedDeltaWithLimits(encoded, limits)
		if err != nil {
			t.Fatal(err)
		}
		if err := target.ApplyDelta(decoded); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := target.String(), value.String(); got != want {
		t.Fatalf("outer v2 mutations produced %q, want %q", got, want)
	}

	prepared, preparedFrame, err := value.PrepareDeletePackedFrameV2WithLimits(0, 1, limits)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalRGAPackedDeltaWithLimits(preparedFrame, limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
	if err := value.ApplyDelta(prepared); err != nil {
		t.Fatal(err)
	}
	if got, want := target.String(), value.String(); got != want {
		t.Fatalf("prepared outer v2 target = %q, want %q", got, want)
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

func TestRGAPackedLocalOperationsPreflightAndSnapshotBounds(t *testing.T) {
	limits := frame.DefaultLimits()
	source := mustRGA(t, "packed-local")

	inserted, err := source.InsertPackedBinaryWithLimits(0, "abc", limits)
	if err != nil {
		t.Fatal(err)
	}
	decodedInsert, err := UnmarshalRGAPackedDeltaWithLimits(inserted, limits)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := source.String(), "abc"; got != want {
		t.Fatalf("packed insertion text = %q, want %q", got, want)
	}
	observer := mustRGA(t, "packed-local-observer")
	if err := observer.ApplyDelta(decodedInsert); err != nil {
		t.Fatal(err)
	}
	if got, want := observer.String(), source.String(); got != want {
		t.Fatalf("packed insertion delivery = %q, want %q", got, want)
	}

	prepared, preparedFrame, err := source.PrepareInsertPackedBinaryWithLimits(3, "d", limits)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := source.String(), "abc"; got != want {
		t.Fatalf("prepared packed insertion changed source to %q, want %q", got, want)
	}
	decodedPrepared, err := UnmarshalRGAPackedDeltaWithLimits(preparedFrame, limits)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := decodedPrepared.NodePositions(), prepared.NodePositions(); !equalPositions(got, want) {
		t.Fatal("prepared packed insertion frame changed positions")
	}
	if err := source.ApplyDelta(prepared); err != nil {
		t.Fatal(err)
	}
	if got, want := source.String(), "abcd"; got != want {
		t.Fatalf("applied prepared insertion = %q, want %q", got, want)
	}

	deleted, err := source.DeletePackedBinaryWithLimits(1, 2, limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalRGAPackedDeltaWithLimits(deleted, limits); err != nil {
		t.Fatal(err)
	}
	if got, want := source.String(), "ad"; got != want {
		t.Fatalf("packed deletion text = %q, want %q", got, want)
	}
	preparedDelete, preparedDeleteFrame, err := source.PrepareDeletePackedBinaryWithLimits(1, 1, limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalRGAPackedDeltaWithLimits(preparedDeleteFrame, limits); err != nil {
		t.Fatal(err)
	}
	if got, want := source.String(), "ad"; got != want {
		t.Fatalf("prepared packed deletion changed source to %q, want %q", got, want)
	}
	if err := source.ApplyDelta(preparedDelete); err != nil {
		t.Fatal(err)
	}
	if got, want := source.String(), "a"; got != want {
		t.Fatalf("applied prepared deletion = %q, want %q", got, want)
	}

	saved, err := source.SnapshotPackedCurrentStateWithLimits(limits)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewFromSnapshotWithOptions(saved, DefaultOptions(), limits)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := recovered.String(), source.String(); got != want {
		t.Fatalf("bounded packed snapshot = %q, want %q", got, want)
	}

	tight := limits
	tight.MaxFrameBytes = 8
	tight.MaxPayload = 1
	before := source.String()
	if _, err := source.InsertPackedBinaryWithLimits(1, "reject", tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tight packed insertion error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if got := source.String(); got != before {
		t.Fatalf("rejected packed insertion changed source to %q, want %q", got, before)
	}
	if _, err := source.MarshalPackedBinaryWithLimits(tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tight packed state error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if _, err := source.SnapshotPackedCurrentStateWithLimits(tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tight packed snapshot error = %v, want %v", err, frame.ErrFrameLimit)
	}

	var nilRGA *RGA
	if _, err := nilRGA.MarshalPackedBinaryWithLimits(limits); !errors.Is(err, ErrNilText) {
		t.Fatalf("nil packed marshal error = %v, want %v", err, ErrNilText)
	}
	if err := nilRGA.UnmarshalPackedBinaryWithLimits(inserted, limits); !errors.Is(err, ErrNilText) {
		t.Fatalf("nil packed unmarshal error = %v, want %v", err, ErrNilText)
	}
}

func TestRGAPackedWireFailureAndLimitPaths(t *testing.T) {
	var nilRGA *RGA
	if _, err := nilRGA.MarshalPackedBinary(); !errors.Is(err, ErrNilText) {
		t.Fatalf("nil MarshalPackedBinary = %v", err)
	}
	if err := nilRGA.UnmarshalPackedBinary(nil); !errors.Is(err, ErrNilText) {
		t.Fatalf("nil UnmarshalPackedBinary = %v", err)
	}
	if _, err := nilRGA.SnapshotPackedCurrentState(); !errors.Is(err, ErrNilText) {
		t.Fatalf("nil SnapshotPackedCurrentState = %v", err)
	}

	first := Position{ReplicaID: "source", WallTime: 1}
	second := Position{ReplicaID: "source", WallTime: 2}
	nodes := map[Position]node{
		first:  {rune: 'a'},
		second: {parent: first, rune: 'b'},
	}
	limits := frame.DefaultLimits()
	if _, err := marshalRGAPacked(crdt.TypeIDRGAPackedState, map[Position]node{second: nodes[second]}, nil, limits); !errors.Is(err, ErrIncompleteState) {
		t.Fatalf("incomplete packed state = %v", err)
	}
	if _, err := marshalRGAPacked(crdt.TypeIDRGAPackedDelta, map[Position]node{{}: {rune: 'x'}}, nil, limits); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("invalid packed delta = %v", err)
	}
	limitedNodes := limits
	limitedNodes.MaxElements = 1
	if _, err := marshalRGAPacked(crdt.TypeIDRGAPackedState, nodes, nil, limitedNodes); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("node-limited packed state = %v", err)
	}
	if _, err := marshalRGAPackedState(rgaRunState{nodes: []runNode{{id: first, item: nodes[first]}, {id: second, item: nodes[second]}}}, limitedNodes); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("node-limited captured packed state = %v", err)
	}
	if _, err := marshalRGAPackedState(rgaRunState{nodes: []runNode{{item: node{rune: 'x'}}}}, limits); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("invalid captured packed state = %v", err)
	}
	limitedString := limits
	limitedString.MaxStringBytes = 1
	if _, err := packedRunPayloadSize([]packedRunBlock{{nodes: []runNode{{id: first, item: nodes[first]}}}}, nil, limitedString); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("string-limited packed payload = %v", err)
	}
	limitedPayload := limits
	limitedPayload.MaxPayload = 1
	if _, err := packedRunPayloadSize([]packedRunBlock{{nodes: []runNode{{id: first, item: nodes[first]}}}}, nil, limitedPayload); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("payload-limited packed payload = %v", err)
	}
	if size, err := packedRunPayloadSize([]packedRunBlock{{nodes: []runNode{{id: first, item: nodes[first]}, {id: second, item: nodes[second]}}}}, []Position{first}, limits); err != nil || size == 0 {
		t.Fatalf("packed chain payload = %d, %v", size, err)
	}
	longParent := Position{ReplicaID: "long", WallTime: 1}
	if _, err := packedRunPayloadSize([]packedRunBlock{{nodes: []runNode{{id: first, item: node{parent: longParent, rune: 'a'}}}}}, nil, limitedString); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("parent string-limited packed payload = %v", err)
	}
	if _, err := packedRunBlockSize(packedRunBlock{nodes: []runNode{{id: first, item: node{rune: -1}}}}, limits); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("invalid packed scalar = %v", err)
	}
	if gap, ok := packedWallGap([]runNode{{id: first, item: nodes[first]}, {id: second, item: nodes[second]}}, 1); !ok || gap != 1 {
		t.Fatalf("packed wall gap = %d, %t", gap, ok)
	}
	if _, ok := packedWallGap([]runNode{{id: first, item: nodes[first]}, {id: second, item: nodes[second]}}, 0); ok {
		t.Fatal("first packed wall gap accepted")
	}
	if _, ok := packedWallGap([]runNode{{id: second, item: nodes[second]}, {id: first, item: nodes[first]}}, 1); ok {
		t.Fatal("non-increasing packed wall gap accepted")
	}
	if _, ok := packedTransitionLength(1); ok {
		t.Fatal("short packed transition length accepted")
	}
	if got, ok := packedTransitionLength(9); !ok || got != 1 {
		t.Fatalf("packed transition length = %d, %v", got, ok)
	}
	if unusedPackedTransitionBitsSet([]byte{0}, 2) || !unusedPackedTransitionBitsSet([]byte{0x80}, 2) {
		t.Fatal("packed unused transition-bit validation mismatch")
	}
	if _, ok := makePackedRunChain([]runNode{{id: first, item: nodes[first]}}); ok {
		t.Fatal("single packed chain node accepted")
	}
	if _, ok := makePackedRunChain([]runNode{{id: first, item: node{rune: -1}}, {id: second, item: nodes[second]}}); ok {
		t.Fatal("invalid packed chain scalar accepted")
	}
	if _, err := marshalPackedRunPayload([]packedRunBlock{{nodes: []runNode{{id: first, item: node{rune: -1}}}}}, nil, limits); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("invalid packed payload scalar = %v", err)
	}
	if err := writePackedRunPayload(make([]byte, 1), []packedRunBlock{{nodes: []runNode{{id: first, item: nodes[first]}}}}, nil); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("short packed payload buffer = %v", err)
	}
	if _, err := packedRunPayloadSize(nil, []Position{longParent}, limitedString); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tombstone string-limited packed payload = %v", err)
	}

	encoded, err := marshalRGAPacked(crdt.TypeIDRGAPackedState, nodes, map[Position]struct{}{first: {}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	decodedNodes, decodedTombstones, err := unmarshalRGAPacked(encoded, crdt.TypeIDRGAPackedState, limits, true, nil)
	if err != nil || len(decodedNodes) != len(nodes) || len(decodedTombstones) != 1 {
		t.Fatalf("packed state decode = nodes=%d tombstones=%d err=%v", len(decodedNodes), len(decodedTombstones), err)
	}
	if _, err := UnmarshalRGAPackedDelta(encoded); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("packed wrong type = %v", err)
	}
	decodeLimited := limits
	decodeLimited.MaxElements = 1
	if _, _, err := unmarshalRGAPacked(encoded, crdt.TypeIDRGAPackedState, decodeLimited, true, nil); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("packed decode limit = %v", err)
	}
	pending := mustRGA(t, "packed-pending")
	pending.pending[Position{ReplicaID: "missing", WallTime: 1}] = node{parent: Position{ReplicaID: "parent", WallTime: 1}, rune: 'x'}
	if _, err := pending.MarshalPackedBinaryWithLimits(limits); !errors.Is(err, ErrIncompleteState) {
		t.Fatalf("pending packed state = %v", err)
	}
	if _, err := pending.SnapshotPackedCurrentStateWithLimits(limits); !errors.Is(err, ErrIncompleteState) {
		t.Fatalf("pending packed snapshot = %v", err)
	}

	for name, payload := range map[string][]byte{
		"bad-block-kind":   frame.AppendUvarint(frame.AppendUvarint(nil, 1), 3),
		"truncated-node":   frame.AppendUvarint(frame.AppendUvarint(nil, 1), runBlockNode),
		"short-chain":      frame.AppendUvarint(frame.AppendUvarint(frame.AppendUvarint(nil, 1), runBlockChain), 1),
		"trailing-payload": append(frame.AppendUvarint(nil, 0), 0, 0),
	} {
		t.Run(name, func(t *testing.T) {
			data, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRGAPackedDelta, Payload: payload})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := unmarshalRGAPacked(data, crdt.TypeIDRGAPackedDelta, limits, false, nil); !errors.Is(err, frame.ErrInvalidFrame) {
				t.Fatalf("unmarshal invalid packed = %v", err)
			}
		})
	}
	chainParentFlag := frame.AppendUvarint(nil, 1)
	chainParentFlag = frame.AppendUvarint(chainParentFlag, packedRunBlockChain)
	chainParentFlag = frame.AppendUvarint(chainParentFlag, 2)
	chainParentFlag = frame.AppendUvarint(chainParentFlag, uint64(len(first.ReplicaID)))
	chainParentFlag = append(chainParentFlag, first.ReplicaID...)
	chainParentFlag = frame.AppendUvarint(chainParentFlag, 2)
	data, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRGAPackedDelta, Payload: chainParentFlag})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := unmarshalRGAPacked(data, crdt.TypeIDRGAPackedDelta, limits, false, nil); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("invalid packed chain parent flag = %v", err)
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
