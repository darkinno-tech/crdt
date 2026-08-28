package wasm

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/richtext"
	"github.com/im10furry/crdt/text"
)

func TestRichTextRuntimeEditorDeltaInteroperabilityAndRecovery(t *testing.T) {
	runtime, err := NewRichTextRuntime(DefaultRichTextOptions())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := runtime.Protocol(), (RichTextProtocol{StateTypeID: RichTextStateTypeID, DeltaTypeID: RichTextDeltaTypeID, SemanticsVersion: RichTextSemanticsVersion}); got != want {
		t.Fatalf("Protocol() = %#v, want %#v", got, want)
	}
	writer, err := runtime.Create("rich-writer")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := runtime.Create("rich-reader")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := runtime.ApplyEditorDelta(writer, []richtext.EditorOperation{{
		Insert: "Hello\n", Changes: []richtext.AttributeChange{{Key: richtext.AttributeBold, Value: "true"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ApplyDelta(reader, seed); err != nil {
		t.Fatal(err)
	}
	change, err := runtime.ApplyEditorDelta(writer, []richtext.EditorOperation{
		{Retain: 5, Changes: []richtext.AttributeChange{{Key: richtext.AttributeItalic, Value: "true"}}},
		{Insert: " world", Changes: []richtext.AttributeChange{{Key: "rt.link", Value: "issue-7"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ApplyDelta(reader, change); err != nil {
		t.Fatal(err)
	}
	writerSpans, err := runtime.Spans(writer)
	if err != nil {
		t.Fatal(err)
	}
	readerSpans, err := runtime.Spans(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(writerSpans, readerSpans) {
		t.Fatalf("reader Spans() = %#v, want %#v", readerSpans, writerSpans)
	}
	saved, err := runtime.Snapshot(writer)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := runtime.Restore(saved)
	if err != nil {
		t.Fatal(err)
	}
	restoredSpans, err := runtime.Spans(restored)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restoredSpans, writerSpans) {
		t.Fatalf("restored Spans() = %#v, want %#v", restoredSpans, writerSpans)
	}
}

func TestRichTextRuntimeRejectsBoundedEditorInputWithoutMutation(t *testing.T) {
	options := DefaultRichTextOptions()
	options.Document.MaxMarkEntries = 1
	options.MaxLocalEditorOps = 1
	runtime, err := NewRichTextRuntime(options)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Create("rich-bounded")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ApplyEditorDelta(handle, []richtext.EditorOperation{{Insert: "abc"}}); err != nil {
		t.Fatal(err)
	}
	before, err := runtime.Spans(handle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ApplyEditorDelta(handle, []richtext.EditorOperation{{Retain: 1}, {Delete: 1}}); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("too many editor operations = %v, want %v", err, frame.ErrFrameLimit)
	}
	resourceOptions := options
	resourceOptions.MaxLocalEditorOps = 8
	resourceRuntime, err := NewRichTextRuntime(resourceOptions)
	if err != nil {
		t.Fatal(err)
	}
	resourceHandle, err := resourceRuntime.Create("rich-resource")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resourceRuntime.ApplyEditorDelta(resourceHandle, []richtext.EditorOperation{{Insert: "abc"}}); err != nil {
		t.Fatal(err)
	}
	resourceBefore, err := resourceRuntime.Spans(resourceHandle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resourceRuntime.ApplyEditorDelta(resourceHandle, []richtext.EditorOperation{
		{Retain: 1},
		{Delete: 1},
		{Insert: "X", Changes: []richtext.AttributeChange{{Key: "bold", Value: "true"}, {Key: "italic", Value: "true"}}},
	}); !errors.Is(err, richtext.ErrResourceLimit) {
		t.Fatalf("over-limit editor transaction = %v, want %v", err, richtext.ErrResourceLimit)
	}
	after, err := resourceRuntime.Spans(resourceHandle)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, resourceBefore) {
		t.Fatalf("rejected editor transaction changed spans: %#v, want %#v", after, resourceBefore)
	}
	if err := runtime.ApplyDelta(handle, []byte("invalid")); err == nil {
		t.Fatal("invalid remote rich-text frame was accepted")
	}
	if got, err := runtime.Spans(handle); err != nil || !reflect.DeepEqual(got, before) {
		t.Fatalf("invalid remote frame changed spans: %#v, %v", got, err)
	}
}

func TestRichTextRuntimePersistsBoundedAnchorRangesOutsideFrames(t *testing.T) {
	runtime, err := NewRichTextRuntime(DefaultRichTextOptions())
	if err != nil {
		t.Fatal(err)
	}
	alice, err := runtime.Create("rich-anchor-alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := runtime.Create("rich-anchor-bob")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := runtime.ApplyEditorDelta(alice, []richtext.EditorOperation{{Insert: "abcd"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ApplyDelta(bob, seed); err != nil {
		t.Fatal(err)
	}
	anchors, err := runtime.AnchorRangeAt(alice, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := runtime.MarshalAnchorRange(anchors)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) == 0 || len(encoded) > runtime.MaxAnchorBytes() {
		t.Fatalf("encoded anchor range length = %d, max = %d", len(encoded), runtime.MaxAnchorBytes())
	}
	persisted, err := runtime.UnmarshalAnchorRange(encoded)
	if err != nil {
		t.Fatal(err)
	}
	change, err := runtime.ApplyEditorDelta(bob, []richtext.EditorOperation{{Retain: 1}, {Insert: "X"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ApplyDelta(alice, change); err != nil {
		t.Fatal(err)
	}
	for _, handle := range []uint64{alice, bob} {
		start, end, err := runtime.ResolveAnchorRange(handle, persisted)
		if err != nil || start != 4 || end != 2 {
			t.Fatalf("ResolveAnchorRange() = %d, %d, %v; want 4, 2, nil", start, end, err)
		}
	}
	if _, err := runtime.UnmarshalAnchor([]byte{1, 3, 0}); !errors.Is(err, text.ErrInvalidAnchor) {
		t.Fatalf("UnmarshalAnchor(invalid) = %v, want %v", err, text.ErrInvalidAnchor)
	}
}

func TestRichTextRuntimePersistsOneBoundedAnchorOutsideFrames(t *testing.T) {
	runtime, err := NewRichTextRuntime(DefaultRichTextOptions())
	if err != nil {
		t.Fatal(err)
	}
	handle, err := runtime.Create("rich-single-anchor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ApplyEditorDelta(handle, []richtext.EditorOperation{{Insert: "abcd"}}); err != nil {
		t.Fatal(err)
	}
	anchor, err := runtime.AnchorAt(handle, 2)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := runtime.MarshalAnchor(anchor)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := runtime.UnmarshalAnchor(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if offset, err := runtime.ResolveAnchor(handle, decoded); err != nil || offset != 2 {
		t.Fatalf("ResolveAnchor() = %d, %v, want 2, nil", offset, err)
	}
	if _, err := runtime.AnchorAt(0, 0); !errors.Is(err, ErrUnknownDocument) {
		t.Fatalf("AnchorAt(unknown) = %v, want %v", err, ErrUnknownDocument)
	}
	if _, err := runtime.ResolveAnchor(0, decoded); !errors.Is(err, ErrUnknownDocument) {
		t.Fatalf("ResolveAnchor(unknown) = %v, want %v", err, ErrUnknownDocument)
	}

	var nilRuntime *RichTextRuntime
	if got := nilRuntime.MaxAnchorBytes(); got != 0 {
		t.Fatalf("nil MaxAnchorBytes() = %d, want 0", got)
	}
	if _, err := nilRuntime.MarshalAnchor(decoded); !errors.Is(err, ErrUnknownDocument) {
		t.Fatalf("nil MarshalAnchor() = %v, want %v", err, ErrUnknownDocument)
	}
	if _, err := nilRuntime.UnmarshalAnchor(encoded); !errors.Is(err, ErrUnknownDocument) {
		t.Fatalf("nil UnmarshalAnchor() = %v, want %v", err, ErrUnknownDocument)
	}
	if _, err := nilRuntime.MarshalAnchorRange(text.AnchorRange{}); !errors.Is(err, ErrUnknownDocument) {
		t.Fatalf("nil MarshalAnchorRange() = %v, want %v", err, ErrUnknownDocument)
	}
	if _, err := nilRuntime.UnmarshalAnchorRange(encoded); !errors.Is(err, ErrUnknownDocument) {
		t.Fatalf("nil UnmarshalAnchorRange() = %v, want %v", err, ErrUnknownDocument)
	}
}

func BenchmarkRichTextRuntimeApplyEditorDelta(b *testing.B) {
	runtime, err := NewRichTextRuntime(DefaultRichTextOptions())
	if err != nil {
		b.Fatal(err)
	}
	seedHandle, err := runtime.Create("rich-benchmark-seed")
	if err != nil {
		b.Fatal(err)
	}
	seed, err := runtime.ApplyEditorDelta(seedHandle, []richtext.EditorOperation{{Insert: strings.Repeat("a", 8_192)}})
	if err != nil {
		b.Fatal(err)
	}
	operations := []richtext.EditorOperation{
		{Retain: 4096, Changes: []richtext.AttributeChange{{Key: richtext.AttributeBold, Value: "true"}}},
		{Insert: " review", Changes: []richtext.AttributeChange{{Key: richtext.AttributeItalic, Value: "true"}}},
	}
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		b.StopTimer()
		handle, err := runtime.Create("rich-benchmark-target")
		if err != nil {
			b.Fatal(err)
		}
		if err := runtime.ApplyDelta(handle, seed); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := runtime.ApplyEditorDelta(handle, operations); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if !runtime.Drop(handle) {
			b.Fatal("benchmark handle was absent")
		}
	}
}
