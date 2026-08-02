//go:build js && wasm

// crdt-rga-wasm exposes the bounded RGA browser runtime through one small
// syscall/js surface. The default artifact uses compact run-v2 frames; legacy
// v1, packed-v3, and packed-v3 outer-v2 artifacts remain available for their
// respective manifest groups. It deliberately does not provide networking,
// identity, manifest negotiation, persistence, or authorization; applications
// own those boundaries before passing framed bytes to this module.
package main

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"syscall/js"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	clientwasm "github.com/DarkInno/crdt/internal/wasm"
	"github.com/DarkInno/crdt/richtext"
	"github.com/DarkInno/crdt/text"
)

const runtimeGlobalName = "__darkinnoCRDTRGA"

var (
	errInvalidArgument = errors.New("wasm: invalid JavaScript argument")
	callbacks          []js.Func
	wasmWireFormat     = "run-v2"
)

type method func([]js.Value) (any, error)

func main() {
	options, err := runtimeOptions()
	if err != nil {
		js.Global().Get("console").Call("error", "crdt RGA Wasm initialization failed")
		return
	}
	runtime, err := clientwasm.NewRuntime(options)
	if err != nil {
		js.Global().Get("console").Call("error", "crdt RGA Wasm initialization failed")
		return
	}
	richTextRuntime, err := clientwasm.NewRichTextRuntime(clientwasm.DefaultRichTextOptions())
	if err != nil {
		js.Global().Get("console").Call("error", "crdt rich-text Wasm initialization failed")
		return
	}

	api := newObject()
	register(api, "protocol", func([]js.Value) (any, error) {
		protocol := runtime.Protocol()
		value := newObject()
		value.Set("stateTypeID", strconv.FormatUint(protocol.StateTypeID, 10))
		value.Set("deltaTypeID", strconv.FormatUint(protocol.DeltaTypeID, 10))
		value.Set("semanticsVersion", strconv.FormatUint(protocol.SemanticsVersion, 10))
		value.Set("wireFormatVersion", strconv.FormatUint(protocol.WireFormatVersion, 10))
		value.Set("maxFrameBytes", runtime.MaxFrameBytes())
		value.Set("maxTags", runtime.MaxTags())
		value.Set("maxStringBytes", runtime.MaxStringBytes())
		value.Set("maxLocalEditBytes", runtime.MaxLocalEditBytes())
		value.Set("maxLocalEditRunes", runtime.MaxLocalEditRunes())
		return value, nil
	})
	register(api, "create", func(args []js.Value) (any, error) {
		if len(args) != 1 {
			return nil, errInvalidArgument
		}
		replicaID, err := requiredBoundedString(args[0], runtime.MaxStringBytes())
		if err != nil {
			return nil, err
		}
		return runtime.Create(replicaID)
	})
	register(api, "drop", func(args []js.Value) (any, error) {
		if len(args) != 1 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		return runtime.Drop(handle), nil
	})
	register(api, "insert", func(args []js.Value) (any, error) {
		if len(args) != 3 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		offset, err := requiredIndex(args[1])
		if err != nil {
			return nil, err
		}
		value, err := requiredBoundedString(args[2], runtime.MaxLocalEditBytes())
		if err != nil {
			return nil, err
		}
		encoded, err := runtime.Insert(handle, offset, value)
		if err != nil {
			return nil, err
		}
		return bytesToJS(encoded), nil
	})
	register(api, "delete", func(args []js.Value) (any, error) {
		if len(args) != 3 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		offset, err := requiredIndex(args[1])
		if err != nil {
			return nil, err
		}
		count, err := requiredIndex(args[2])
		if err != nil {
			return nil, err
		}
		encoded, err := runtime.Delete(handle, offset, count)
		if err != nil {
			return nil, err
		}
		return bytesToJS(encoded), nil
	})
	register(api, "replace", func(args []js.Value) (any, error) {
		if len(args) != 4 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		offset, err := requiredIndex(args[1])
		if err != nil {
			return nil, err
		}
		count, err := requiredIndex(args[2])
		if err != nil {
			return nil, err
		}
		value, err := requiredBoundedString(args[3], runtime.MaxLocalEditBytes())
		if err != nil {
			return nil, err
		}
		encoded, err := runtime.Replace(handle, offset, count, value)
		if err != nil {
			return nil, err
		}
		return bytesToJS(encoded), nil
	})
	register(api, "applyDelta", func(args []js.Value) (any, error) {
		if len(args) != 2 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		encoded, err := requiredBytes(args[1], runtime.MaxFrameBytes())
		if err != nil {
			return nil, err
		}
		if err := runtime.ApplyDelta(handle, encoded); err != nil {
			return nil, err
		}
		return nil, nil
	})
	register(api, "text", func(args []js.Value) (any, error) {
		if len(args) != 1 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		return runtime.Text(handle)
	})
	register(api, "anchorAt", func(args []js.Value) (any, error) {
		if len(args) != 2 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		offset, err := requiredIndex(args[1])
		if err != nil {
			return nil, err
		}
		anchor, err := runtime.AnchorAt(handle, offset)
		if err != nil {
			return nil, err
		}
		return anchorToJS(anchor), nil
	})
	register(api, "resolveAnchor", func(args []js.Value) (any, error) {
		if len(args) != 2 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		anchor, err := anchorFromJS(args[1], runtime.MaxStringBytes())
		if err != nil {
			return nil, err
		}
		return runtime.ResolveAnchor(handle, anchor)
	})
	register(api, "pendingCount", func(args []js.Value) (any, error) {
		if len(args) != 1 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		return runtime.PendingCount(handle)
	})
	register(api, "snapshot", func(args []js.Value) (any, error) {
		if len(args) != 1 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		saved, err := runtime.Snapshot(handle)
		if err != nil {
			return nil, err
		}
		return snapshotToJS(saved), nil
	})
	register(api, "restore", func(args []js.Value) (any, error) {
		if len(args) != 1 {
			return nil, errInvalidArgument
		}
		saved, err := snapshotFromJS(args[0], runtime.MaxFrameBytes(), runtime.MaxTags(), runtime.MaxStringBytes())
		if err != nil {
			return nil, err
		}
		return runtime.Restore(saved)
	})
	registerRichTextAPI(api, richTextRuntime)
	js.Global().Set(runtimeGlobalName, api)
	select {}
}

func runtimeOptions() (clientwasm.RGAOptions, error) {
	switch wasmWireFormat {
	case "run-v2":
		return clientwasm.DefaultRunRGAOptions(), nil
	case "v1":
		return clientwasm.DefaultRGAOptions(), nil
	case "packed-v3":
		return clientwasm.DefaultPackedRGAOptions(), nil
	case "packed-v3-v2":
		return clientwasm.DefaultPackedRGAFrameV2Options(), nil
	default:
		return clientwasm.RGAOptions{}, errors.New("wasm: unsupported build wire format")
	}
}

func registerRichTextAPI(api js.Value, runtime *clientwasm.RichTextRuntime) {
	register(api, "richTextProtocol", func([]js.Value) (any, error) {
		protocol := runtime.Protocol()
		value := newObject()
		value.Set("stateTypeID", strconv.FormatUint(protocol.StateTypeID, 10))
		value.Set("deltaTypeID", strconv.FormatUint(protocol.DeltaTypeID, 10))
		value.Set("semanticsVersion", strconv.FormatUint(protocol.SemanticsVersion, 10))
		value.Set("wireFormatVersion", strconv.FormatUint(frame.FormatVersion, 10))
		value.Set("maxFrameBytes", runtime.MaxFrameBytes())
		value.Set("maxTags", runtime.MaxTags())
		value.Set("maxStringBytes", runtime.MaxStringBytes())
		value.Set("maxLocalEditBytes", runtime.MaxLocalEditBytes())
		value.Set("maxLocalEditRunes", runtime.MaxLocalEditRunes())
		value.Set("maxLocalEditorOps", runtime.MaxLocalEditorOps())
		value.Set("maxAttributesPerOperation", runtime.MaxAttributesPerOperation())
		value.Set("maxAnchorBytes", runtime.MaxAnchorBytes())
		return value, nil
	})
	register(api, "richTextCreate", func(args []js.Value) (any, error) {
		if len(args) != 1 {
			return nil, errInvalidArgument
		}
		replicaID, err := requiredBoundedString(args[0], runtime.MaxStringBytes())
		if err != nil {
			return nil, err
		}
		return runtime.Create(replicaID)
	})
	register(api, "richTextDrop", func(args []js.Value) (any, error) {
		if len(args) != 1 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		return runtime.Drop(handle), nil
	})
	register(api, "richTextApplyEditorDelta", func(args []js.Value) (any, error) {
		if len(args) != 2 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		operations, err := editorOperationsFromJS(args[1], runtime.MaxLocalEditorOps(), runtime.MaxAttributesPerOperation(), runtime.MaxStringBytes())
		if err != nil {
			return nil, err
		}
		encoded, err := runtime.ApplyEditorDelta(handle, operations)
		if err != nil {
			return nil, err
		}
		return bytesToJS(encoded), nil
	})
	register(api, "richTextApplyDelta", func(args []js.Value) (any, error) {
		if len(args) != 2 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		encoded, err := requiredBytes(args[1], runtime.MaxFrameBytes())
		if err != nil {
			return nil, err
		}
		if err := runtime.ApplyDelta(handle, encoded); err != nil {
			return nil, err
		}
		return nil, nil
	})
	register(api, "richTextSpans", func(args []js.Value) (any, error) {
		if len(args) != 1 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		spans, err := runtime.Spans(handle)
		if err != nil {
			return nil, err
		}
		return spansToJS(spans), nil
	})
	register(api, "richTextAnchorAt", func(args []js.Value) (any, error) {
		if len(args) != 2 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		offset, err := requiredIndex(args[1])
		if err != nil {
			return nil, err
		}
		anchor, err := runtime.AnchorAt(handle, offset)
		if err != nil {
			return nil, err
		}
		return anchorToJS(anchor), nil
	})
	register(api, "richTextResolveAnchor", func(args []js.Value) (any, error) {
		if len(args) != 2 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		anchor, err := anchorFromJS(args[1], runtime.MaxStringBytes())
		if err != nil {
			return nil, err
		}
		return runtime.ResolveAnchor(handle, anchor)
	})
	register(api, "richTextAnchorRangeAt", func(args []js.Value) (any, error) {
		if len(args) != 3 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		start, err := requiredIndex(args[1])
		if err != nil {
			return nil, err
		}
		end, err := requiredIndex(args[2])
		if err != nil {
			return nil, err
		}
		anchors, err := runtime.AnchorRangeAt(handle, start, end)
		if err != nil {
			return nil, err
		}
		return anchorRangeToJS(anchors), nil
	})
	register(api, "richTextResolveAnchorRange", func(args []js.Value) (any, error) {
		if len(args) != 2 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		anchors, err := anchorRangeFromJS(args[1], runtime.MaxStringBytes())
		if err != nil {
			return nil, err
		}
		start, end, err := runtime.ResolveAnchorRange(handle, anchors)
		if err != nil {
			return nil, err
		}
		value := newObject()
		value.Set("start", start)
		value.Set("end", end)
		return value, nil
	})
	register(api, "richTextMarshalAnchor", func(args []js.Value) (any, error) {
		if len(args) != 1 {
			return nil, errInvalidArgument
		}
		anchor, err := anchorFromJS(args[0], runtime.MaxStringBytes())
		if err != nil {
			return nil, err
		}
		encoded, err := runtime.MarshalAnchor(anchor)
		if err != nil {
			return nil, err
		}
		return bytesToJS(encoded), nil
	})
	register(api, "richTextUnmarshalAnchor", func(args []js.Value) (any, error) {
		if len(args) != 1 {
			return nil, errInvalidArgument
		}
		encoded, err := requiredBytes(args[0], runtime.MaxAnchorBytes())
		if err != nil {
			return nil, err
		}
		anchor, err := runtime.UnmarshalAnchor(encoded)
		if err != nil {
			return nil, err
		}
		return anchorToJS(anchor), nil
	})
	register(api, "richTextMarshalAnchorRange", func(args []js.Value) (any, error) {
		if len(args) != 1 {
			return nil, errInvalidArgument
		}
		anchors, err := anchorRangeFromJS(args[0], runtime.MaxStringBytes())
		if err != nil {
			return nil, err
		}
		encoded, err := runtime.MarshalAnchorRange(anchors)
		if err != nil {
			return nil, err
		}
		return bytesToJS(encoded), nil
	})
	register(api, "richTextUnmarshalAnchorRange", func(args []js.Value) (any, error) {
		if len(args) != 1 {
			return nil, errInvalidArgument
		}
		encoded, err := requiredBytes(args[0], runtime.MaxAnchorBytes())
		if err != nil {
			return nil, err
		}
		anchors, err := runtime.UnmarshalAnchorRange(encoded)
		if err != nil {
			return nil, err
		}
		return anchorRangeToJS(anchors), nil
	})
	register(api, "richTextSnapshot", func(args []js.Value) (any, error) {
		if len(args) != 1 {
			return nil, errInvalidArgument
		}
		handle, err := requiredHandle(args[0])
		if err != nil {
			return nil, err
		}
		saved, err := runtime.Snapshot(handle)
		if err != nil {
			return nil, err
		}
		return snapshotToJS(clientwasm.RGASnapshot{State: saved.State, Frontier: saved.Frontier, Clock: saved.Clock}), nil
	})
	register(api, "richTextRestore", func(args []js.Value) (any, error) {
		if len(args) != 1 {
			return nil, errInvalidArgument
		}
		saved, err := snapshotFromJS(args[0], runtime.MaxFrameBytes(), runtime.MaxTags(), runtime.MaxStringBytes())
		if err != nil {
			return nil, err
		}
		return runtime.Restore(clientwasm.RichTextSnapshot{State: saved.State, Frontier: saved.Frontier, Clock: saved.Clock})
	})
}

