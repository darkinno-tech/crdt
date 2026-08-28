package richtext

import (
	"errors"
	"testing"

	"github.com/im10furry/crdt/text"
)

func TestDocumentAnchorRangePersistsAcrossRichTextReplicationAndRecovery(t *testing.T) {
	alice := mustDocument(t, "rich-anchor-alice")
	bob := mustDocument(t, "rich-anchor-bob")
	seed, err := alice.Insert(0, "abcd")
	if err != nil {
		t.Fatal(err)
	}
	if err := bob.ApplyDelta(seed); err != nil {
		t.Fatal(err)
	}
	anchors, err := alice.AnchorRangeAt(1, 3)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := anchors.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := text.UnmarshalAnchorRange(encoded)
	if err != nil {
		t.Fatal(err)
	}
	insert, err := bob.Insert(2, "X")
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.ApplyDelta(insert); err != nil {
		t.Fatal(err)
	}
	for _, document := range []*Document{alice, bob} {
		start, end, err := document.ResolveAnchorRange(persisted)
		if err != nil || start != 1 || end != 4 {
			t.Fatalf("ResolveAnchorRange() = %d, %d, %v; want 1, 4, nil", start, end, err)
		}
	}

	saved, err := alice.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	start, end, err := recovered.ResolveAnchorRange(persisted)
	if err != nil || start != 1 || end != 4 {
		t.Fatalf("recovered ResolveAnchorRange() = %d, %d, %v; want 1, 4, nil", start, end, err)
	}
}

func TestDocumentAnchorRangeConvergesAfterShuffledDuplicateRichTextDelivery(t *testing.T) {
	alice := mustDocument(t, "rich-anchor-sim-alice")
	bob := mustDocument(t, "rich-anchor-sim-bob")
	carol := mustDocument(t, "rich-anchor-sim-carol")
	seed, err := alice.Insert(0, "abcd")
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range []*Document{bob, carol} {
		if err := document.ApplyDelta(seed); err != nil {
			t.Fatal(err)
		}
	}
	anchors, err := bob.AnchorRangeAt(1, 3)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := anchors.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := text.UnmarshalAnchorRange(encoded)
	if err != nil {
		t.Fatal(err)
	}
	insert, err := carol.Insert(2, "X")
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := bob.Delete(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	changes := []Delta{seed, insert, deleted}
	documents := []*Document{alice, bob, carol}
	for index, document := range documents {
		deliverRichTextChanges(t, document, changes, int64(20260802+index))
		if got := document.String(); got != "abXd" {
			t.Fatalf("replica text = %q, want abXd", got)
		}
		start, end, err := document.ResolveAnchorRange(persisted)
		if err != nil || start != 1 || end != 3 {
			t.Fatalf("ResolveAnchorRange() = %d, %d, %v; want 1, 3, nil", start, end, err)
		}
	}
	wantState, err := alice.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range documents[1:] {
		state, err := document.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if string(state) != string(wantState) {
			t.Fatal("anchor simulation replicas produced different canonical state")
		}
	}
}

func TestDocumentAnchorRangeRejectsInvalidAndCompactedBoundaries(t *testing.T) {
	document := mustDocument(t, "rich-anchor-gc")
	if _, err := document.Insert(0, "ab"); err != nil {
		t.Fatal(err)
	}
	anchors, err := document.AnchorRangeAt(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := document.ResolveAnchorRange(text.AnchorRange{}); !errors.Is(err, text.ErrInvalidAnchor) {
		t.Fatalf("ResolveAnchorRange(invalid) = %v, want %v", err, text.ErrInvalidAnchor)
	}
	tags := document.TombstoneTags()
	if _, err := document.Delete(1, 1); err != nil {
		t.Fatal(err)
	}
	if len(tags) != 0 {
		t.Fatal("unexpected tombstone before delete")
	}
	position := anchors.Start.Position
	if removed, err := document.CompactTombstones([]text.Position{position}); err != nil || removed != 1 {
		t.Fatalf("CompactTombstones() = %d, %v", removed, err)
	}
	if _, _, err := document.ResolveAnchorRange(anchors); !errors.Is(err, text.ErrAnchorGone) {
		t.Fatalf("ResolveAnchorRange(compacted) = %v, want %v", err, text.ErrAnchorGone)
	}
	var nilDocument *Document
	if _, err := nilDocument.AnchorRangeAt(0, 0); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil AnchorRangeAt() = %v, want %v", err, ErrNilDocument)
	}
}
