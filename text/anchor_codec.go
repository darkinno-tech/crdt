package text

import frame "github.com/DarkInno/crdt/encoding"

const anchorEncodingVersion uint64 = 1

// AnchorEncodingLimits bounds host-owned relative-position metadata before it
// allocates a replica ID or attempts to resolve an RGA position. Applications
// receiving anchors from a peer should use the authenticated group limit, not
// a process-global default.
type AnchorEncodingLimits struct {
	MaxBytes          int
	MaxReplicaIDBytes int
}

// DefaultAnchorEncodingLimits returns conservative limits for an encoded
// cursor, selection, or comment range. They intentionally remain independent
// from CRDT frame limits because anchors are not CRDT frames.
func DefaultAnchorEncodingLimits() AnchorEncodingLimits {
	return AnchorEncodingLimits{
		MaxBytes:          128<<10 + 64,
		MaxReplicaIDBytes: 64 << 10,
	}
}

func (limits AnchorEncodingLimits) valid() bool {
	return limits.MaxBytes > 0 && limits.MaxReplicaIDBytes > 0 &&
		limits.MaxReplicaIDBytes <= limits.MaxBytes
}

// MarshalBinary returns the canonical, versioned relative-position encoding
// for anchor. The bytes are portable host metadata, not a rich-text or RGA
// frame; applications must bind their envelope to one authenticated document,
// group, epoch, and retention policy.
func (anchor Anchor) MarshalBinary() ([]byte, error) {
	return anchor.MarshalBinaryWithLimits(DefaultAnchorEncodingLimits())
}

// MarshalBinaryWithLimits returns the canonical relative-position encoding
// while checking the receiver's metadata budget before allocating output.
func (anchor Anchor) MarshalBinaryWithLimits(limits AnchorEncodingLimits) ([]byte, error) {
	if !limits.valid() || !anchor.Valid() {
		return nil, ErrInvalidAnchor
	}
	size, ok := anchorPayloadSize(anchor, limits)
	if !ok || size > limits.MaxBytes-frame.UvarintSize(anchorEncodingVersion) {
		return nil, ErrResourceLimit
	}
	encoded := make([]byte, 0, frame.UvarintSize(anchorEncodingVersion)+size)
	encoded = frame.AppendUvarint(encoded, anchorEncodingVersion)
	return appendAnchorPayload(encoded, anchor), nil
}

// UnmarshalAnchor decodes a canonical relative-position metadata record using
// default bounded limits. It does not prove that the anchor belongs to the
// current document; ResolveAnchor performs that check against retained state.
func UnmarshalAnchor(data []byte) (Anchor, error) {
	return UnmarshalAnchorWithLimits(data, DefaultAnchorEncodingLimits())
}

// UnmarshalAnchorWithLimits decodes one complete canonical anchor record. It
// rejects unknown versions, non-canonical varints, trailing bytes, malformed
// tags, and oversized replica IDs before retaining any decoded string.
func UnmarshalAnchorWithLimits(data []byte, limits AnchorEncodingLimits) (Anchor, error) {
	if !limits.valid() || len(data) == 0 || len(data) > limits.MaxBytes {
		return Anchor{}, ErrInvalidAnchor
	}
	version, position, ok := frame.ReadUvarint(data, 0)
	if !ok || version != anchorEncodingVersion {
		return Anchor{}, ErrInvalidAnchor
	}
	anchor, position, ok := readAnchorPayload(data, position, limits)
	if !ok || position != len(data) {
		return Anchor{}, ErrInvalidAnchor
	}
	return anchor, nil
}

// MarshalBinary returns the canonical, versioned metadata encoding for both
// relative boundaries. It does not normalize their order so editor selections
// can retain an anchor/head direction.
func (anchors AnchorRange) MarshalBinary() ([]byte, error) {
	return anchors.MarshalBinaryWithLimits(DefaultAnchorEncodingLimits())
}

