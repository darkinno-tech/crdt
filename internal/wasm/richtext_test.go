package wasm

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/richtext"
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
