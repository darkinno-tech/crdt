package set

import (
	"reflect"
	"strings"
)

// boundElementCodec is a validated codec and its stable wire identifier.
//
// A non-nil interface can still hold a nil pointer. Keep that distinction at
// the public boundary, then retain the identifier with the stateful set so
// encode, decode, and merge paths do not repeat reflection or codec.ID calls.
type boundElementCodec[T comparable] struct {
	value ElementCodec[T]
	id    string
}

func bindElementCodec[T comparable](codec ElementCodec[T]) (boundElementCodec[T], error) {
	if isNilCodec(codec) {
		return boundElementCodec[T]{}, ErrInvalidCodec
	}
	id := codec.ID()
	if strings.TrimSpace(id) == "" {
		return boundElementCodec[T]{}, ErrInvalidCodec
	}
	return boundElementCodec[T]{value: codec, id: id}, nil
}

func isNilCodec[T comparable](codec ElementCodec[T]) bool {
	if codec == nil {
		return true
	}
	value := reflect.ValueOf(codec)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
