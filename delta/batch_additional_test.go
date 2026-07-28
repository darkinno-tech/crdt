package delta

import (
	"errors"
	"testing"

	"github.com/darkinno/crdt"
	frame "github.com/darkinno/crdt/encoding"
	"github.com/darkinno/crdt/set"
)

func TestBatchDecoderRejectsMalformedAndMismatchedItems(t *testing.T) {
	t.Parallel()
	item := mustEncodedDelta(t, "a", 1)
	batch, err := NewBatch([][]byte{item}, len(item))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := batch.MarshalBinary(len(item))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "bad magic", data: []byte("nope")},
		{name: "trailing bytes", data: append(append([]byte(nil), encoded...), 1)},
		{name: "truncated item", data: encoded[:len(encoded)-1]},
		{name: "invalid item", data: append([]byte("DBAT\x01\x01x"), []byte{}...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := UnmarshalBatch(test.data, 1, len(item)+8); err == nil {
				t.Fatal("UnmarshalBatch accepted malformed data")
			}
		})
	}
	if _, err := UnmarshalBatch(encoded, 0, len(item)); !errors.Is(err, ErrLimit) {
		t.Fatalf("zero item limit error = %v", err)
	}
	if _, err := UnmarshalBatch(encoded, 1, 0); !errors.Is(err, ErrLimit) {
		t.Fatalf("zero byte limit error = %v", err)
	}

	state, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDGCounterState, Payload: []byte{0}})
	if err != nil {
		t.Fatal(err)
	}
	stateBatch := append([]byte("DBAT\x01"), frame.AppendUvarint(nil, uint64(len(state)))...)
	stateBatch = append(stateBatch, state...)
	if _, err := UnmarshalBatch(stateBatch, 1, len(state)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("state item error = %v", err)
	}
}

func TestCoalescerLimitsTypesAndFailurePaths(t *testing.T) {
	t.Parallel()
	var nilCoalescer *Coalescer
	if err := nilCoalescer.Add([]byte("x")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil Add() error = %v", err)
	}
	if items, bytes := nilCoalescer.Len(); items != 0 || bytes != 0 {
		t.Fatalf("nil Len() = %d, %d", items, bytes)
	}
	if len(nilCoalescer.Drain().Items()) != 0 {
		t.Fatal("nil Drain() was non-empty")
	}
	if _, err := NewCoalescer(0, 1, nil); !errors.Is(err, ErrLimit) {
		t.Fatalf("zero item limit error = %v", err)
	}
	if _, err := NewCoalescer(1, 0, nil); !errors.Is(err, ErrLimit) {
		t.Fatalf("zero byte limit error = %v", err)
	}

	first := mustEncodedDelta(t, "a", 1)
	queue, err := NewCoalescer(1, len(first), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Add([]byte("not a frame")); err == nil {
		t.Fatal("invalid frame accepted")
	}
	if err := queue.Add(first); err != nil {
		t.Fatal(err)
	}
	if err := queue.Add(first); !errors.Is(err, ErrLimit) {
		t.Fatalf("full queue error = %v", err)
	}

	orsetDelta := mustEncodedORSetDelta(t)
	typed, err := NewCoalescer(2, len(first)+len(orsetDelta), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := typed.Add(first); err != nil {
		t.Fatal(err)
	}
	if err := typed.Add(orsetDelta); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("type mismatch error = %v", err)
	}

	failing, err := NewCoalescer(2, len(first)*2, func(_, _ []byte) ([]byte, error) {
		return nil, errors.New("merge failed")
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := failing.Add(first); err != nil {
		t.Fatal(err)
	}
	if err := failing.Add(first); err == nil {
		t.Fatal("merge failure was accepted")
	}
	if items, _ := failing.Len(); items != 1 {
		t.Fatalf("failed merge changed queue length to %d", items)
	}
}

func mustEncodedORSetDelta(t *testing.T) []byte {
	t.Helper()
	codec := deltaStringCodec{}
	value, err := set.NewORSet("orset", codec)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := value.Add("x")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalBinary(codec)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type deltaStringCodec struct{}

func (deltaStringCodec) ID() string                            { return "example.com/delta-string/v1" }
func (deltaStringCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (deltaStringCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }
