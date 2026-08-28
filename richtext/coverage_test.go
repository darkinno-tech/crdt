package richtext

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/clock"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/snapshot"
	"github.com/darkinno-tech/crdt/text"
)

func TestConstructionDiagnosticsAndNilPaths(t *testing.T) {
	invalid := DefaultOptions()
	invalid.MaxMarkEntries = 0
	if _, err := NewWithOptions("author", invalid); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid options = %v", err)
	}
	if _, err := NewFromClockWithOptions(clock.State{ReplicaID: "author"}, invalid); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid clock options = %v", err)
	}
	document, err := NewFromClock(clock.State{ReplicaID: "author"})
	if err != nil {
		t.Fatal(err)
	}
	if document.ClockState().ReplicaID != "author" || document.Len() != 0 {
		t.Fatalf("restored document state = %#v len=%d", document.ClockState(), document.Len())
	}
	if document.Spans() != nil {
		t.Fatal("empty document spans were not nil")
	}
	if _, err := json.Marshal(document); err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(Delta{}); err != nil {
		t.Fatal(err)
	}
	var nilDocument *Document
	if nilDocument.String() != "" || nilDocument.Len() != 0 || nilDocument.State().Type != "rich-text" {
		t.Fatal("nil diagnostic methods returned unexpected values")
	}
	if _, ok := nilDocument.AttributesAt(0); ok {
		t.Fatal("nil AttributesAt succeeded")
	}
	if nilDocument.Spans() != nil {
		t.Fatal("nil Spans was not nil")
	}
	if _, err := nilDocument.Insert(0, "x"); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil Insert = %v", err)
	}
	if _, err := nilDocument.Delete(0, 0); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil Delete = %v", err)
	}
	if err := nilDocument.ApplyDelta(Delta{}); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil ApplyDelta = %v", err)
	}
	if _, err := nilDocument.MarshalBinary(); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil MarshalBinary = %v", err)
	}
}

func TestWireStateLimitsAndRecoveryRejectionPaths(t *testing.T) {
	source := mustDocument(t, "source")
	if _, err := source.InsertWithAttributes(0, "state", Attributes{"bold": "true"}); err != nil {
		t.Fatal(err)
	}
	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	target := mustDocument(t, "target")
	if err := target.UnmarshalBinary(state); err != nil {
		t.Fatal(err)
	}
	if got, want := target.Spans(), source.Spans(); !spansEqual(got, want) {
		t.Fatalf("state spans = %#v, want %#v", got, want)
	}
	tight := frame.DefaultLimits()
	tight.MaxPayload = 1
	if _, err := source.MarshalBinaryWithLimits(tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tight state marshal = %v", err)
	}
	if err := target.UnmarshalBinaryWithLimits(state, tight); !errors.Is(err, ErrInvalidDelta) && !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tight state decode = %v", err)
	}
	wrong, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRichTextDelta, Payload: []byte{0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.UnmarshalBinary(wrong); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("wrong state type = %v", err)
	}
	if _, err := NewFromSnapshot(snapshot.Snapshot{TypeID: crdt.TypeIDRichTextState}); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("invalid snapshot = %v", err)
	}
	if _, err := NewFromSnapshot(snapshot.Snapshot{TypeID: crdt.TypeIDGCounterState}); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("wrong snapshot type = %v", err)
	}
}

