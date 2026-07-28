// Package encoding provides canonical, bounded binary frames for CRDT state.
package encoding

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
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
	limits := DefaultLimits()
	if frame.TypeID == 0 || len(frame.CodecID) > limits.MaxCodecID || len(frame.Payload) > limits.MaxPayload {
		return nil, ErrFrameLimit
	}
	buf := make([]byte, 0, 4+binary.MaxVarintLen64*4+len(frame.CodecID)+len(frame.Payload)+4)
	buf = append(buf, 'C', 'R', 'D', 'T')
	buf = binary.AppendUvarint(buf, FormatVersion)
	buf = binary.AppendUvarint(buf, frame.TypeID)
	buf = binary.AppendUvarint(buf, uint64(len(frame.CodecID)))
	buf = append(buf, frame.CodecID...)
	buf = binary.AppendUvarint(buf, uint64(len(frame.Payload)))
	buf = append(buf, frame.Payload...)
	checksum := crc32.Checksum(buf[4:], crc32.MakeTable(crc32.Castagnoli))
	return binary.BigEndian.AppendUint32(buf, checksum), nil
}

// UnmarshalFrame validates and decodes one complete canonical v1 frame.
func UnmarshalFrame(data []byte, limits Limits) (Frame, error) {
	if !limits.valid() || len(data) > limits.MaxFrameBytes || len(data) < 9 {
		return Frame{}, ErrFrameLimit
	}
	if string(data[:4]) != "CRDT" {
		return Frame{}, ErrInvalidFrame
	}
	stored := binary.BigEndian.Uint32(data[len(data)-4:])
	if crc32.Checksum(data[4:len(data)-4], crc32.MakeTable(crc32.Castagnoli)) != stored {
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
	payload := append([]byte(nil), data[next:len(data)-4]...)
	return Frame{TypeID: typeID, CodecID: codecID, Payload: payload}, nil
}

// AppendUvarint appends the unique shortest representation of value.
func AppendUvarint(dst []byte, value uint64) []byte {
	return binary.AppendUvarint(dst, value)
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
