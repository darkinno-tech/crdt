package richtext

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/text"
	"github.com/im10furry/crdt/tombstonegc"
)

func TestRichCompactionErrorRecognizesWrappedUnsafeCompaction(t *testing.T) {
	wrapped := fmt.Errorf("remote compaction: %w", text.ErrUnsafeCompaction)
	if got := richCompactionError(wrapped); !errors.Is(got, ErrUnsafeCompaction) {
		t.Fatalf("richCompactionError(%v) = %v, want %v", wrapped, got, ErrUnsafeCompaction)
	}
}

func TestDocumentExactAcknowledgementCompactsTextAndFormattingTombstones(t *testing.T) {
	source := mustDocument(t, "source")
	insert, err := source.Insert(0, "a")
	if err != nil {
		t.Fatal(err)
	}
	assign, err := source.Format(0, 1, []AttributeChange{{Key: "bold", Value: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	removeMark, err := source.Format(0, 1, []AttributeChange{{Key: "bold", Remove: true}})
	if err != nil {
		t.Fatal(err)
	}
	removeText, err := source.Delete(0, 1)
	if err != nil {
		t.Fatal(err)
	}

	remote := mustDocument(t, "remote")
	// Deliver formatting and deletion before their text to exercise the same
	// delayed-delivery lifecycle that the compactor must preserve.
	for _, change := range []Delta{removeMark, removeText, assign, insert} {
		if err := remote.ApplyDelta(change); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := remote.TombstoneTags(), source.TombstoneTags(); !reflect.DeepEqual(got, want) {
		t.Fatalf("remote tombstones = %#v, want %#v", got, want)
	}

	const groupID = "editor/richtext/v1"
	coordinator, err := tombstonegc.NewCoordinator[struct{}](groupID, []string{"source", "remote"})
	if err != nil {
		t.Fatal(err)
	}
	epoch := coordinator.Membership().Epoch
	if removed, err := coordinator.AcknowledgeAndCompactTarget(groupID, "source", epoch, source.TombstoneTags(), source); err != nil || removed != 0 {
		t.Fatalf("source acknowledgement = %d, %v; want 0, nil", removed, err)
	}
	if removed, err := coordinator.AcknowledgeAndCompactTarget(groupID, "remote", epoch, remote.TombstoneTags(), source); err != nil || removed != 2 {
		t.Fatalf("remote acknowledgement = %d, %v; want 2, nil", removed, err)
	}
	if got := source.TombstoneTags(); len(got) != 0 {
		t.Fatalf("compacted tombstones = %#v, want none", got)
	}
	if source.markCount != 0 || len(source.marks) != 0 {
		t.Fatalf("compacted formatting metadata = count %d, marks %#v", source.markCount, source.marks)
	}
	if state := source.State(); state.ElementCount != 0 || state.TombstoneCount != 0 {
		t.Fatalf("compacted state = %#v", state)
	}
	saved, err := source.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	if got := recovered.State(); got.ElementCount != 0 || got.TombstoneCount != 0 {
		t.Fatalf("recovered compacted state = %#v", got)
	}
}

func TestDocumentCompactionPreservesFormattingForAnUnresolvedPosition(t *testing.T) {
	source := mustDocument(t, "source")
	if _, err := source.Insert(0, "a"); err != nil {
		t.Fatal(err)
	}
	format, err := source.Format(0, 1, []AttributeChange{{Key: "bold", Value: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	target := mustDocument(t, "target")
	if err := target.ApplyDelta(format); err != nil {
		t.Fatal(err)
	}
	if target.markCount != 1 {
		t.Fatalf("unresolved mark count = %d, want 1", target.markCount)
	}

	// A selected but unknown text tombstone is a no-op. It must not make an
	// out-of-order live formatting assignment look like an orphaned position.
	unknown := crdt.Tag{ReplicaID: "other", WallTime: 1}
	if removed, err := target.CompactTombstones([]crdt.Tag{unknown}); err != nil || removed != 0 {
		t.Fatalf("CompactTombstones() = %d, %v; want 0, nil", removed, err)
	}
	if target.markCount != 1 {
		t.Fatalf("compaction dropped unresolved live mark: %d", target.markCount)
	}
	if _, err := target.CompactTombstones([]crdt.Tag{{}}); !errors.Is(err, ErrUnsafeCompaction) {
		t.Fatalf("invalid compaction tag = %v, want %v", err, ErrUnsafeCompaction)
	}
}

func TestDocumentCompactsAttributeTombstoneWithoutRemovingText(t *testing.T) {
	document := mustDocument(t, "writer")
	if _, err := document.Insert(0, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := document.Format(0, 1, []AttributeChange{{Key: "bold", Value: "true"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := document.Format(0, 1, []AttributeChange{{Key: "bold", Remove: true}}); err != nil {
		t.Fatal(err)
	}
	tags := document.TombstoneTags()
	if len(tags) != 1 {
		t.Fatalf("attribute tombstones = %#v, want one", tags)
	}
	if removed, err := document.CompactTombstones(tags); err != nil || removed != 1 {
		t.Fatalf("CompactTombstones() = %d, %v; want 1, nil", removed, err)
	}
	if document.String() != "a" || document.markCount != 0 {
		t.Fatalf("attribute-only compaction = text %q marks %d", document.String(), document.markCount)
	}
}

func TestDocumentCompactionFailsClosedForStructuralText(t *testing.T) {
	document := mustDocument(t, "writer")
	if _, err := document.Insert(0, "ab"); err != nil {
		t.Fatal(err)
	}
	if _, err := document.Delete(0, 2); err != nil {
		t.Fatal(err)
	}
	tags := document.TombstoneTags()
	before := document.State()
	if removed, err := document.CompactTombstones(tags); !errors.Is(err, ErrUnsafeCompaction) || removed != 0 {
		t.Fatalf("CompactTombstones() = %d, %v; want 0, %v", removed, err, ErrUnsafeCompaction)
	}
	if got := document.State(); got != before {
		t.Fatalf("unsafe compaction changed state: got %#v, want %#v", got, before)
	}
}

func TestMarkSetRemovePreservesInlineRepresentation(t *testing.T) {
	var marks markSet
	if marks.remove("missing") {
		t.Fatal("empty mark set removed a key")
	}
	marks = markSet{
		key:   "z",
		value: markValue{value: "primary"},
		extra: map[string]markValue{
			"a": {value: "first"},
			"m": {value: "middle"},
		},
	}
	if !marks.remove("z") || marks.key != "a" || marks.value.value != "first" {
		t.Fatalf("primary removal = %#v", marks)
	}
	if !marks.remove("m") || marks.len() != 1 || marks.extra != nil {
		t.Fatalf("extra removal = %#v", marks)
	}
	if !marks.remove("a") || marks.len() != 0 {
		t.Fatalf("final removal = %#v", marks)
	}
	if marks.remove("missing") {
		t.Fatal("removed missing key")
	}
}

func TestNilDocumentTombstoneSurface(t *testing.T) {
	var document *Document
	if tags := document.TombstoneTags(); tags != nil {
		t.Fatalf("nil TombstoneTags() = %#v", tags)
	}
	if removed, err := document.CompactEligibleTombstones(nil); !errors.Is(err, ErrNilDocument) || removed != 0 {
		t.Fatalf("nil CompactEligibleTombstones() = %d, %v", removed, err)
	}
}
