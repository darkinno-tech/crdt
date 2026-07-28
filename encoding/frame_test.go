package encoding

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"reflect"
	"testing"
)

func TestFrameRoundTripAndRejectsTampering(t *testing.T) {
	t.Parallel()
	want := Frame{TypeID: 1, CodecID: "example.com/string/v1", Payload: []byte("state")}
	encoded, err := MarshalFrame(want)
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	got, err := UnmarshalFrame(encoded, DefaultLimits())
	if err != nil {
		t.Fatalf("UnmarshalFrame() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UnmarshalFrame() = %#v, want %#v", got, want)
	}
	encoded[5] ^= 1
	if _, err := UnmarshalFrame(encoded, DefaultLimits()); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("tampered frame error = %v", err)
	}
}

func TestFrameRejectsLimitsAndMalformedBytes(t *testing.T) {
	t.Parallel()
	encoded, err := MarshalFrame(Frame{TypeID: 1, Payload: []byte("x")})
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	if _, err := UnmarshalFrame(encoded, Limits{MaxFrameBytes: len(encoded), MaxPayload: 0, MaxCodecID: 1, MaxElements: 1, MaxTags: 1, MaxStringBytes: 1}); !errors.Is(err, ErrFrameLimit) {
		t.Fatalf("limit error = %v", err)
	}
	malformed := append([]byte(nil), encoded...)
	malformed = append(malformed[:4], append([]byte{0x81, 0x00}, malformed[5:]...)...)
	checksum := crc32.Checksum(malformed[4:len(malformed)-4], castagnoliTable)
	binary.BigEndian.PutUint32(malformed[len(malformed)-4:], checksum)
	if _, err := UnmarshalFrame(malformed, DefaultLimits()); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("non-canonical frame error = %v", err)
	}
}

func TestDefaultLimitsRoundTripLargestEncodablePayload(t *testing.T) {
	limits := DefaultLimits()
	payload := make([]byte, limits.MaxPayload)
	encoded, err := MarshalFrame(Frame{TypeID: ^uint64(0), CodecID: string(make([]byte, limits.MaxCodecID)), Payload: payload})
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	if len(encoded) > limits.MaxFrameBytes {
		t.Fatalf("encoded frame is %d bytes, limit is %d", len(encoded), limits.MaxFrameBytes)
	}
	if _, err := UnmarshalFrame(encoded, limits); err != nil {
		t.Fatalf("UnmarshalFrame() error = %v", err)
	}
	if _, err := MarshalFrame(Frame{TypeID: 1, Payload: make([]byte, limits.MaxPayload+1)}); !errors.Is(err, ErrFrameLimit) {
		t.Fatalf("oversized payload error = %v, want %v", err, ErrFrameLimit)
	}
}

func TestReadUvarintRejectsNonCanonicalEncoding(t *testing.T) {
	t.Parallel()
	if _, _, ok := ReadUvarint([]byte{0x81, 0x00}, 0); ok {
		t.Fatal("non-canonical uvarint was accepted")
	}
	if value, next, ok := ReadUvarint([]byte{0x81, 0x01}, 0); !ok || value != 129 || next != 2 {
		t.Fatalf("ReadUvarint() = %d, %d, %v", value, next, ok)
	}
}

func FuzzUnmarshalFrame(f *testing.F) {
	seed, err := MarshalFrame(Frame{TypeID: 1, Payload: []byte("seed")})
	if err != nil {
		f.Fatalf("MarshalFrame() error = %v", err)
	}
	f.Add(seed)
	f.Add([]byte("CRDT"))
	f.Fuzz(func(t *testing.T, data []byte) {
		decoded, err := UnmarshalFrame(data, DefaultLimits())
		if err == nil && decoded.TypeID == 0 {
			t.Fatal("successful frame decode returned an invalid type ID")
		}
	})
}
