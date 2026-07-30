// Package encoding provides canonical, bounded binary frames for CRDT state.
package encoding

import (
	"encoding/binary"
	"errors"
	"hash/crc32"

	"github.com/DarkInno/crdt"
)

// FormatVersion identifies the canonical frame layout implemented by this
// package. Unknown versions are rejected rather than guessed.
const FormatVersion uint64 = 1

const (
	defaultMaxFrameBytes = 16 << 20
	defaultMaxCodecID    = 256
	// maxFrameOverhead reserves enough space for the magic, four maximum-size
	// uvarints, the largest accepted codec ID, and the checksum. Keeping this
	// reservation in the default payload limit guarantees that every frame
	// accepted by MarshalFrame is accepted by UnmarshalFrame with DefaultLimits.
	maxFrameOverhead = 4 + 4*binary.MaxVarintLen64 + defaultMaxCodecID + 4
)

var (
	ErrInvalidFrame = errors.New("encoding: invalid frame")
	ErrFrameLimit   = errors.New("encoding: frame limit exceeded")

	castagnoliTable = crc32.MakeTable(crc32.Castagnoli)
)

// DecoderLimits bounds decoder allocation and input work. All limits must be
// positive. MaxElements and MaxTags apply to a single decoded payload.
type DecoderLimits struct {
	MaxFrameBytes  int
	MaxPayload     int
	MaxCodecID     int
	MaxElements    int
	MaxTags        int
	MaxStringBytes int
}

// Limits is retained as a short name for DecoderLimits.
type Limits = DecoderLimits

// DefaultLimits returns conservative bounds for in-memory library use. It
// returns a value rather than exposing mutable process-wide configuration.
func DefaultLimits() DecoderLimits {
	return DecoderLimits{
		MaxFrameBytes:  defaultMaxFrameBytes,
		MaxPayload:     defaultMaxFrameBytes - maxFrameOverhead,
		MaxCodecID:     defaultMaxCodecID,
		MaxElements:    1 << 20,
		MaxTags:        1 << 20,
		MaxStringBytes: 1 << 20,
	}
}

// Frame is the versioned outer envelope of a CRDT state or delta payload.
type Frame struct {
	TypeID  uint64
	CodecID string
	Payload []byte
}

// MarshalFrame returns the canonical v1 encoding of frame.
func MarshalFrame(frame Frame) ([]byte, error) {
	return MarshalFrameWithPayload(frame.TypeID, frame.CodecID, len(frame.Payload), func(payload []byte) error {
		copy(payload, frame.Payload)
		return nil
	})
}

// PayloadWriter writes exactly one framed payload into the supplied buffer. The
// buffer has the requested payload length and is only valid for the duration of
// the call. Writers must not retain or modify it after returning.
type PayloadWriter func([]byte) error

// MarshalFrameWithPayload returns the canonical v1 frame for a payload written
// directly into its final output buffer. When writePayload follows the
// PayloadWriter buffer-lifetime contract, this function owns the envelope and
// computes a checksum matching the completed payload.
func MarshalFrameWithPayload(typeID uint64, codecID string, payloadLength int, writePayload PayloadWriter) ([]byte, error) {
	limits := DefaultLimits()
	if typeID == 0 || payloadLength < 0 || len(codecID) > limits.MaxCodecID || payloadLength > limits.MaxPayload {
		return nil, ErrFrameLimit
	}
	if writePayload == nil {
		return nil, ErrInvalidFrame
	}
	buf := make([]byte, 0, 4+binary.MaxVarintLen64*4+len(codecID)+payloadLength+4)
	buf = append(buf, 'C', 'R', 'D', 'T')
	buf = binary.AppendUvarint(buf, FormatVersion)
	buf = binary.AppendUvarint(buf, typeID)
	buf = binary.AppendUvarint(buf, uint64(len(codecID)))
	buf = append(buf, codecID...)
	buf = binary.AppendUvarint(buf, uint64(payloadLength))
	payloadStart := len(buf)
	buf = buf[:payloadStart+payloadLength]
	if err := writePayload(buf[payloadStart:]); err != nil {
		return nil, err
	}
	checksum := crc32.Checksum(buf[4:], castagnoliTable)
	return binary.BigEndian.AppendUint32(buf, checksum), nil
}

// UnmarshalFrame validates and decodes one complete canonical v1 frame. Its
// returned payload is independent of data and remains safe to retain.
func UnmarshalFrame(data []byte, limits Limits) (Frame, error) {
	frame, err := UnmarshalFrameView(data, limits)
	if err != nil {
		return Frame{}, err
	}
	frame.Payload = append([]byte(nil), frame.Payload...)
	return frame, nil
}