func editorOperationsFromJS(value js.Value, maxOperations, maxAttributes, maxStringBytes int) ([]richtext.EditorOperation, error) {
	if !js.Global().Get("Array").Call("isArray", value).Bool() {
		return nil, errInvalidArgument
	}
	length, err := requiredBoundedLength(value.Get("length"), maxOperations)
	if err != nil {
		return nil, err
	}
	operations := make([]richtext.EditorOperation, 0, length)
	for index := 0; index < length; index++ {
		item := value.Index(index)
		if item.Type() != js.TypeObject || item.IsNull() || item.IsUndefined() {
			return nil, errInvalidArgument
		}
		retain, retainPresent, err := optionalIndex(item.Get("retain"))
		if err != nil {
			return nil, err
		}
		deleted, deletePresent, err := optionalIndex(item.Get("delete"))
		if err != nil {
			return nil, err
		}
		insert, insertPresent, err := optionalBoundedString(item.Get("insert"), maxStringBytes)
		if err != nil {
			return nil, err
		}
		if boolToInt(retainPresent && retain > 0)+boolToInt(deletePresent && deleted > 0)+boolToInt(insertPresent && insert != "") != 1 {
			return nil, errInvalidArgument
		}
		changes, err := attributeChangesFromJS(item.Get("changes"), maxAttributes, maxStringBytes)
		if err != nil {
			return nil, err
		}
		operations = append(operations, richtext.EditorOperation{Retain: retain, Delete: deleted, Insert: insert, Changes: changes})
	}
	return operations, nil
}

