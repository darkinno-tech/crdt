package richtext

import (
	"errors"
	"reflect"
	"testing"
)

func TestApplyEditorDeltaPreservesInsertedAndRetainedFormatting(t *testing.T) {
	writer, reader := mustDocument(t, "editor-writer"), mustDocument(t, "editor-reader")
	seed, err := writer.Insert(0, "Hello world")
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.ApplyDelta(seed); err != nil {
		t.Fatal(err)
	}

	change, err := writer.ApplyEditorDelta([]EditorOperation{
		{Retain: 5, Changes: []AttributeChange{{Key: AttributeBold, Value: "true"}}},
		{Retain: 1},
		{Delete: 5},
		{Insert: "CRDT", Changes: []AttributeChange{{Key: AttributeItalic, Value: "true"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := writer.String(), "Hello CRDT"; got != want {
		t.Fatalf("writer String() = %q, want %q", got, want)
	}
	wantSpans := []Span{
		{Text: "Hello", Attributes: Attributes{AttributeBold: "true"}},
		{Text: " "},
		{Text: "CRDT", Attributes: Attributes{AttributeItalic: "true"}},
	}
	if got := writer.Spans(); !reflect.DeepEqual(got, wantSpans) {
		t.Fatalf("writer Spans() = %#v, want %#v", got, wantSpans)
	}

	encoded, err := change.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalDelta(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
	if got := reader.Spans(); !reflect.DeepEqual(got, wantSpans) {
		t.Fatalf("reader Spans() = %#v, want %#v", got, wantSpans)
	}
	writerState, err := writer.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	readerState, err := reader.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if string(writerState) != string(readerState) {
		t.Fatal("editor delta did not produce equal canonical states")
	}
}

func TestApplyEditorDeltaExpandsQuillStyleBlockNewline(t *testing.T) {
	document := mustDocument(t, "editor-block")
	if _, err := document.ApplyEditorDelta([]EditorOperation{
		{Insert: "Title"},
		{Insert: "\n", Changes: []AttributeChange{{Key: AttributeBlock, Value: "heading:2"}}},
		{Insert: "Body\n"},
	}); err != nil {
		t.Fatal(err)
	}
	want := []Block{
		{Text: "Title", Format: BlockFormat{Kind: "heading", Level: 2}, Formatted: true},
		{Text: "Body"},
	}
	if got := document.Blocks(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Blocks() = %#v, want %#v", got, want)
	}
	if _, err := document.ApplyEditorDelta([]EditorOperation{
		{Retain: 10},
		{Retain: 1, Changes: []AttributeChange{{Key: AttributeBlock, Value: "quote"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := document.Blocks()[1], (Block{Text: "Body", Format: BlockFormat{Kind: "quote"}, Formatted: true}); got != want {
		t.Fatalf("updated block = %#v, want %#v", got, want)
	}
}

func TestApplyEditorDeltaRejectsInvalidOrOverLimitTransactionAtomically(t *testing.T) {
	options := DefaultOptions()
	options.MaxMarkEntries = 1
	document, err := NewWithOptions("editor-atomic", options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := document.Insert(0, "abc"); err != nil {
		t.Fatal(err)
	}
	before, err := document.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	beforeClock := document.ClockState()

	if _, err := document.ApplyEditorDelta([]EditorOperation{{Retain: 1, Delete: 1}}); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("mixed editor operation = %v, want %v", err, ErrInvalidDelta)
	}
	if _, err := document.ApplyEditorDelta([]EditorOperation{
		{Retain: 1},
		{Delete: 1},
		{Insert: "X", Changes: []AttributeChange{{Key: "bold", Value: "true"}, {Key: "italic", Value: "true"}}},
	}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("over-limit replacement = %v, want %v", err, ErrResourceLimit)
	}
	if got, want := document.String(), "abc"; got != want {
		t.Fatalf("rejected editor transaction String() = %q, want %q", got, want)
	}
	after, err := document.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("rejected editor transaction changed replication state")
	}
	if got := document.ClockState(); got != beforeClock {
		t.Fatalf("rejected editor transaction changed clock: got %#v, want %#v", got, beforeClock)
	}
}

func TestApplyEditorDeltaConvergesAfterDuplicateShuffledDelivery(t *testing.T) {
	alice, bob, carol := mustDocument(t, "editor-alice"), mustDocument(t, "editor-bob"), mustDocument(t, "editor-carol")
	seed, err := alice.Insert(0, "review")
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range []*Document{bob, carol} {
		if err := document.ApplyDelta(seed); err != nil {
			t.Fatal(err)
		}
	}

	left, err := alice.ApplyEditorDelta([]EditorOperation{{Retain: 3, Changes: []AttributeChange{{Key: AttributeBold, Value: "true"}}}})
	if err != nil {
		t.Fatal(err)
	}
	middle, err := bob.ApplyEditorDelta([]EditorOperation{{Retain: 3}, {Insert: "-", Changes: []AttributeChange{{Key: AttributeItalic, Value: "true"}}}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := carol.ApplyEditorDelta([]EditorOperation{{Retain: 5}, {Delete: 1}, {Insert: "!"}})
	if err != nil {
		t.Fatal(err)
	}
	changes := []Delta{seed, left, middle, right}
	for index, document := range []*Document{alice, bob, carol} {
		deliverRichTextChanges(t, document, changes, int64(2026073100+index))
	}
	want, err := alice.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range []*Document{bob, carol} {
		got, err := document.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatal("editor transactions did not converge after duplicate shuffled delivery")
		}
	}
}
