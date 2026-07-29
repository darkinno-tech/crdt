//go:build js && wasm

// crdt-rga-wasm exposes the bounded RGA browser runtime through one small
// syscall/js surface. The default artifact uses compact run-v2 frames; an
// explicitly built legacy v1 artifact remains available for migration. It
// deliberately does not provide networking, identity, manifest negotiation,
// persistence, or authorization; applications own those boundaries before
// passing framed bytes to this module.
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

	api := newObject()
	register(api, "protocol", func([]js.Value) (any, error) {
		protocol := runtime.Protocol()
		value := newObject()
		value.Set("stateTypeID", strconv.FormatUint(protocol.StateTypeID, 10))
		value.Set("deltaTypeID", strconv.FormatUint(protocol.DeltaTypeID, 10))
		value.Set("semanticsVersion", strconv.FormatUint(protocol.SemanticsVersion, 10))
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
	js.Global().Set(runtimeGlobalName, api)
	select {}
}

func runtimeOptions() (clientwasm.RGAOptions, error) {
	switch wasmWireFormat {
	case "run-v2":
		return clientwasm.DefaultRunRGAOptions(), nil
	case "v1":
		return clientwasm.DefaultRGAOptions(), nil
	default:
		return clientwasm.RGAOptions{}, errors.New("wasm: unsupported build wire format")
	}
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
	case errors.Is(err, frame.ErrFrameLimit), errors.Is(err, text.ErrResourceLimit):
		return "resource_limit"
	case errors.Is(err, frame.ErrInvalidFrame), errors.Is(err, text.ErrInvalidDelta), errors.Is(err, text.ErrInvalidText):
		return "invalid_frame"
	case errors.Is(err, text.ErrRange):
		return "range_error"
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
