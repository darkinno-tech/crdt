package text

import (
	"errors"
	"testing"
)

func TestUndoManagerInsertDeleteAndRedo(t *testing.T) {
	value := mustRGA(t, "writer")
	manager, err := NewUndoManager(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Insert(0, "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Delete(1, 3); err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "ho" {
		t.Fatalf("after delete = %q, want ho", got)
	}
	if _, err := manager.Undo(); err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "hello" {
		t.Fatalf("undo delete = %q, want hello", got)
	}
	if _, err := manager.Undo(); err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "" {
		t.Fatalf("undo insert = %q, want empty", got)
	}
	if _, err := manager.Redo(); err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "hello" {
		t.Fatalf("redo insert = %q, want hello", got)
	}
	if _, err := manager.Redo(); err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "ho" {
		t.Fatalf("redo delete = %q, want ho", got)
	}
	if manager.CanUndo() != true || manager.CanRedo() {
		t.Fatalf("history flags undo=%t redo=%t", manager.CanUndo(), manager.CanRedo())
	}
	manager.Clear()
	if _, err := manager.Undo(); !errors.Is(err, ErrNoUndo) {
		t.Fatalf("Undo after Clear = %v", err)
	}
	if _, err := manager.Redo(); !errors.Is(err, ErrNoRedo) {
		t.Fatalf("Redo after Clear = %v", err)
	}
}

func TestUndoManagerConvergesWithConcurrentRemoteEdit(t *testing.T) {
	left := mustRGA(t, "left")
	right := mustRGA(t, "right")
	manager, err := NewUndoManager(left)
	if err != nil {
		t.Fatal(err)
	}
	base, err := manager.Insert(0, "A")
	if err != nil {
		t.Fatal(err)
	}
	if err := right.ApplyDelta(base); err != nil {
		t.Fatal(err)
	}
	local, err := manager.Insert(1, "B")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := right.Insert(1, "X")
	if err != nil {
		t.Fatal(err)
	}
	if err := left.ApplyDelta(remote); err != nil {
		t.Fatal(err)
	}
	if err := right.ApplyDelta(local); err != nil {
		t.Fatal(err)
	}

	undo, err := manager.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if err := right.ApplyDelta(undo); err != nil {
		t.Fatal(err)
	}
	if got, want := left.String(), right.String(); got != want || got == "" {
		t.Fatalf("after undo left=%q right=%q", got, want)
	}
	if got := left.String(); got != "AX" {
		t.Fatalf("undo removed remote text: %q", got)
	}

	redo, err := manager.Redo()
	if err != nil {
		t.Fatal(err)
	}
	if err := right.ApplyDelta(redo); err != nil {
		t.Fatal(err)
	}
	if got, want := left.String(), right.String(); got != want {
		t.Fatalf("after redo left=%q right=%q", got, want)
	}
	if got := left.String(); len([]rune(got)) != 3 {
		t.Fatalf("redo result = %q, want three characters", got)
	}
}

func TestUndoManagerFailsClosedAfterAnchorCompaction(t *testing.T) {
	value := mustRGA(t, "writer")
	if _, err := value.Insert(0, "A"); err != nil {
		t.Fatal(err)
	}
	manager, err := NewUndoManager(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Insert(1, "B"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Undo(); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Delete(0, 1); err != nil {
		t.Fatal(err)
	}
	tags := value.TombstoneTags()
	if removed, err := value.CompactEligibleTombstones(tags); err != nil || removed != 2 {
		t.Fatalf("CompactEligibleTombstones() = %d, %v", removed, err)
	}
	if _, err := manager.Redo(); !errors.Is(err, ErrUndoAnchorGone) {
		t.Fatalf("Redo after anchor compaction = %v", err)
	}
}