func TestInternalValidationAndConflictPaths(t *testing.T) {
	document := mustDocument(t, "author")
	position := text.Position{ReplicaID: "position", WallTime: 1}
	tag := crdt.Tag{ReplicaID: "format", WallTime: 1}
	valid := Delta{operations: []formatOperation{{
		tag: tag, targets: []text.Position{position}, changes: []AttributeChange{{Key: "bold", Value: "true"}},
	}}}
	if err := document.ApplyDelta(valid); err != nil {
		t.Fatal(err)
	}
	conflicting := Delta{operations: []formatOperation{{
		tag: tag, targets: []text.Position{position}, changes: []AttributeChange{{Key: "bold", Value: "false"}},
	}}}
	if err := document.ApplyDelta(conflicting); !errors.Is(err, ErrTagConflict) {
		t.Fatalf("equal-tag conflict = %v", err)
	}
	if err := validateOperations([]formatOperation{{}}); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("invalid operation = %v", err)
	}
	if err := validateDelta(Delta{textDelta: []byte("invalid")}, frame.DefaultLimits()); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("invalid nested delta = %v", err)
	}
	textState, err := document.text.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := marshalState(richState{textState: textState, marks: map[text.Position]markSet{
		position: {key: "bad", value: markValue{}},
	}}, frame.DefaultLimits()); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("invalid state metadata = %v", err)
	}
	if err := document.preflightMarksLocked(map[text.Position]markSet{
		position: {key: "bad", value: markValue{tag: crdt.Tag{}}},
	}); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("invalid mark preflight = %v", err)
	}
	if _, err := appendString(nil, string(make([]byte, frame.DefaultLimits().MaxStringBytes+1)), frame.DefaultLimits()); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("oversized string = %v", err)
	}
	if _, err := appendBytes(nil, make([]byte, frame.DefaultLimits().MaxPayload+1), frame.DefaultLimits()); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("oversized bytes = %v", err)
	}
}

func TestDeleteAndOuterWireRoundTrip(t *testing.T) {
	source := mustDocument(t, "source")
	insert, err := source.Insert(0, "abc")
	if err != nil {
		t.Fatal(err)
	}
	deleteDelta, err := source.Delete(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := source.String(); got != "ac" {
		t.Fatalf("Delete String() = %q", got)
	}
	target := mustDocument(t, "target")
	for _, delta := range []Delta{deleteDelta, insert, deleteDelta} {
		encoded, err := delta.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := UnmarshalDeltaWithLimits(encoded, frame.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		if err := target.ApplyDelta(decoded); err != nil {
			t.Fatal(err)
		}
	}
	if got := target.String(); got != "ac" {
		t.Fatalf("out-of-order delete String() = %q", got)
	}
}

func TestWireRejectsMalformedDeltaAndStatePayloads(t *testing.T) {
	validTag := crdt.Tag{ReplicaID: "writer", WallTime: 1}
	validPosition := text.Position{ReplicaID: "position", WallTime: 1}
	validChange := []AttributeChange{{Key: "bold", Value: "true"}}
	validOperation := formatOperation{tag: validTag, targets: []text.Position{validPosition}, changes: validChange}
	validDelta, err := marshalDelta(Delta{operations: []formatOperation{validOperation}}, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := frame.UnmarshalFrame(validDelta, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	badPayloads := [][]byte{
		nil,
		frame.AppendUvarint(nil, 0),
		append(frame.AppendUvarint(nil, 0), 1),
		func() []byte {
			payload := frame.AppendUvarint(nil, 0)
			payload = frame.AppendUvarint(payload, 1)
			payload = frame.AppendTag(payload, validTag)
			return frame.AppendUvarint(payload, 0)
		}(),
		func() []byte {
			payload := frame.AppendUvarint(nil, 0)
			payload = frame.AppendUvarint(payload, 1)
			payload = frame.AppendTag(payload, validTag)
			payload = frame.AppendUvarint(payload, 1)
			payload = frame.AppendTag(payload, validPosition)
			return frame.AppendUvarint(payload, 0)
		}(),
	}
	for index, payload := range badPayloads {
		data, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRichTextDelta, Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := UnmarshalDelta(data); !errors.Is(err, ErrInvalidDelta) {
			t.Fatalf("bad delta payload %d error = %v", index, err)
		}
	}
	wrongCodec, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRichTextDelta, CodecID: "x", Payload: decoded.Payload})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalDeltaWithLimits(wrongCodec, frame.DefaultLimits()); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("wrong delta codec = %v", err)
	}
	if _, _, ok := readTargets(nil, 0, frame.DefaultLimits(), new(int)); ok {
		t.Fatal("empty targets accepted")
	}
	if _, _, ok := readChanges(nil, 0, frame.DefaultLimits()); ok {
		t.Fatal("empty changes accepted")
	}

	document := mustDocument(t, "source")
	textState, err := document.text.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	base := frame.AppendUvarint(nil, uint64(len(textState)))
	base = append(base, textState...)
	statePayloads := [][]byte{
		nil,
		base,
		append(append([]byte(nil), base...), 1),
		func() []byte {
			payload := append([]byte(nil), base...)
			payload = frame.AppendUvarint(payload, 1)
			payload = frame.AppendTag(payload, validPosition)
			payload = frame.AppendUvarint(payload, 0)
			return payload
		}(),
	}
	for index, payload := range statePayloads {
		data, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRichTextState, Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := unmarshalState(data, frame.DefaultLimits()); !errors.Is(err, ErrInvalidDelta) {
			t.Fatalf("bad state payload %d error = %v", index, err)
		}
	}
}

func TestAdditionalFailureAndMergePaths(t *testing.T) {
	left, right := mustDocument(t, "left"), mustDocument(t, "right")
	if err := left.Merge(nil); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("Merge(nil) = %v", err)
	}
	if err := left.Merge(left); err != nil {
		t.Fatalf("Merge(self) = %v", err)
	}
	if _, err := left.DeleteWithLimits(-1, 1, frame.DefaultLimits()); !errors.Is(err, text.ErrRange) {
		t.Fatalf("invalid delete = %v", err)
	}
	if _, err := left.InsertWithAttributesWithLimits(0, "x", Attributes{"\xff": "x"}, frame.DefaultLimits()); !errors.Is(err, ErrInvalidAttribute) {
		t.Fatalf("invalid attributes = %v", err)
	}
	if _, err := left.InsertWithAttributesWithLimits(0, "x", nil, frame.DecoderLimits{}); err == nil {
		t.Fatal("invalid limits accepted")
	}
	if _, err := left.FormatWithLimits(-1, 1, []AttributeChange{{Key: "x", Value: "y"}}, frame.DefaultLimits()); !errors.Is(err, text.ErrRange) {
		t.Fatalf("invalid format range = %v", err)
	}
	if _, err := right.Insert(0, "right"); err != nil {
		t.Fatal(err)
	}
	if err := left.Merge(right); err != nil {
		t.Fatal(err)
	}
	if left.State().ElementCount != len([]rune(left.String())) {
		t.Fatalf("diagnostic state = %#v", left.State())
	}
}

