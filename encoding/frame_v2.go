package encoding

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"strconv"
)

const (
	v2PayloadRaw     uint64 = 0
	v2PayloadDeflate uint64 = 1

	// Avoid constructing a DEFLATE encoder for small interactive updates where
	// its setup cost and the v2 length fields are unlikely to repay themselves.
	minV2DeflatePayloadBytes = 128
	maxIntValue              = 1<<(strconv.IntSize-1) - 1
)

// deflateWriterCache retains at most one reusable writer. A sync.Pool would
// improve more concurrent callers at the cost of retaining compressor state
// per active P; outer-frame compression must keep that memory trade-off
// bounded because its input can originate from application text.
var deflateWriterCache = make(chan *flate.Writer, 1)

// MarshalFrameV2 returns a compression-aware v2 frame. The encoder chooses
// raw payload bytes for small or incompressible inputs and DEFLATE only when
// it makes the complete v2 envelope smaller. V2 changes representation, not
// CRDT semantics: TypeID, CodecID, and the decoded payload are unchanged.
func MarshalFrameV2(frame Frame) ([]byte, error) {
	return MarshalFrameV2WithLimits(frame, DefaultLimits())
}

// MarshalFrameV2WithLimits returns a bounded v2 frame. Callers must negotiate
// FormatVersionV2 before sending it: older peers correctly reject this format.
func MarshalFrameV2WithLimits(frame Frame, limits Limits) ([]byte, error) {
	if !validFrameV2Input(frame.TypeID, frame.CodecID, len(frame.Payload), limits) {
		return nil, ErrFrameLimit
	}
	return marshalFrameV2Payload(frame.TypeID, frame.CodecID, frame.Payload, limits)
}

// MarshalFrameV2WithPayload writes a bounded v2 payload directly into its
// final raw envelope when compression is not considered. For larger payloads,
// it uses a bounded temporary buffer so it can choose the smaller raw or
// DEFLATE representation. The writer has the same lifetime contract as
// PayloadWriter: it must fill only the supplied payload and must not retain it.
func MarshalFrameV2WithPayload(typeID uint64, codecID string, payloadLength int, writePayload PayloadWriter) ([]byte, error) {
	return MarshalFrameV2WithPayloadAndLimits(typeID, codecID, payloadLength, DefaultLimits(), writePayload)
}

// MarshalFrameV2WithPayloadAndLimits is MarshalFrameV2WithPayload with
// explicit output limits. For raw interactive payloads it validates the final
// v2 frame budget before invoking writePayload, avoiding a temporary payload
// allocation and copy. A payload at or above the compression threshold keeps a
// bounded temporary buffer because mode selection depends on its bytes.
func MarshalFrameV2WithPayloadAndLimits(typeID uint64, codecID string, payloadLength int, limits Limits, writePayload PayloadWriter) ([]byte, error) {
	if !validFrameV2Input(typeID, codecID, payloadLength, limits) {
		return nil, ErrFrameLimit
	}
	if writePayload == nil {
		return nil, ErrInvalidFrame
	}
	if payloadLength < minV2DeflatePayloadBytes {
		return marshalFrameV2RawWithPayload(typeID, codecID, payloadLength, limits, writePayload)
	}
	payload := make([]byte, payloadLength)
	if err := writePayload(payload); err != nil {
		return nil, err
	}
	return marshalFrameV2Payload(typeID, codecID, payload, limits)
}

func validFrameV2Input(typeID uint64, codecID string, payloadLength int, limits Limits) bool {
	return limits.valid() && typeID != 0 && payloadLength >= 0 && len(codecID) <= limits.MaxCodecID && payloadLength <= limits.MaxPayload
}

func marshalFrameV2Payload(typeID uint64, codecID string, payload []byte, limits Limits) ([]byte, error) {
	mode, encoded := v2PayloadRaw, payload
	if len(payload) >= minV2DeflatePayloadBytes {
		compressed, err := deflatePayload(payload)
		if err != nil {
			return nil, err
		}
		if v2FrameSize(typeID, codecID, len(payload), len(compressed)) < v2FrameSize(typeID, codecID, len(payload), len(payload)) {
			mode, encoded = v2PayloadDeflate, compressed
		}
	}
	frameBytes := v2FrameSize(typeID, codecID, len(payload), len(encoded))
	if frameBytes > limits.MaxFrameBytes {
		return nil, ErrFrameLimit
	}

	output := make([]byte, 0, frameBytes)
	output = append(output, 'C', 'R', 'D', 'T')
	output = binary.AppendUvarint(output, FormatVersionV2)
	output = binary.AppendUvarint(output, typeID)
	output = binary.AppendUvarint(output, uint64(len(codecID)))
	output = append(output, codecID...)
	output = binary.AppendUvarint(output, mode)
	output = binary.AppendUvarint(output, uint64(len(payload)))
	output = binary.AppendUvarint(output, uint64(len(encoded)))
	output = append(output, encoded...)
	checksum := crc32.Checksum(output[4:], castagnoliTable)
	return binary.BigEndian.AppendUint32(output, checksum), nil
}

