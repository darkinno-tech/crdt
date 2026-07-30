// Package codecguard contains the panic boundary for application-provided
// element codecs. Codec implementations are extension code: a panic must not
// escape a CRDT encode or decode operation and terminate the host process.
package codecguard

import "errors"

// ErrPanic reports that an application-provided codec panicked. It deliberately
// does not retain or format the recovered value, which can contain application
// data and is not safe to expose from a library boundary.
var ErrPanic = errors.New("codec: panic")

// ID invokes an element codec identifier without allowing a panic to escape.
func ID(call func() string) (id string, err error) {
	defer func() {
		if recover() != nil {
			id, err = "", ErrPanic
		}
	}()
	return call(), nil
}

// Marshal invokes an element codec encoder without allowing a panic to escape.
func Marshal(call func() ([]byte, error)) (encoded []byte, err error) {
	defer func() {
		if recover() != nil {
			encoded, err = nil, ErrPanic
		}
	}()
	return call()
}

// Unmarshal invokes an element codec decoder without allowing a panic to
// escape.
func Unmarshal[T any](call func() (T, error)) (value T, err error) {
	defer func() {
		if recover() != nil {
			var zero T
			value, err = zero, ErrPanic
		}
	}()
	return call()
}
