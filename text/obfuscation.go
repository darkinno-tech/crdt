package text

import (
	"github.com/im10furry/crdt"
	frame "github.com/im10furry/crdt/encoding"
)

// Obfuscate returns a join-compatible debug copy of d with every inserted
// character replaced by an inert placeholder of the same uvarint width.
// Positions, parent links, tombstones, and operation cardinality are retained
// so independently obfuscated deltas from the same document remain safe to
// decode, deduplicate, and merge with one another.
//
// Obfuscation is diagnostic redaction, not encryption or anonymization. It
// intentionally retains CRDT identifiers, topology, operation counts, and
// HLC-derived metadata. Never feed the returned delta into a replica that may
// already contain the original values: the same immutable position with a
// different rune is correctly rejected as ErrTagConflict.
func (d Delta) Obfuscate() (Delta, error) {
	if err := validateDelta(d); err != nil {
		return Delta{}, err
	}
	return Delta{nodes: obfuscatedNodes(d.nodes), tombstones: cloneTombstones(d.tombstones)}, nil
}

// MarshalObfuscatedBinary encodes an obfuscated legacy scalar RGA delta for
// isolated debugging. The result uses the normal TypeIDRGA delta contract.
func (d Delta) MarshalObfuscatedBinary() ([]byte, error) {
	return d.MarshalObfuscatedBinaryWithLimits(frame.DefaultLimits())
}

// MarshalObfuscatedBinaryWithLimits encodes an obfuscated legacy scalar RGA
// delta while enforcing the supplied output limits.
func (d Delta) MarshalObfuscatedBinaryWithLimits(limits frame.DecoderLimits) ([]byte, error) {
	obfuscated, err := d.Obfuscate()
	if err != nil {
		return nil, err
	}
	return obfuscated.MarshalBinaryWithLimits(limits)
}

// MarshalObfuscatedRunBinary encodes an obfuscated run-v2 RGA delta for
// isolated debugging. The result remains valid only for a separately
// negotiated run-v2 group.
func (d Delta) MarshalObfuscatedRunBinary() ([]byte, error) {
	return d.MarshalObfuscatedRunBinaryWithLimits(frame.DefaultLimits())
}

// MarshalObfuscatedRunBinaryWithLimits encodes an obfuscated run-v2 delta
// while enforcing the supplied output limits.
func (d Delta) MarshalObfuscatedRunBinaryWithLimits(limits frame.DecoderLimits) ([]byte, error) {
	obfuscated, err := d.Obfuscate()
	if err != nil {
		return nil, err
	}
	return obfuscated.MarshalRunBinaryWithLimits(limits)
}

// MarshalObfuscatedBinary encodes a complete obfuscated legacy scalar RGA
// state. It does not alter r and refuses incomplete state just like
// MarshalBinary.
func (r *RGA) MarshalObfuscatedBinary() ([]byte, error) {
	return r.MarshalObfuscatedBinaryWithLimits(frame.DefaultLimits())
}

// MarshalObfuscatedBinaryWithLimits encodes complete obfuscated legacy state
// while enforcing the supplied output limits.
func (r *RGA) MarshalObfuscatedBinaryWithLimits(limits frame.DecoderLimits) ([]byte, error) {
	if r == nil {
		return nil, ErrNilText
	}
	r.mu.RLock()
	if len(r.pending) > 0 {
		r.mu.RUnlock()
		return nil, ErrIncompleteState
	}
	nodes, tombstones := obfuscatedNodes(r.nodes), cloneTombstones(r.tombstones)
	r.mu.RUnlock()
	return marshalRGAWithLimits(crdt.TypeIDRGAState, nodes, tombstones, limits)
}

// MarshalObfuscatedRunBinary encodes a complete obfuscated run-v2 RGA state.
// It does not alter r and refuses incomplete state just like MarshalRunBinary.
func (r *RGA) MarshalObfuscatedRunBinary() ([]byte, error) {
	return r.MarshalObfuscatedRunBinaryWithLimits(frame.DefaultLimits())
}

// MarshalObfuscatedRunBinaryWithLimits encodes complete obfuscated run-v2
// state while enforcing the supplied output limits.
func (r *RGA) MarshalObfuscatedRunBinaryWithLimits(limits frame.DecoderLimits) ([]byte, error) {
	if r == nil {
		return nil, ErrNilText
	}
	r.mu.RLock()
	if len(r.pending) > 0 {
		r.mu.RUnlock()
		return nil, ErrIncompleteState
	}
	nodes, tombstones := obfuscatedNodes(r.nodes), cloneTombstones(r.tombstones)
	r.mu.RUnlock()
	return marshalRGARun(crdt.TypeIDRGARunState, nodes, tombstones, limits)
}

func obfuscatedNodes(nodes map[Position]node) map[Position]node {
	output := make(map[Position]node, len(nodes))
	for id, item := range nodes {
		item.rune = obfuscatedRune(item.rune)
		output[id] = item
	}
	return output
}

func obfuscatedRune(value rune) rune {
	switch frame.UvarintSize(uint64(value)) {
	case 1:
		return 'a'
	case 2:
		return 0x80
	default:
		return 0x4000
	}
}