func attributeChangesFromJS(value js.Value, maxAttributes, maxStringBytes int) ([]richtext.AttributeChange, error) {
	if value.IsUndefined() {
		return nil, nil
	}
	if !js.Global().Get("Array").Call("isArray", value).Bool() {
		return nil, errInvalidArgument
	}
	length, err := requiredBoundedLength(value.Get("length"), maxAttributes)
	if err != nil {
		return nil, err
	}
	changes := make([]richtext.AttributeChange, 0, length)
	for index := 0; index < length; index++ {
		item := value.Index(index)
		if item.Type() != js.TypeObject || item.IsNull() || item.IsUndefined() {
			return nil, errInvalidArgument
		}
		key, err := requiredBoundedString(item.Get("key"), maxStringBytes)
		if err != nil {
			return nil, err
		}
		removeValue := item.Get("remove")
		remove := false
		if !removeValue.IsUndefined() {
			if removeValue.Type() != js.TypeBoolean {
				return nil, errInvalidArgument
			}
			remove = removeValue.Bool()
		}
		attribute := richtext.AttributeChange{Key: key, Remove: remove}
		if !remove {
			attribute.Value, err = requiredBoundedString(item.Get("value"), maxStringBytes)
			if err != nil {
				return nil, err
			}
		} else if value := item.Get("value"); !value.IsUndefined() {
			return nil, errInvalidArgument
		}
		changes = append(changes, attribute)
	}
	return changes, nil
}

