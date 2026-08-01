package encoding

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"hash/crc32"
	"io"
)

const (
	v2PayloadRaw     uint64 = 0
	v2PayloadDeflate uint64 = 1

	// Avoid constructing a DEFLATE encoder for small interactive updates where
	// its setup cost and the v2 length fields are unlikely to repay themselves.
	minV2DeflatePayloadBytes = 128
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
	if !limits.valid() || frame.TypeID == 0 || len(frame.CodecID) > limits.MaxCodecID || len(frame.Payload) > limits.MaxPayload {
		return nil, ErrFrameLimit
	}

	mode, encoded := v2PayloadRaw, frame.Payload
	if len(frame.Payload) >= minV2DeflatePayloadBytes {
		compressed, err := deflatePayload(frame.Payload)
		if err != nil {
			return nil, err
		}
		if v2FrameSize(frame.TypeID, frame.CodecID, len(frame.Payload), len(compressed)) < v2FrameSize(frame.TypeID, frame.CodecID, len(frame.Payload), len(frame.Payload)) {
			mode, encoded = v2PayloadDeflate, compressed
		}
	}
	frameBytes := v2FrameSize(frame.TypeID, frame.CodecID, len(frame.Payload), len(encoded))
	if frameBytes > limits.MaxFrameBytes {
		return nil, ErrFrameLimit
	}

	output := make([]byte, 0, frameBytes)
	output = append(output, 'C', 'R', 'D', 'T')
	output = binary.AppendUvarint(output, FormatVersionV2)
	output = binary.AppendUvarint(output, frame.TypeID)
	output = binary.AppendUvarint(output, uint64(len(frame.CodecID)))
	output = append(output, frame.CodecID...)
	output = binary.AppendUvarint(output, mode)
	output = binary.AppendUvarint(output, uint64(len(frame.Payload)))
	output = binary.AppendUvarint(output, uint64(len(encoded)))
	output = append(output, encoded...)
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
	if !ok || rawLength > uint64(limits.MaxPayload) {
		return Frame{}, ErrFrameLimit
	}
	encodedLength, next, ok := ReadUvarint(body, next)
	if !ok || encodedLength > uint64(len(body)-next) || encodedLength != uint64(len(body)-next) {
		return Frame{}, ErrFrameLimit
	}
	encoded := body[next:]
	if mode == v2PayloadRaw {
		if rawLength != encodedLength {
			return Frame{}, ErrInvalidFrame
		}
		return Frame{TypeID: typeID, CodecID: codecID, Payload: encoded, formatVersion: FormatVersionV2}, nil
	}
	payload, err := inflatePayload(encoded, int(rawLength))
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
	if count != rawLength || (err != io.ErrUnexpectedEOF && (rawLength != 0 || err != io.EOF)) {
		return nil, ErrInvalidFrame
	}
	return payload[:rawLength], nil
}

func v2FrameSize(typeID uint64, codecID string, rawLength, encodedLength int) int {
	return 4 + UvarintSize(FormatVersionV2) + UvarintSize(typeID) +
		UvarintSize(uint64(len(codecID))) + len(codecID) +
		UvarintSize(v2PayloadDeflate) + UvarintSize(uint64(rawLength)) +
		UvarintSize(uint64(encodedLength)) + encodedLength + 4
}
