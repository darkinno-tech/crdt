package encoding

import (
	"errors"
	"reflect"
	"testing"

	"github.com/darkinno/crdt"
)

func TestUvarintAndBytesHelpersValidateOffsetsAndBounds(t *testing.T) {
	t.Parallel()
	encoded := AppendUvarint(nil, 3)
	encoded = append(encoded, "abc"...)
	value, next, ok := ReadUvarint(encoded, 0)
	if !ok || value != 3 || next != 1 {
		t.Fatalf("ReadUvarint() = %d, %d, %v", value, next, ok)
	}
	bytes, next, ok := ReadBytes(encoded, 0, 3)
	if !ok || next != len(encoded) || !reflect.DeepEqual(bytes, []byte("abc")) {
		t.Fatalf("ReadBytes() = %q, %d, %v", bytes, next, ok)
	}
	for _, position := range []int{-1, len(encoded)} {
		if _, _, ok := ReadUvarint(encoded, position); ok {
			t.Fatalf("ReadUvarint accepted position %d", position)
		}
		if _, _, ok := ReadBytes(encoded, position, 3); ok {
			t.Fatalf("ReadBytes accepted position %d", position)
		}
	}
	if _, _, ok := ReadBytes(encoded, 0, 2); ok {
		t.Fatal("ReadBytes accepted an over-limit value")
	}
	if _, _, ok := ReadBytes(encoded[:1], 0, 3); ok {
		t.Fatal("ReadBytes accepted a truncated value")
	}
}

func TestMarshalFrameRejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	if _, err := MarshalFrame(Frame{}); !errors.Is(err, ErrFrameLimit) {
		t.Fatalf("zero type ID error = %v", err)
	}
	limits := DefaultLimits()
	if _, err := MarshalFrame(Frame{TypeID: 1, CodecID: string(make([]byte, limits.MaxCodecID+1))}); !errors.Is(err, ErrFrameLimit) {
		t.Fatalf("overlong codec ID error = %v", err)
	}
}

func TestTagHelpersAreCanonicalAndBounded(t *testing.T) {
	tag := crdt.Tag{ReplicaID: "replica", WallTime: 7, Logical: 3}
	encoded := AppendTag(nil, tag)
	if got := TagSize(tag); got != len(encoded) {
		t.Fatalf("TagSize() = %d, want %d", got, len(encoded))
	}
	decoded, next, ok := ReadTag(encoded, 0, len(tag.ReplicaID))
	if !ok || next != len(encoded) || decoded != tag {
		t.Fatalf("ReadTag() = %#v, %d, %v", decoded, next, ok)
	}
	if _, _, ok := ReadTag(encoded, 0, len(tag.ReplicaID)-1); ok {
		t.Fatal("ReadTag accepted an over-limit replica ID")
	}
	if _, _, ok := ReadTag([]byte{1, 'a'}, 0, 1); ok {
		t.Fatal("ReadTag accepted a tag without a wall time")
	}
	if _, _, ok := ReadTag(encoded[:len(encoded)-1], 0, len(tag.ReplicaID)); ok {
		t.Fatal("ReadTag accepted a tag without a logical time")
	}
	if _, _, ok := ReadTag([]byte{0, 0, 0}, 0, 1); ok {
		t.Fatal("ReadTag accepted an empty replica ID")
	}
	if got := UvarintSize(1 << 63); got != 10 {
		t.Fatalf("UvarintSize() = %d, want 10", got)
	}
}
