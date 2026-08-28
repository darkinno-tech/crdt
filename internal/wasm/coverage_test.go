package wasm

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/im10furry/crdt"
	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/richtext"
)

func TestRichTextRuntimeBoundariesAccessorsAndRecovery(t *testing.T) {
	var nilRuntime *RichTextRuntime
	if nilRuntime.Protocol() != (RichTextProtocol{}) || nilRuntime.MaxFrameBytes() != 0 || nilRuntime.MaxTags() != 0 ||
		nilRuntime.MaxStringBytes() != 0 || nilRuntime.MaxLocalEditBytes() != 0 || nilRuntime.MaxLocalEditRunes() != 0 ||
		nilRuntime.MaxLocalEditorOps() != 0 || nilRuntime.MaxAttributesPerOperation() != 0 {
		t.Fatal("nil rich-text runtime exposed state")
	}
	if _, err := nilRuntime.Create("writer"); !errors.Is(err, ErrUnknownDocument) {
		t.Fatalf("nil Create() = %v", err)
	}
	if _, err := nilRuntime.Restore(RichTextSnapshot{}); !errors.Is(err, ErrUnknownDocument) {
		t.Fatalf("nil Restore() = %v", err)
	}
	if nilRuntime.Drop(1) {
		t.Fatal("nil Drop() succeeded")
	}

	if _, err := NewRichTextRuntime(RichTextOptions{}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("zero options = %v", err)
	}
	options := DefaultRichTextOptions()
	runtime, err := NewRichTextRuntime(options)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := runtime.Protocol(), (RichTextProtocol{StateTypeID: RichTextStateTypeID, DeltaTypeID: RichTextDeltaTypeID, SemanticsVersion: RichTextSemanticsVersion}); got != want {
		t.Fatalf("Protocol() = %#v, want %#v", got, want)
	}
	if runtime.MaxFrameBytes() != 1<<20 || runtime.MaxTags() != 100_000 || runtime.MaxStringBytes() != 64<<10 ||
		runtime.MaxLocalEditBytes() != 64<<10 || runtime.MaxLocalEditRunes() != 16<<10 || runtime.MaxLocalEditorOps() != 512 ||
		runtime.MaxAttributesPerOperation() != 32 {
		t.Fatal("unexpected rich-text runtime limits")
	}
	if _, err := runtime.Create(strings.Repeat("r", runtime.MaxStringBytes()+1)); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("overlong replica ID = %v", err)
	}
	if _, err := runtime.Create(" "); err == nil {
		t.Fatal("blank replica ID was accepted")
	}
	if runtime.Drop(0) || runtime.Drop(99) {
		t.Fatal("unknown document was dropped")
	}
	for _, operation := range []struct {
		name string
		err  error
	}{
		{"editor", func() error { _, err := runtime.ApplyEditorDelta(99, nil); return err }()},
		{"delta", runtime.ApplyDelta(99, nil)},
		{"spans", func() error { _, err := runtime.Spans(99); return err }()},
		{"snapshot", func() error { _, err := runtime.Snapshot(99); return err }()},
	} {
		if !errors.Is(operation.err, ErrUnknownDocument) {
			t.Fatalf("unknown %s = %v", operation.name, operation.err)
		}
	}

	handle, err := runtime.Create("writer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ApplyEditorDelta(handle, []richtext.EditorOperation{{Insert: strings.Repeat("a", runtime.MaxLocalEditBytes()+1)}}); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("overlong editor bytes = %v", err)
	}
	if _, err := runtime.ApplyEditorDelta(handle, []richtext.EditorOperation{{Insert: strings.Repeat("界", runtime.MaxLocalEditRunes()+1)}}); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("overlong editor runes = %v", err)
	}
	tooManyOperations := make([]richtext.EditorOperation, runtime.MaxLocalEditorOps()+1)
	if _, err := runtime.ApplyEditorDelta(handle, tooManyOperations); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("too many editor operations = %v", err)
	}
	tightEditorOptions := DefaultRichTextOptions()
	tightEditorOptions.MaxLocalEditBytes = 1
	if localEditorOperationsWithin([]richtext.EditorOperation{{Insert: "ok"}}, tightEditorOptions) ||
		!localEditorOperationsWithin([]richtext.EditorOperation{{Insert: "ok"}}, DefaultRichTextOptions()) {
		t.Fatal("local editor operation bounds")
	}

	encoded, err := runtime.ApplyEditorDelta(handle, []richtext.EditorOperation{{Insert: "safe"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ApplyDelta(handle, encoded); err != nil {
		t.Fatal(err)
	}
	wrongType, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDGCounterDelta, Payload: []byte{0}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ApplyDelta(handle, wrongType); !errors.Is(err, richtext.ErrInvalidDelta) {
		t.Fatalf("wrong rich-text delta type = %v", err)
	}

	saved, err := runtime.Snapshot(handle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Restore(RichTextSnapshot{State: wrongType}); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("wrong rich-text state type = %v", err)
	}
	overState := saved
	overState.State = make([]byte, runtime.MaxFrameBytes()+1)
	if _, err := runtime.Restore(overState); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("oversized state = %v", err)
	}
	overReplica := saved
	overReplica.Clock.ReplicaID = strings.Repeat("r", runtime.MaxStringBytes()+1)
	if _, err := runtime.Restore(overReplica); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("oversized clock replica = %v", err)
	}
	tightOptions := DefaultRichTextOptions()
	tightOptions.Decoder.MaxTags = 1
	tightRuntime, err := NewRichTextRuntime(tightOptions)
	if err != nil {
		t.Fatal(err)
	}
	overFrontier := saved
	overFrontier.Frontier["second"] = crdt.Tag{ReplicaID: "second", WallTime: 1}
	if _, err := tightRuntime.Restore(overFrontier); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("oversized frontier = %v", err)
	}

	exhausted, err := NewRichTextRuntime(DefaultRichTextOptions())
	if err != nil {
		t.Fatal(err)
	}
	exhausted.next = math.MaxUint64
	if _, err := exhausted.Create("exhausted"); !errors.Is(err, ErrHandleExhausted) {
		t.Fatalf("exhausted Create() = %v", err)
	}
	if _, err := exhausted.Restore(saved); !errors.Is(err, ErrHandleExhausted) {
		t.Fatalf("exhausted Restore() = %v", err)
	}
	if !runtime.Drop(handle) || runtime.Drop(handle) {
		t.Fatal("Drop() lifecycle")
	}
}
