package text

import (
	"errors"
	"testing"

	frame "github.com/darkinno-tech/crdt/encoding"
)

func TestRGAReplaceRunBinaryIsAtomicAndReplicable(t *testing.T) {
	writer := mustRGA(t, "writer")
	base := mustInsertRGA(t, writer, 0, "abc")
	receiver := mustRGA(t, "receiver")
	mustApplyRGA(t, receiver, base)

	encoded, err := writer.ReplaceRunBinaryWithLimits(1, 1, "XY", frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	delta, err := UnmarshalRGARunDelta(encoded)
	if err != nil {
		t.Fatal(err)
	}
	mustApplyRGA(t, receiver, delta)
	if got, want := writer.String(), "aXYc"; got != want {
		t.Fatalf("writer = %q, want %q", got, want)
	}
	if got, want := receiver.String(), writer.String(); got != want {
		t.Fatalf("receiver = %q, want %q", got, want)
	}
}

func TestRGAReplaceBinaryIsAtomicAndReplicable(t *testing.T) {
	writer := mustRGA(t, "writer")
	base := mustInsertRGA(t, writer, 0, "abc")
	receiver := mustRGA(t, "receiver")
	mustApplyRGA(t, receiver, base)

	encoded, err := writer.ReplaceBinaryWithLimits(1, 1, "Z", frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	delta, err := UnmarshalRGADelta(encoded)
	if err != nil {
		t.Fatal(err)
	}
	mustApplyRGA(t, receiver, delta)
	if got, want := writer.String(), "aZc"; got != want {
		t.Fatalf("writer = %q, want %q", got, want)
	}
	if got, want := receiver.String(), writer.String(); got != want {
		t.Fatalf("receiver = %q, want %q", got, want)
	}
}

func TestRGAReplaceRejectsBeforeMutatingVisibleState(t *testing.T) {
	document := mustRGA(t, "writer")
	mustInsertRGA(t, document, 0, "abc")
	before := document.String()
	tight := frame.DefaultLimits()
	tight.MaxElements = 1
	if _, err := document.ReplaceRunBinaryWithLimits(1, 1, "XY", tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("replace error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if got := document.String(); got != before {
		t.Fatalf("text after rejected replace = %q, want %q", got, before)
	}
	if _, err := document.ReplaceRunBinaryWithLimits(-1, 0, "x", frame.DefaultLimits()); !errors.Is(err, ErrRange) {
		t.Fatalf("invalid range error = %v, want %v", err, ErrRange)
	}
	if _, err := document.ReplaceRunBinaryWithLimits(0, 0, string([]byte{0xff}), frame.DefaultLimits()); !errors.Is(err, ErrInvalidText) {
		t.Fatalf("invalid text error = %v, want %v", err, ErrInvalidText)
	}
	var nilDocument *RGA
	if _, err := nilDocument.ReplaceBinaryWithLimits(0, 0, "x", frame.DefaultLimits()); !errors.Is(err, ErrNilText) {
		t.Fatalf("nil document error = %v, want %v", err, ErrNilText)
	}
}