func spansToJS(spans []richtext.Span) js.Value {
	result := js.Global().Get("Array").New(len(spans))
	for index, span := range spans {
		value := newObject()
		value.Set("text", span.Text)
		attributes := newObject()
		keys := make([]string, 0, len(span.Attributes))
		for key := range span.Attributes {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			attributes.Set(key, span.Attributes[key])
		}
		value.Set("attributes", attributes)
		result.SetIndex(index, value)
	}
	return result
}

func optionalIndex(value js.Value) (int, bool, error) {
	if value.IsUndefined() {
		return 0, false, nil
	}
	index, err := requiredIndex(value)
	return index, true, err
}

func optionalBoundedString(value js.Value, maxBytes int) (string, bool, error) {
	if value.IsUndefined() {
		return "", false, nil
	}
	text, err := requiredBoundedString(value, maxBytes)
	return text, true, err
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func register(api js.Value, name string, invoke method) {
	callback := js.FuncOf(func(_ js.Value, args []js.Value) (result any) {
		defer func() {
			if recover() != nil {
				result = failure("operation_failed")
			}
		}()
		value, err := invoke(args)
		if err != nil {
			return failure(errorCode(err))
		}
		return success(value)
	})
	callbacks = append(callbacks, callback)
	api.Set(name, callback)
}

func success(value any) js.Value {
	result := newObject()
	result.Set("ok", true)
	if value != nil {
		result.Set("value", value)
	}
	return result
}

func failure(code string) js.Value {
	result := newObject()
	result.Set("ok", false)
	result.Set("error", code)
	return result
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, errInvalidArgument):
		return "invalid_argument"
	case errors.Is(err, clientwasm.ErrUnknownDocument):
		return "unknown_document"
	case errors.Is(err, clientwasm.ErrHandleExhausted):
		return "handle_exhausted"
	case errors.Is(err, frame.ErrFrameLimit), errors.Is(err, text.ErrResourceLimit), errors.Is(err, richtext.ErrResourceLimit):
		return "resource_limit"
	case errors.Is(err, frame.ErrInvalidFrame), errors.Is(err, text.ErrInvalidDelta), errors.Is(err, text.ErrInvalidText), errors.Is(err, richtext.ErrInvalidDelta), errors.Is(err, richtext.ErrInvalidAttribute):
		return "invalid_frame"
	case errors.Is(err, text.ErrRange):
		return "range_error"
	case errors.Is(err, text.ErrInvalidAnchor):
		return "invalid_anchor"
	case errors.Is(err, text.ErrAnchorGone):
		return "anchor_gone"
	case errors.Is(err, text.ErrIncompleteState):
		return "incomplete_state"
	default:
		return "operation_failed"
	}
}

func requiredString(value js.Value) (string, error) {
	if value.Type() != js.TypeString {
		return "", errInvalidArgument
	}
	return value.String(), nil
}

func requiredHandle(value js.Value) (uint64, error) {
	if value.Type() != js.TypeNumber {
		return 0, errInvalidArgument
	}
	number := value.Float()
	if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number || number <= 0 || number > (1<<53-1) {
		return 0, errInvalidArgument
	}
	return uint64(number), nil
}

func requiredIndex(value js.Value) (int, error) {
	if value.Type() != js.TypeNumber {
		return 0, errInvalidArgument
	}
	number := value.Float()
	maxInt := float64(int(^uint(0) >> 1))
	if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number || number < 0 || number > maxInt {
		return 0, errInvalidArgument
	}
	return int(number), nil
}