// MarshalBinaryWithLimits encodes both boundaries after preflighting the full
// result against the supplied metadata budget.
func (anchors AnchorRange) MarshalBinaryWithLimits(limits AnchorEncodingLimits) ([]byte, error) {
	if !limits.valid() || !anchors.Valid() {
		return nil, ErrInvalidAnchor
	}
	startSize, startOK := anchorPayloadSize(anchors.Start, limits)
	endSize, endOK := anchorPayloadSize(anchors.End, limits)
	versionSize := frame.UvarintSize(anchorEncodingVersion)
	if !startOK || !endOK || startSize > limits.MaxBytes-versionSize || endSize > limits.MaxBytes-versionSize-startSize {
		return nil, ErrResourceLimit
	}
	encoded := make([]byte, 0, versionSize+startSize+endSize)
	encoded = frame.AppendUvarint(encoded, anchorEncodingVersion)
	encoded = appendAnchorPayload(encoded, anchors.Start)
	return appendAnchorPayload(encoded, anchors.End), nil
}

// UnmarshalAnchorRange decodes one complete, canonical pair of relative
// boundaries with default metadata limits.
func UnmarshalAnchorRange(data []byte) (AnchorRange, error) {
	return UnmarshalAnchorRangeWithLimits(data, DefaultAnchorEncodingLimits())
}

// UnmarshalAnchorRangeWithLimits decodes one complete range metadata record
// without accepting unknown versions, trailing data, or malformed positions.
func UnmarshalAnchorRangeWithLimits(data []byte, limits AnchorEncodingLimits) (AnchorRange, error) {
	if !limits.valid() || len(data) == 0 || len(data) > limits.MaxBytes {
		return AnchorRange{}, ErrInvalidAnchor
	}
	version, position, ok := frame.ReadUvarint(data, 0)
	if !ok || version != anchorEncodingVersion {
		return AnchorRange{}, ErrInvalidAnchor
	}
	start, position, ok := readAnchorPayload(data, position, limits)
	if !ok {
		return AnchorRange{}, ErrInvalidAnchor
	}
	end, position, ok := readAnchorPayload(data, position, limits)
	if !ok || position != len(data) {
		return AnchorRange{}, ErrInvalidAnchor
	}
	return AnchorRange{Start: start, End: end}, nil
}

func anchorPayloadSize(anchor Anchor, limits AnchorEncodingLimits) (int, bool) {
	if !anchor.Valid() {
		return 0, false
	}
	// association and position-present are both canonical one-byte varints.
	size := 2
	if !anchor.Position.Valid() {
		return size, true
	}
	if len(anchor.Position.ReplicaID) > limits.MaxReplicaIDBytes {
		return 0, false
	}
	tagSize := frame.TagSize(anchor.Position)
	if tagSize > limits.MaxBytes-size {
		return 0, false
	}
	return size + tagSize, true
}

func appendAnchorPayload(dst []byte, anchor Anchor) []byte {
	dst = frame.AppendUvarint(dst, uint64(anchor.Association))
	if !anchor.Position.Valid() {
		return frame.AppendUvarint(dst, 0)
	}
	dst = frame.AppendUvarint(dst, 1)
	return frame.AppendTag(dst, anchor.Position)
}

func readAnchorPayload(data []byte, position int, limits AnchorEncodingLimits) (Anchor, int, bool) {
	association, position, ok := frame.ReadUvarint(data, position)
	if !ok || (association != uint64(AnchorBefore) && association != uint64(AnchorAfter)) {
		return Anchor{}, position, false
	}
	present, position, ok := frame.ReadUvarint(data, position)
	if !ok || (present != 0 && present != 1) {
		return Anchor{}, position, false
	}
	anchor := Anchor{Association: AnchorAssociation(association)}
	if present == 0 {
		return anchor, position, true
	}
	positionValue, next, ok := frame.ReadTag(data, position, limits.MaxReplicaIDBytes)
	if !ok {
		return Anchor{}, position, false
	}
	anchor.Position = positionValue
	return anchor, next, anchor.Valid()
}