// UnmarshalFrameView validates and decodes one complete canonical v1 frame
// without copying the payload. The returned Payload aliases data, so callers
// must not retain it or modify data while the view is in use. Use
// UnmarshalFrame when a caller-owned payload is required.
//
// This is intended for bounded decoders that validate and copy only the fields
// they retain. Validation, including the checksum, completes before the view is
// returned.
func UnmarshalFrameView(data []byte, limits Limits) (Frame, error) {
	if !limits.valid() || len(data) > limits.MaxFrameBytes || len(data) < 9 {
		return Frame{}, ErrFrameLimit
	}
	if string(data[:4]) != "CRDT" {
		return Frame{}, ErrInvalidFrame
	}
	stored := binary.BigEndian.Uint32(data[len(data)-4:])
	if crc32.Checksum(data[4:len(data)-4], castagnoliTable) != stored {
		return Frame{}, ErrInvalidFrame
	}
	position := 4
	version, next, ok := ReadUvarint(data[:len(data)-4], position)
	if !ok || version != FormatVersion {
		return Frame{}, ErrInvalidFrame
	}
	position = next
	typeID, next, ok := ReadUvarint(data[:len(data)-4], position)
	if !ok || typeID == 0 {
		return Frame{}, ErrInvalidFrame
	}
	position = next
	codecLength, next, ok := ReadUvarint(data[:len(data)-4], position)
	if !ok || codecLength > uint64(limits.MaxCodecID) || codecLength > uint64(len(data)-4-next) {
		return Frame{}, ErrFrameLimit
	}
	position = next
	codecEnd := position + int(codecLength)
	codecID := string(data[position:codecEnd])
	position = codecEnd
	payloadLength, next, ok := ReadUvarint(data[:len(data)-4], position)
	if !ok || payloadLength > uint64(limits.MaxPayload) || payloadLength != uint64(len(data)-4-next) {
		return Frame{}, ErrFrameLimit
	}
	payload := data[next : len(data)-4]
	return Frame{TypeID: typeID, CodecID: codecID, Payload: payload}, nil
}

// AppendUvarint appends the unique shortest representation of value.
func AppendUvarint(dst []byte, value uint64) []byte {
	return binary.AppendUvarint(dst, value)
}

// UvarintSize returns the size of value's canonical unsigned-varint encoding.
func UvarintSize(value uint64) int {
	var encoded [binary.MaxVarintLen64]byte
	return binary.PutUvarint(encoded[:], value)
}

// AppendTag appends the canonical CRDT tag payload shared by framed CRDTs.
func AppendTag(dst []byte, tag crdt.Tag) []byte {
	dst = AppendUvarint(dst, uint64(len(tag.ReplicaID)))
	dst = append(dst, tag.ReplicaID...)
	dst = AppendUvarint(dst, tag.WallTime)
	return AppendUvarint(dst, tag.Logical)
}

// TagSize returns the number of bytes used by AppendTag.
func TagSize(tag crdt.Tag) int {
	return UvarintSize(uint64(len(tag.ReplicaID))) + len(tag.ReplicaID) +
		UvarintSize(tag.WallTime) + UvarintSize(tag.Logical)
}

// ReadTag decodes one bounded canonical tag without retaining data's backing
// storage. The caller owns the returned tag value.
func ReadTag(data []byte, position, maxStringBytes int) (crdt.Tag, int, bool) {
	replicaID, next, ok := ReadBytes(data, position, maxStringBytes)
	if !ok {
		return crdt.Tag{}, position, false
	}
	wallTime, next, ok := ReadUvarint(data, next)
	if !ok {
		return crdt.Tag{}, position, false
	}
	logical, next, ok := ReadUvarint(data, next)
	if !ok {
		return crdt.Tag{}, position, false
	}
	tag := crdt.Tag{ReplicaID: string(replicaID), WallTime: wallTime, Logical: logical}
	if !tag.Valid() {
		return crdt.Tag{}, position, false
	}
	return tag, next, true
}

// ReadUvarint reads one shortest-form unsigned varint at position. It rejects
// truncated, overflowing, and non-canonical encodings.
func ReadUvarint(data []byte, position int) (uint64, int, bool) {
	if position < 0 || position >= len(data) {
		return 0, position, false
	}
	value, count := binary.Uvarint(data[position:])
	if count <= 0 {
		return 0, position, false
	}
	var canonical [binary.MaxVarintLen64]byte
	if binary.PutUvarint(canonical[:], value) != count {
		return 0, position, false
	}
	return value, position + count, true
}

// ReadBytes reads a length-prefixed byte sequence without allocating. max is
// an explicit bound for the sequence and must be non-negative.
func ReadBytes(data []byte, position, max int) ([]byte, int, bool) {
	if max < 0 {
		return nil, position, false
	}
	length, next, ok := ReadUvarint(data, position)
	if !ok || length > uint64(max) || length > uint64(len(data)-next) {
		return nil, position, false
	}
	end := next + int(length)
	return data[next:end], end, true
}

func (limits DecoderLimits) valid() bool {
	return limits.MaxFrameBytes > 0 && limits.MaxPayload > 0 && limits.MaxCodecID > 0 &&
		limits.MaxElements > 0 && limits.MaxTags > 0 && limits.MaxStringBytes > 0
}