func TestMetadataTombstonesAndHelperBoundaries(t *testing.T) {
	document := mustDocument(t, "author")
	if _, err := document.Insert(0, "ab"); err != nil {
		t.Fatal(err)
	}
	if _, err := document.Format(0, 1, []AttributeChange{{Key: "bold", Value: "true"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := document.Format(0, 1, []AttributeChange{{Key: "bold", Remove: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := document.Format(1, 1, []AttributeChange{{Key: "italic", Value: "true"}}); err != nil {
		t.Fatal(err)
	}
	if state := document.State(); state.Type != "rich-text" || state.TombstoneCount == 0 {
		t.Fatalf("diagnostic state = %#v", state)
	}
	if _, err := document.SnapshotCurrentStateWithLimits(frame.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	state, err := document.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	limited := DefaultOptions()
	limited.MaxMarkEntries = 1
	target, err := NewWithOptions("target", limited)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.UnmarshalBinary(state); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("state mark limit = %v", err)
	}

	positionA := text.Position{ReplicaID: "a", WallTime: 1}
	positionB := text.Position{ReplicaID: "b", WallTime: 1}
	tag := crdt.Tag{ReplicaID: "writer", WallTime: 1}
	targetsPayload := frame.AppendUvarint(nil, 2)
	targetsPayload = frame.AppendTag(targetsPayload, positionA)
	targetsPayload = frame.AppendTag(targetsPayload, positionB)
	var total int
	targets, next, ok := readTargets(targetsPayload, 0, frame.DefaultLimits(), &total)
	if !ok || next != len(targetsPayload) || total != 2 || len(targets) != 2 {
		t.Fatalf("readTargets = %#v, %d, %t, total=%d", targets, next, ok, total)
	}
	changesPayload := frame.AppendUvarint(nil, 2)
	changesPayload, err = appendString(changesPayload, "bold", frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	changesPayload = frame.AppendUvarint(changesPayload, 0)
	changesPayload, err = appendString(changesPayload, "true", frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	changesPayload, err = appendString(changesPayload, "link", frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	changesPayload = frame.AppendUvarint(changesPayload, 1)
	changes, next, ok := readChanges(changesPayload, 0, frame.DefaultLimits())
	if !ok || next != len(changesPayload) || len(changes) != 2 || !changes[1].Remove {
		t.Fatalf("readChanges = %#v, %d, %t", changes, next, ok)
	}
	if err := validateDelta(Delta{operations: []formatOperation{{tag: tag, targets: targets, changes: changes}}}, frame.DefaultLimits()); err != nil {
		t.Fatalf("validate helper delta = %v", err)
	}
	tight := frame.DefaultLimits()
	tight.MaxElements = 1
	if err := validateDelta(Delta{operations: []formatOperation{{tag: tag, targets: targets, changes: changes}}}, tight); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("target count limit = %v", err)
	}

	document.mu.Lock()
	position := document.text.Positions()[0]
	document.marks[position] = markSet{key: "bold", value: markValue{tag: tag, value: "true"}}
	document.markCount = 1
	err = document.preflightOperationsLocked([]formatOperation{{tag: tag, targets: []text.Position{position}, changes: []AttributeChange{{Key: "bold", Value: "false"}}}})
	document.mu.Unlock()
	if !errors.Is(err, ErrTagConflict) {
		t.Fatalf("preflight equal-tag conflict = %v", err)
	}
}

func TestPublicLimitAndRecoveryFailurePaths(t *testing.T) {
	options := DefaultOptions()
	options.MaxAttributesPerOperation = 1
	document, err := NewWithOptions("author", options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := document.InsertWithAttributes(0, "a", Attributes{"bold": "true", "italic": "true"}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("insert attribute limit = %v", err)
	}
	if document.String() != "" {
		t.Fatal("rejected formatted insert changed text")
	}
	if _, err := document.Insert(0, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := document.Format(0, 1, []AttributeChange{{Key: "bold", Value: "true"}, {Key: "italic", Value: "true"}}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("format attribute limit = %v", err)
	}
	if _, err := document.Format(0, 0, nil); err != nil {
		t.Fatalf("empty format = %v", err)
	}
	tight := frame.DefaultLimits()
	tight.MaxPayload = 1
	if _, err := document.DeleteWithLimits(0, 1, tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tight delete = %v", err)
	}
	if document.String() != "a" {
		t.Fatal("rejected delete changed text")
	}
	if _, err := NewWithOptions(" ", DefaultOptions()); err == nil {
		t.Fatal("invalid replica ID accepted")
	}
	if _, err := NewFromClock(clock.State{ReplicaID: " "}); err == nil {
		t.Fatal("invalid clock replica ID accepted")
	}
	if _, err := NewFromSnapshotWithOptions(snapshot.Snapshot{TypeID: crdt.TypeIDRichTextState}, DefaultOptions(), frame.DefaultLimits()); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("invalid recovery snapshot = %v", err)
	}
	if _, err := document.SnapshotCurrentStateWithLimits(tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tight snapshot = %v", err)
	}
	if err := document.ApplyDelta(Delta{textDelta: []byte("bad")}); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("bad direct apply = %v", err)
	}
}

func TestNilAndLowLevelWireLimits(t *testing.T) {
	var nilDocument *Document
	if nilDocument.ClockState() != (clock.State{}) || nilDocument.Spans() != nil {
		t.Fatal("nil clock or spans was not empty")
	}
	if _, err := nilDocument.Format(0, 0, nil); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil Format = %v", err)
	}
	if err := nilDocument.UnmarshalBinary(nil); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil UnmarshalBinary = %v", err)
	}
	if _, err := nilDocument.SnapshotCurrentState(); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil Snapshot = %v", err)
	}
	if _, err := marshalDelta(Delta{}, frame.DecoderLimits{}); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("invalid delta limits = %v", err)
	}
	tight := frame.DefaultLimits()
	tight.MaxPayload = 1
	if _, err := appendBytes([]byte{1}, []byte{2}, tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("payload append limit = %v", err)
	}
	if _, err := appendString([]byte{1}, "x", tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("string append payload limit = %v", err)
	}
	if _, err := marshalState(richState{textState: []byte("not a text frame")}, frame.DefaultLimits()); err == nil {
		t.Fatal("invalid nested state accepted")
	}
	wrong, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRichTextState, CodecID: "codec", Payload: nil})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unmarshalState(wrong, frame.DefaultLimits()); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("wrong state codec = %v", err)
	}
}

func TestSnapshotRecoveryWithExplicitOptions(t *testing.T) {
	source := mustDocument(t, "source")
	if _, err := source.InsertWithAttributes(0, "recover", Attributes{"bold": "true"}); err != nil {
		t.Fatal(err)
	}
	saved, err := source.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewFromSnapshotWithOptions(saved, DefaultOptions(), frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !spansEqual(restored.Spans(), source.Spans()) {
		t.Fatalf("explicit recovery spans = %#v, want %#v", restored.Spans(), source.Spans())
	}
	invalid := DefaultOptions()
	invalid.MaxMarkEntries = 0
	if _, err := NewFromSnapshotWithOptions(saved, invalid, frame.DefaultLimits()); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid recovery options = %v", err)
	}
}

func TestMetadataPreflightCapacityAndOlderWrites(t *testing.T) {
	options := DefaultOptions()
	options.MaxMarkEntries = 1
	document, err := NewWithOptions("author", options)
	if err != nil {
		t.Fatal(err)
	}
	first := text.Position{ReplicaID: "one", WallTime: 1}
	second := text.Position{ReplicaID: "two", WallTime: 1}
	newer := markValue{tag: crdt.Tag{ReplicaID: "writer", WallTime: 2}, value: "new"}
	document.marks[first] = markSet{key: "bold", value: newer}
	document.markCount = 1
	if err := document.preflightMarksLocked(map[text.Position]markSet{
		second: {key: "italic", value: markValue{tag: crdt.Tag{ReplicaID: "writer", WallTime: 3}, value: "true"}},
	}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("metadata capacity = %v", err)
	}
	document.applyMarksLocked(map[text.Position]markSet{
		first: {key: "bold", value: markValue{tag: crdt.Tag{ReplicaID: "writer", WallTime: 1}, value: "old"}},
	})
	if got := document.marks[first].value.value; got != "new" {
		t.Fatalf("older mark overwrote newer value: %q", got)
	}
}

func TestResolveAnchorReturnsCurrentBoundaryAndRejectsNilDocument(t *testing.T) {
	document, err := New("anchor-author")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := document.Insert(0, "abc"); err != nil {
		t.Fatal(err)
	}
	anchor, err := document.AnchorAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if offset, err := document.ResolveAnchor(anchor); err != nil || offset != 1 {
		t.Fatalf("ResolveAnchor() = %d, %v", offset, err)
	}
	var nilDocument *Document
	if _, err := nilDocument.ResolveAnchor(text.Anchor{}); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil ResolveAnchor() = %v", err)
	}
}

func spansEqual(left, right []Span) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Text != right[index].Text || !attributesEqual(left[index].Attributes, right[index].Attributes) {
			return false
		}
	}
	return true
}
