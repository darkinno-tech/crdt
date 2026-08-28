package text

import (
	"errors"
	"testing"

	frame "github.com/im10furry/crdt/encoding"
)

func TestBinaryMutationHelpersPreflightAndReplicate(t *testing.T) {
	if ErrNoUndo.Error() != "text: no undo operation" || ErrNoRedo.Error() != "text: no redo operation" {
		t.Fatal("undo history errors changed their stable messages")
	}
	writer := mustRGA(t, "writer")
	limits := frame.DefaultLimits()
	inserted, err := writer.InsertBinaryWithLimits(0, "ab", limits)
	if err != nil {
		t.Fatal(err)
	}
	if got := writer.String(); got != "ab" {
		t.Fatalf("inserted text = %q", got)
	}
	insertDelta, err := UnmarshalRGADelta(inserted)
	if err != nil {
		t.Fatal(err)
	}
	replica := mustRGA(t, "replica")
	if err := replica.ApplyDelta(insertDelta); err != nil || replica.String() != "ab" {
		t.Fatalf("replicated insert = %q, %v", replica.String(), err)
	}

	deleted, err := writer.DeleteBinaryWithLimits(0, 1, limits)
	if err != nil {
		t.Fatal(err)
	}
	if got := writer.String(); got != "b" {
		t.Fatalf("deleted text = %q", got)
	}
	deleteDelta, err := UnmarshalRGADelta(deleted)
	if err != nil {
		t.Fatal(err)
	}
	if err := replica.ApplyDelta(deleteDelta); err != nil || replica.String() != "b" {
		t.Fatalf("replicated delete = %q, %v", replica.String(), err)
	}

	tight := limits
	tight.MaxPayload = 1
	if _, err := writer.InsertBinaryWithLimits(1, "x", tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("limited insert = %v", err)
	}
	if _, err := writer.DeleteBinaryWithLimits(0, 1, tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("limited delete = %v", err)
	}
	if got := writer.String(); got != "b" {
		t.Fatalf("rejected binary mutation changed text = %q", got)
	}
}
