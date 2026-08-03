package text

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
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

func TestUndoManagerFailsClosedAfterInsertedPositionCompaction(t *testing.T) {
	value := mustRGA(t, "writer")
	manager, err := NewUndoManager(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Insert(0, "B"); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Delete(0, 1); err != nil {
		t.Fatal(err)
	}
	if removed, err := value.CompactEligibleTombstones(value.TombstoneTags()); err != nil || removed != 1 {
		t.Fatalf("CompactEligibleTombstones() = %d, %v; want 1, nil", removed, err)
	}

	if _, err := manager.Undo(); !errors.Is(err, ErrUndoAnchorGone) {
		t.Fatalf("Undo after inserted-position compaction = %v, want %v", err, ErrUndoAnchorGone)
	}
	if got := len(value.TombstoneTags()); got != 0 {
		t.Fatalf("obsolete undo recreated %d tombstones", got)
	}
	if !manager.CanUndo() || manager.CanRedo() {
		t.Fatalf("failed undo changed history flags: undo=%t redo=%t", manager.CanUndo(), manager.CanRedo())
	}
}

func TestUndoManagerBoundsHistoryAndRetainsLatestEdit(t *testing.T) {
	value := mustRGA(t, "writer")
	manager, err := NewUndoManagerWithOptions(value, UndoOptions{MaxEntries: 2, MaxRunes: 2})
	if err != nil {
		t.Fatal(err)
	}
	for offset, input := range []string{"a", "b", "c"} {
		if _, err := manager.Insert(offset, input); err != nil {
			t.Fatal(err)
		}
	}
	if got := manager.Len(); got != 1 {
		t.Fatalf("history length after bounded reset = %d, want 1", got)
	}
	if _, err := manager.Undo(); err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "ab" {
		t.Fatalf("undo latest bounded edit = %q, want ab", got)
	}
	if _, err := manager.Undo(); !errors.Is(err, ErrNoUndo) {
		t.Fatalf("undo before bounded reset = %v, want %v", err, ErrNoUndo)
	}
	if _, err := manager.Redo(); err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "abc" {
		t.Fatalf("redo latest bounded edit = %q, want abc", got)
	}
}

func TestUndoManagerBoundsRunesAndRejectsOversizedEdit(t *testing.T) {
	value := mustRGA(t, "writer")
	manager, err := NewUndoManagerWithOptions(value, UndoOptions{MaxEntries: 8, MaxRunes: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Insert(0, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Insert(1, "bc"); err != nil {
		t.Fatal(err)
	}
	if got := manager.Len(); got != 1 {
		t.Fatalf("history length after rune-budget reset = %d, want 1", got)
	}
	if _, err := manager.Undo(); err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "a" {
		t.Fatalf("undo rune-bounded edit = %q, want a", got)
	}
	if _, err := manager.Redo(); err != nil {
		t.Fatal(err)
	}
	before := value.String()
	if _, err := manager.Insert(3, "too"); !errors.Is(err, ErrUndoHistoryLimit) {
		t.Fatalf("oversized insert = %v, want %v", err, ErrUndoHistoryLimit)
	}
	if got := value.String(); got != before {
		t.Fatalf("oversized insert changed RGA = %q, want %q", got, before)
	}
	historyBefore := manager.Len()
	if _, err := manager.Delete(0, 3); !errors.Is(err, ErrUndoHistoryLimit) {
		t.Fatalf("oversized delete = %v, want %v", err, ErrUndoHistoryLimit)
	}
	if got := value.String(); got != before {
		t.Fatalf("oversized delete changed RGA = %q, want %q", got, before)
	}
	if got := manager.Len(); got != historyBefore {
		t.Fatalf("oversized delete changed history length = %d, want %d", got, historyBefore)
	}
	for _, options := range []UndoOptions{{}, {MaxEntries: 1}, {MaxRunes: 1}} {
		if _, err := NewUndoManagerWithOptions(value, options); !errors.Is(err, ErrInvalidUndoOptions) {
			t.Fatalf("NewUndoManagerWithOptions(%+v) = %v, want %v", options, err, ErrInvalidUndoOptions)
		}
	}
}

func TestUndoManagerDiscardedRedoReleasesOwnedPositions(t *testing.T) {
	value := mustRGA(t, "writer")
	manager, err := NewUndoManagerWithOptions(value, UndoOptions{MaxEntries: 4, MaxRunes: 64})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 64; index++ {
		if _, err := manager.Insert(0, "x"); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Undo(); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Insert(0, "y"); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Undo(); err != nil {
			t.Fatal(err)
		}
	}
	if got := manager.Len(); got != 1 {
		t.Fatalf("history length after repeated branch edits = %d, want 1", got)
	}
	if got := len(manager.owners); got != 1 {
		t.Fatalf("owned position count after discarded redo = %d, want 1", got)
	}
	if _, err := manager.Redo(); err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "y" {
		t.Fatalf("redo after ownership cleanup = %q, want y", got)
	}
}

func TestUndoManagerRunV2ThreeReplicaUnreliableDelivery(t *testing.T) {
	alice := mustRGA(t, "undo-alice")
	bob := mustRGA(t, "undo-bob")
	carol := mustRGA(t, "undo-carol")
	base := mustInsertRGA(t, alice, 0, "Draft")
	mustApplyRGA(t, bob, base)
	mustApplyRGA(t, carol, base)

	manager, err := NewUndoManager(alice)
	if err != nil {
		t.Fatal(err)
	}
	localInsert, err := manager.Insert(5, " A")
	if err != nil {
		t.Fatal(err)
	}
	bobEdit := mustInsertRGA(t, bob, 5, " B")
	carolEdit := mustInsertRGA(t, carol, 5, " C")
	mustApplyRGARunDelta(t, alice, bobEdit)
	mustApplyRGARunDelta(t, alice, carolEdit)
	localDelete, err := manager.Delete(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	undo, err := manager.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if got := alice.String(); !strings.Contains(got, " B") || !strings.Contains(got, " C") {
		t.Fatalf("undo deleted concurrent remote text: %q", got)
	}
	redo, err := manager.Redo()
	if err != nil {
		t.Fatal(err)
	}

	changes := []Delta{base, localInsert, bobEdit, carolEdit, localDelete, undo, redo}
	for index, replica := range []*RGA{alice, bob, carol} {
		deliverRGARunChanges(t, replica, changes, int64(20260803+index))
	}
	want := alice.String()
	for _, replica := range []*RGA{bob, carol} {
		if got := replica.String(); got != want {
			t.Fatalf("replica text = %q, want %q", got, want)
		}
		if replica.PendingCount() != 0 {
			t.Fatalf("replica retained %d unresolved dependencies", replica.PendingCount())
		}
	}
	snapshot, err := bob.SnapshotRunCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewFromSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if got := recovered.String(); got != want {
		t.Fatalf("recovered text = %q, want %q", got, want)
	}
}

func TestUndoManagerSerializesConcurrentLocalEdits(t *testing.T) {
	value := mustRGA(t, "writer")
	manager, err := NewUndoManager(value)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 32
	errors := make(chan error, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := manager.Insert(0, "x")
			errors <- err
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := manager.Len(); got != workers {
		t.Fatalf("history length = %d, want %d", got, workers)
	}
	if got := utf8.RuneCountInString(value.String()); got != workers {
		t.Fatalf("visible rune count = %d, want %d", got, workers)
	}
}