func marshalFrameV2RawWithPayload(typeID uint64, codecID string, payloadLength int, limits Limits, writePayload PayloadWriter) ([]byte, error) {
	if payloadLength < 0 {
		return nil, ErrFrameLimit
	}
	payloadSize := uint64(payloadLength)
	frameBytes := v2FrameSize(typeID, codecID, payloadLength, payloadLength)
	if frameBytes > limits.MaxFrameBytes {
		return nil, ErrFrameLimit
	}
	output := make([]byte, 0, frameBytes)
	output = append(output, 'C', 'R', 'D', 'T')
	output = binary.AppendUvarint(output, FormatVersionV2)
	output = binary.AppendUvarint(output, typeID)
	output = binary.AppendUvarint(output, uint64(len(codecID)))
	output = append(output, codecID...)
	output = binary.AppendUvarint(output, v2PayloadRaw)
	output = binary.AppendUvarint(output, payloadSize)
	output = binary.AppendUvarint(output, payloadSize)
	payloadStart := len(output)
	output = output[:payloadStart+payloadLength]
	if err := writePayload(output[payloadStart:]); err != nil {
		return nil, err
	}
	checksum := crc32.Checksum(output[4:], castagnoliTable)
	return binary.BigEndian.AppendUint32(output, checksum), nil
}

// ConvertFrameV1ToV2 converts one validated v1 frame without changing its
// CRDT payload. It is useful for stores or providers that retain a v1 producer
// API while negotiating v2 at their transport boundary.
func ConvertFrameV1ToV2(data []byte, limits Limits) ([]byte, error) {
	decoded, err := UnmarshalFrame(data, limits)
	if err != nil {
		return nil, err
	}
	if decoded.Version() != FormatVersion {
		return nil, ErrInvalidFrame
	}
	return MarshalFrameV2WithLimits(decoded, limits)
}

// ConvertFrameV2ToV1 converts one validated v2 frame without changing its
// CRDT payload. It provides an explicit downgrade path for a separately
// negotiated legacy peer; it never happens implicitly during decoding.
func ConvertFrameV2ToV1(data []byte, limits Limits) ([]byte, error) {
	decoded, err := UnmarshalFrame(data, limits)
	if err != nil {
		return nil, err
	}
	if decoded.Version() != FormatVersionV2 {
		return nil, ErrInvalidFrame
	}
	return MarshalFrameWithPayloadAndLimits(decoded.TypeID, decoded.CodecID, len(decoded.Payload), limits, func(payload []byte) error {
		copy(payload, decoded.Payload)
		return nil
	})
}

func unmarshalFrameV2(body []byte, typeID uint64, codecID string, position int, limits Limits) (Frame, error) {
	mode, next, ok := ReadUvarint(body, position)
	if !ok || (mode != v2PayloadRaw && mode != v2PayloadDeflate) {
		return Frame{}, ErrInvalidFrame
	}
	rawLength, next, ok := ReadUvarint(body, next)
	if !ok || limits.MaxPayload < 0 || rawLength > uint64(limits.MaxPayload) {
		return Frame{}, ErrFrameLimit
	}
	encodedLength, next, ok := ReadUvarint(body, next)
	if !ok {
		return Frame{}, ErrFrameLimit
	}
	remaining := len(body) - next
	if remaining < 0 || encodedLength > uint64(remaining) || encodedLength != uint64(remaining) {
		return Frame{}, ErrFrameLimit
	}
	encoded := body[next:]
	if mode == v2PayloadRaw {
		if rawLength != encodedLength {
			return Frame{}, ErrInvalidFrame
		}
		return Frame{TypeID: typeID, CodecID: codecID, Payload: encoded, formatVersion: FormatVersionV2}, nil
	}
	decodedLength, ok := uint64AsInt(rawLength)
	if !ok {
		return Frame{}, ErrFrameLimit
	}
	payload, err := inflatePayload(encoded, decodedLength)
	if err != nil {
		return Frame{}, err
	}
	return Frame{TypeID: typeID, CodecID: codecID, Payload: payload, formatVersion: FormatVersionV2, payloadOwned: true}, nil
}

func deflatePayload(payload []byte) ([]byte, error) {
	var output bytes.Buffer
	writer, err := acquireDeflateWriter()
	if err != nil {
		return nil, err
	}
	defer releaseDeflateWriter(writer)
	writer.Reset(&output)
	if _, err := writer.Write(payload); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func acquireDeflateWriter() (*flate.Writer, error) {
	select {
	case writer := <-deflateWriterCache:
		return writer, nil
	default:
		return flate.NewWriter(io.Discard, flate.BestSpeed)
	}
}

func releaseDeflateWriter(writer *flate.Writer) {
	writer.Reset(io.Discard)
	select {
	case deflateWriterCache <- writer:
	default:
	}
}

func inflatePayload(encoded []byte, rawLength int) (payload []byte, err error) {
	if rawLength < 0 || rawLength >= maxIntValue {
		return nil, ErrFrameLimit
	}
	reader := flate.NewReader(bytes.NewReader(encoded))
	defer func() {
		if closeErr := reader.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	payload = make([]byte, rawLength+1)
	count, err := io.ReadFull(reader, payload)
	if count > rawLength {
		return nil, ErrFrameLimit
	}
	if count != rawLength || (!errors.Is(err, io.ErrUnexpectedEOF) && (rawLength != 0 || !errors.Is(err, io.EOF))) {
		return nil, ErrInvalidFrame
	}
	return payload[:rawLength], nil
}

func v2FrameSize(typeID uint64, codecID string, rawLength, encodedLength int) int {
	return 4 + UvarintSize(FormatVersionV2) + UvarintSize(typeID) +
		uvarintSizeForLength(len(codecID)) + len(codecID) +
		UvarintSize(v2PayloadDeflate) + uvarintSizeForLength(rawLength) +
		uvarintSizeForLength(encodedLength) + encodedLength + 4
}

func uint64AsInt(value uint64) (int, bool) {
	if value > uint64(maxIntValue) {
		return 0, false
	}
	return int(value), true
}

func uvarintSizeForLength(length int) int {
	if length < 0 {
		return 0
	}
	return UvarintSize(uint64(length))
}
