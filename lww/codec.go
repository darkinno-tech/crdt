package lww

import (
	"strings"

	"github.com/im10furry/crdt/internal/codecguard"
)

// boundElementCodec captures the stable ID after one protected call at the
// public boundary. It prevents a stateful codec from changing IDs midway
// through a single encode or decode operation.
type boundElementCodec[T comparable] struct {
	value ElementCodec[T]
	id    string
}

func bindElementCodec[T comparable](codec ElementCodec[T]) (boundElementCodec[T], error) {
	if isNilSetCodec(codec) {
		return boundElementCodec[T]{}, ErrInvalidCodec
	}
	id, err := codecguard.ID(codec.ID)
	if err != nil || strings.TrimSpace(id) == "" {
		return boundElementCodec[T]{}, ErrInvalidCodec
	}
	return boundElementCodec[T]{value: codec, id: id}, nil
}

func (codec boundElementCodec[T]) marshal(value T) ([]byte, error) {
	return codecguard.Marshal(func() ([]byte, error) { return codec.value.Marshal(value) })
}

func (codec boundElementCodec[T]) unmarshal(data []byte) (T, error) {
	return codecguard.Unmarshal(func() (T, error) { return codec.value.Unmarshal(data) })
}