func requiredBytes(value js.Value, maxBytes int) ([]byte, error) {
	if value.Type() != js.TypeObject || value.IsNull() || value.IsUndefined() || !value.InstanceOf(js.Global().Get("Uint8Array")) {
		return nil, errInvalidArgument
	}
	length, err := requiredBoundedLength(value.Get("byteLength"), maxBytes)
	if err != nil {
		return nil, err
	}
	result := make([]byte, length)
	if copied := js.CopyBytesToGo(result, value); copied != len(result) {
		return nil, errInvalidArgument
	}
	return result, nil
}

func requiredBoundedLength(value js.Value, max int) (int, error) {
	if value.Type() != js.TypeNumber || max < 0 {
		return 0, errInvalidArgument
	}
	number := value.Float()
	if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number || number < 0 || number > float64(max) {
		return 0, errInvalidArgument
	}
	return int(number), nil
}

func bytesToJS(data []byte) js.Value {
	result := js.Global().Get("Uint8Array").New(len(data))
	js.CopyBytesToJS(result, data)
	return result
}

func anchorToJS(anchor text.Anchor) js.Value {
	result := newObject()
	switch anchor.Association {
	case text.AnchorBefore:
		result.Set("association", "before")
	case text.AnchorAfter:
		result.Set("association", "after")
	}
	if !anchor.Position.Valid() {
		result.Set("position", js.Null())
		return result
	}
	position := newObject()
	position.Set("replicaID", anchor.Position.ReplicaID)
	position.Set("wallTime", strconv.FormatUint(anchor.Position.WallTime, 10))
	position.Set("logical", strconv.FormatUint(anchor.Position.Logical, 10))
	result.Set("position", position)
	return result
}

func anchorFromJS(value js.Value, maxStringBytes int) (text.Anchor, error) {
	if value.Type() != js.TypeObject || value.IsNull() || value.IsUndefined() {
		return text.Anchor{}, errInvalidArgument
	}
	var association text.AnchorAssociation
	switch value.Get("association").String() {
	case "before":
		association = text.AnchorBefore
	case "after":
		association = text.AnchorAfter
	default:
		return text.Anchor{}, errInvalidArgument
	}
	positionValue := value.Get("position")
	if positionValue.IsNull() || positionValue.IsUndefined() {
		return text.Anchor{Association: association}, nil
	}
	if positionValue.Type() != js.TypeObject {
		return text.Anchor{}, errInvalidArgument
	}
	replicaID, err := requiredBoundedString(positionValue.Get("replicaID"), maxStringBytes)
	if err != nil {
		return text.Anchor{}, err
	}
	wallTime, err := requiredUint64(positionValue.Get("wallTime"))
	if err != nil {
		return text.Anchor{}, err
	}
	logical, err := requiredUint64(positionValue.Get("logical"))
	if err != nil {
		return text.Anchor{}, err
	}
	position := text.Position{ReplicaID: replicaID, WallTime: wallTime, Logical: logical}
	if !position.Valid() {
		return text.Anchor{}, errInvalidArgument
	}
	return text.Anchor{Position: position, Association: association}, nil
}

func anchorRangeToJS(anchors text.AnchorRange) js.Value {
	result := newObject()
	result.Set("start", anchorToJS(anchors.Start))
	result.Set("end", anchorToJS(anchors.End))
	return result
}

func anchorRangeFromJS(value js.Value, maxStringBytes int) (text.AnchorRange, error) {
	if value.Type() != js.TypeObject || value.IsNull() || value.IsUndefined() {
		return text.AnchorRange{}, errInvalidArgument
	}
	start, err := anchorFromJS(value.Get("start"), maxStringBytes)
	if err != nil {
		return text.AnchorRange{}, err
	}
	end, err := anchorFromJS(value.Get("end"), maxStringBytes)
	if err != nil {
		return text.AnchorRange{}, err
	}
	return text.AnchorRange{Start: start, End: end}, nil
}

