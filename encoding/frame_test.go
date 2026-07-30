package encoding

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"reflect"
	"strings"
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

func TestMarshalFrameWithPayloadMatchesFrameAndRejectsWriterFailures(t *testing.T) {
	t.Parallel()
	payload := []byte("state")
	want, err := MarshalFrame(Frame{TypeID: 1, CodecID: "example.com/string/v1", Payload: payload})
	if err != nil {
		t.Fatalf("MarshalFrame() error = %v", err)
	}
	got, err := MarshalFrameWithPayload(1, "example.com/string/v1", len(payload), func(destination []byte) error {
		copy(destination, payload)
		return nil
	})
	if err != nil {
		t.Fatalf("MarshalFrameWithPayload() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MarshalFrameWithPayload() = %x, want %x", got, want)
	}

	writerErr := errors.New("writer failed")
	if _, err := MarshalFrameWithPayload(1, "", 1, func([]byte) error { return writerErr }); !errors.Is(err, writerErr) {
		t.Fatalf("writer error = %v, want %v", err, writerErr)
	}
	if _, err := MarshalFrameWithPayload(1, "", 1, nil); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("nil writer error = %v, want %v", err, ErrInvalidFrame)
	}
}

func TestUnmarshalFrameViewBorrowsPayloadWhileUnmarshalFrameOwnsIt(t *testing.T) {
	encoded, err := MarshalFrame(Frame{TypeID: 7, Payload: []byte("state")})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := UnmarshalFrame(encoded, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	view, err := UnmarshalFrameView(encoded, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if string(view.Payload) != "state" {
		t.Fatalf("view payload = %q", view.Payload)
	}
	view.Payload[0] = 'S'
	if got := encoded[len(encoded)-4-len(view.Payload)]; got != 'S' {
		t.Fatalf("view did not borrow input: got %q", got)
	}

	owned.Payload[0] = 'X'
	if got := encoded[len(encoded)-4-len(owned.Payload)]; got != 'S' {
		t.Fatalf("owned payload modified input: got %q", got)
	}
}

func TestMarshalFrameWithPayloadAndLimitsHonorsWholeFrameBudget(t *testing.T) {
	t.Parallel()
	payload := []byte("state")
	limits := DefaultLimits()
	encoded, err := MarshalFrameWithPayloadAndLimits(1, "", len(payload), limits, func(destination []byte) error {
		copy(destination, payload)
		return nil
	})
	if err != nil {
		t.Fatalf("MarshalFrameWithPayloadAndLimits() error = %v", err)
	}
	if _, err := UnmarshalFrame(encoded, limits); err != nil {
		t.Fatalf("UnmarshalFrame() error = %v", err)
	}

	tight := limits
	tight.MaxFrameBytes = len(encoded) - 1
	if _, err := MarshalFrameWithPayloadAndLimits(1, "", len(payload), tight, func([]byte) error { return nil }); !errors.Is(err, ErrFrameLimit) {
		t.Fatalf("whole-frame limit error = %v, want %v", err, ErrFrameLimit)
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

func TestFrameV2CompressesAndRoundTripsWithoutChangingPayload(t *testing.T) {
	t.Parallel()
	want := Frame{TypeID: 7, CodecID: "example.com/repeated-text/v1", Payload: []byte(strings.Repeat("same author and same value; ", 1_024))}
	v1, err := MarshalFrame(want)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := MarshalFrameV2(want)
	if err != nil {
		t.Fatal(err)
	}
	if len(v2) >= len(v1) {
		t.Fatalf("v2 frame length = %d, want less than v1 length %d", len(v2), len(v1))
	}
	decoded, err := UnmarshalFrame(v2, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Version() != FormatVersionV2 || decoded.TypeID != want.TypeID || decoded.CodecID != want.CodecID || !reflect.DeepEqual(decoded.Payload, want.Payload) {
		t.Fatalf("v2 decode = %#v (version %d), want payload %d bytes", decoded, decoded.Version(), len(want.Payload))
	}

	legacy, err := ConvertFrameV2ToV1(v2, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacy, v1) {
		t.Fatal("v2 to v1 conversion changed canonical v1 frame")
	}
	reencoded, err := ConvertFrameV1ToV2(v1, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	converted, err := UnmarshalFrame(reencoded, DefaultLimits())
	if err != nil || converted.Version() != FormatVersionV2 || !reflect.DeepEqual(converted.Payload, want.Payload) {
		t.Fatalf("v1 to v2 conversion = %#v, %v", converted, err)
	}
}

func TestFrameV2BoundsInflationAndRejectsWrongConversionDirection(t *testing.T) {
	t.Parallel()
	encoded, err := MarshalFrameV2(Frame{TypeID: 1, Payload: []byte(strings.Repeat("x", 8<<10))})
	if err != nil {
		t.Fatal(err)
	}
	tight := DefaultLimits()
	tight.MaxPayload = 64
	if _, err := UnmarshalFrame(encoded, tight); !errors.Is(err, ErrFrameLimit) {
		t.Fatalf("bounded v2 decode error = %v, want ErrFrameLimit", err)
	}
	if _, err := ConvertFrameV1ToV2(encoded, DefaultLimits()); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("v2 as v1 conversion error = %v, want ErrInvalidFrame", err)
	}

	v1, err := MarshalFrame(Frame{TypeID: 1, Payload: []byte("legacy")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertFrameV2ToV1(v1, DefaultLimits()); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("v1 as v2 conversion error = %v, want ErrInvalidFrame", err)
	}
}

func FuzzUnmarshalFrame(f *testing.F) {
	seed, err := MarshalFrame(Frame{TypeID: 1, Payload: []byte("seed")})
	if err != nil {
		f.Fatalf("MarshalFrame() error = %v", err)
	}
	f.Add(seed)
	v2Seed, err := MarshalFrameV2(Frame{TypeID: 1, Payload: []byte(strings.Repeat("fuzz", 64))})
	if err != nil {
		f.Fatalf("MarshalFrameV2: %v", err)
	}
	f.Add(v2Seed)
	f.Add([]byte("CRDT"))
	f.Fuzz(func(t *testing.T, data []byte) {
		decoded, err := UnmarshalFrame(data, DefaultLimits())
		if err == nil && (decoded.TypeID == 0 || (decoded.Version() != FormatVersion && decoded.Version() != FormatVersionV2)) {
			t.Fatal("successful frame decode returned invalid metadata")
		}
	})
}