func snapshotToJS(saved clientwasm.RGASnapshot) js.Value {
	result := newObject()
	result.Set("state", bytesToJS(saved.State))
	clockValue := newObject()
	clockValue.Set("replicaID", saved.Clock.ReplicaID)
	clockValue.Set("wallTime", strconv.FormatUint(saved.Clock.WallTime, 10))
	clockValue.Set("logical", strconv.FormatUint(saved.Clock.Logical, 10))
	result.Set("clock", clockValue)

	keys := make([]string, 0, len(saved.Frontier))
	for replicaID := range saved.Frontier {
		keys = append(keys, replicaID)
	}
	sort.Strings(keys)
	frontier := js.Global().Get("Array").New(len(keys))
	for index, replicaID := range keys {
		tag := saved.Frontier[replicaID]
		value := newObject()
		value.Set("replicaID", tag.ReplicaID)
		value.Set("wallTime", strconv.FormatUint(tag.WallTime, 10))
		value.Set("logical", strconv.FormatUint(tag.Logical, 10))
		frontier.SetIndex(index, value)
	}
	result.Set("frontier", frontier)
	return result
}

func snapshotFromJS(value js.Value, maxFrameBytes, maxTags, maxStringBytes int) (clientwasm.RGASnapshot, error) {
	if value.Type() != js.TypeObject || value.IsNull() || value.IsUndefined() {
		return clientwasm.RGASnapshot{}, errInvalidArgument
	}
	state, err := requiredBytes(value.Get("state"), maxFrameBytes)
	if err != nil {
		return clientwasm.RGASnapshot{}, err
	}
	clockValue := value.Get("clock")
	if clockValue.Type() != js.TypeObject || clockValue.IsNull() || clockValue.IsUndefined() {
		return clientwasm.RGASnapshot{}, errInvalidArgument
	}
	clockReplicaID, err := requiredBoundedString(clockValue.Get("replicaID"), maxStringBytes)
	if err != nil {
		return clientwasm.RGASnapshot{}, err
	}
	clockWallTime, err := requiredUint64(clockValue.Get("wallTime"))
	if err != nil {
		return clientwasm.RGASnapshot{}, err
	}
	clockLogical, err := requiredUint64(clockValue.Get("logical"))
	if err != nil {
		return clientwasm.RGASnapshot{}, err
	}

	frontierValue := value.Get("frontier")
	if !js.Global().Get("Array").Call("isArray", frontierValue).Bool() {
		return clientwasm.RGASnapshot{}, errInvalidArgument
	}
	length, err := requiredBoundedLength(frontierValue.Get("length"), maxTags)
	if err != nil {
		return clientwasm.RGASnapshot{}, err
	}
	frontier := make(map[string]crdt.Tag, length)
	for index := 0; index < length; index++ {
		item := frontierValue.Index(index)
		if item.Type() != js.TypeObject || item.IsNull() || item.IsUndefined() {
			return clientwasm.RGASnapshot{}, errInvalidArgument
		}
		replicaID, err := requiredBoundedString(item.Get("replicaID"), maxStringBytes)
		if err != nil {
			return clientwasm.RGASnapshot{}, err
		}
		wallTime, err := requiredUint64(item.Get("wallTime"))
		if err != nil {
			return clientwasm.RGASnapshot{}, err
		}
		logical, err := requiredUint64(item.Get("logical"))
		if err != nil {
			return clientwasm.RGASnapshot{}, err
		}
		tag := crdt.Tag{ReplicaID: replicaID, WallTime: wallTime, Logical: logical}
		if !tag.Valid() {
			return clientwasm.RGASnapshot{}, errInvalidArgument
		}
		if _, exists := frontier[replicaID]; exists {
			return clientwasm.RGASnapshot{}, errInvalidArgument
		}
		frontier[replicaID] = tag
	}
	return clientwasm.RGASnapshot{
		State:    state,
		Frontier: frontier,
		Clock:    clock.State{ReplicaID: clockReplicaID, WallTime: clockWallTime, Logical: clockLogical},
	}, nil
}

func requiredUint64(value js.Value) (uint64, error) {
	text, err := requiredString(value)
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, errInvalidArgument
	}
	return parsed, nil
}

func requiredBoundedString(value js.Value, maxBytes int) (string, error) {
	text, err := requiredString(value)
	if err != nil {
		return "", err
	}
	if len(text) > maxBytes {
		return "", frame.ErrFrameLimit
	}
	return text, nil
}

func newObject() js.Value { return js.Global().Get("Object").New() }
