package text

import (
	"errors"
	"fmt"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

var (
	// ErrInvalidSnapshot indicates that a delta base is not a canonical,
	// complete RGA state snapshot in either supported RGA wire format.
	ErrInvalidSnapshot = errors.New("text: invalid RGA snapshot")
	// ErrIncompatibleSnapshot indicates that the current RGA no longer contains
	// state retained by the base checkpoint. A delta can add nodes and
	// tombstones, but cannot express physical removal or resurrection, so the
	// peer must install a newer complete checkpoint instead.
	ErrIncompatibleSnapshot = errors.New("text: RGA snapshot is incompatible with current state")
)

// SnapshotBase is an immutable, decoded complete RGA state that can be reused
// for repeated differential encodes against the same peer checkpoint. It has
// no exported mutable state; create it only with NewSnapshotBase.
type SnapshotBase struct {
	nodes      map[Position]node
	tombstones map[Position]struct{}
	stateType  uint64
	valid      bool
}

// NewSnapshotBase validates and decodes the saved snapshot once so callers that repeatedly
// synchronize against the same checkpoint do not reparse its full state frame
// for every delta.
func NewSnapshotBase(saved snapshot.Snapshot) (SnapshotBase, error) {
	return NewSnapshotBaseWithLimits(saved, frame.DefaultLimits())
}

// NewSnapshotBaseWithLimits is NewSnapshotBase with caller-selected decoder
// limits for the supplied checkpoint.
func NewSnapshotBaseWithLimits(saved snapshot.Snapshot, limits frame.DecoderLimits) (SnapshotBase, error) {
	stateType, nodes, tombstones, err := rgaSnapshotState(saved, limits)
	if err != nil {
		return SnapshotBase{}, err
	}
	return SnapshotBase{nodes: nodes, tombstones: tombstones, stateType: stateType, valid: true}, nil
}

// DeltaSince returns the mutations that move a receiver known to contain base
// toward r's current state. The result includes every required structural
// ancestor absent from base, so it remains safe when the receiver installs the
// resulting delta before any later updates.
//
// base is deliberately a validated complete snapshot rather than a map of
// greatest HLC tags. HLC tags are ordered but not contiguous, so a greatest-tag
// frontier cannot prove that a receiver has every earlier mutation and could
// otherwise make a differential update omit data it still needs.
func (r *RGA) DeltaSince(base snapshot.Snapshot) (Delta, error) {
	decoded, err := NewSnapshotBase(base)
	if err != nil {
		return Delta{}, err
	}
	return r.DeltaSinceBase(decoded)
}

// DeltaSinceBase is DeltaSince with a reusable parsed snapshot base.
func (r *RGA) DeltaSinceBase(base SnapshotBase) (Delta, error) {
	return r.deltaSinceBase(base)
}

// MarshalDeltaSince encodes DeltaSince(base) using the matching delta wire
// protocol for base. The receiver must apply the result to a state that
// contains base (or a CRDT superset of base).
func (r *RGA) MarshalDeltaSince(base snapshot.Snapshot) ([]byte, error) {
	return r.MarshalDeltaSinceWithLimits(base, frame.DefaultLimits())
}

// MarshalDeltaSinceBase encodes DeltaSinceBase(base) using the matching delta
// wire protocol and default frame limits. Reuse a SnapshotBase when a peer
// checkpoint serves multiple anti-entropy rounds.
func (r *RGA) MarshalDeltaSinceBase(base SnapshotBase) ([]byte, error) {
	return r.MarshalDeltaSinceBaseWithLimits(base, frame.DefaultLimits())
}

// MarshalDeltaSinceWithLimits is MarshalDeltaSince with caller-selected bounds
// for both decoding the base snapshot and producing the matching delta frame.
func (r *RGA) MarshalDeltaSinceWithLimits(base snapshot.Snapshot, limits frame.DecoderLimits) ([]byte, error) {
	decoded, err := NewSnapshotBaseWithLimits(base, limits)
	if err != nil {
		return nil, err
	}
	return r.MarshalDeltaSinceBaseWithLimits(decoded, limits)
}

// MarshalDeltaSinceBaseWithLimits is MarshalDeltaSinceBase with caller-selected
// output frame bounds.
func (r *RGA) MarshalDeltaSinceBaseWithLimits(base SnapshotBase, limits frame.DecoderLimits) ([]byte, error) {
	delta, err := r.deltaSinceBase(base)
	if err != nil {
		return nil, err
	}
	switch base.stateType {
	case crdt.TypeIDRGAState:
		return marshalRGAWithLimits(crdt.TypeIDRGADelta, delta.nodes, delta.tombstones, limits)
	case crdt.TypeIDRGARunState:
		return marshalRGARun(crdt.TypeIDRGARunDelta, delta.nodes, delta.tombstones, limits)
	case crdt.TypeIDRGAPackedState:
		return marshalRGAPacked(crdt.TypeIDRGAPackedDelta, delta.nodes, delta.tombstones, limits)
	default:
		return nil, ErrInvalidSnapshot
	}
}

// MarshalRunDeltaSinceFrameV2 encodes the run-v2 delta from base to r in a
// separately negotiated compression-aware outer frame v2. The base must be a
// complete run-v2 RGA snapshot; scalar-v1 bases remain a distinct protocol.
func (r *RGA) MarshalRunDeltaSinceFrameV2(base snapshot.Snapshot) ([]byte, error) {
	return r.MarshalRunDeltaSinceFrameV2WithLimits(base, frame.DefaultLimits())
}

// MarshalRunDeltaSinceFrameV2WithLimits is MarshalRunDeltaSinceFrameV2 with
// explicit bounds for both base validation and final v2 output.
func (r *RGA) MarshalRunDeltaSinceFrameV2WithLimits(base snapshot.Snapshot, limits frame.DecoderLimits) ([]byte, error) {
	decoded, err := NewSnapshotBaseWithLimits(base, limits)
	if err != nil {
		return nil, err
	}
	return r.MarshalRunDeltaSinceFrameV2BaseWithLimits(decoded, limits)
}

// MarshalRunDeltaSinceFrameV2Base is MarshalRunDeltaSinceFrameV2 for a
// validated snapshot base that can be reused across anti-entropy rounds.
func (r *RGA) MarshalRunDeltaSinceFrameV2Base(base SnapshotBase) ([]byte, error) {
	return r.MarshalRunDeltaSinceFrameV2BaseWithLimits(base, frame.DefaultLimits())
}

// MarshalRunDeltaSinceFrameV2BaseWithLimits encodes a delta against a cached
// run-v2 snapshot base without building an intermediate v1 envelope.
func (r *RGA) MarshalRunDeltaSinceFrameV2BaseWithLimits(base SnapshotBase, limits frame.DecoderLimits) ([]byte, error) {
	if !base.valid || base.stateType != crdt.TypeIDRGARunState {
		return nil, ErrInvalidSnapshot
	}
	delta, err := r.deltaSinceBase(base)
	if err != nil {
		return nil, err
	}
	return marshalRGARunFrameV2(crdt.TypeIDRGARunDelta, delta.nodes, delta.tombstones, limits)
}

// MarshalPackedDeltaSinceFrameV2 encodes the packed-v3 delta from base to r
// in a separately negotiated compression-aware outer frame v2. The base must
// be a complete packed-v3 RGA snapshot.
func (r *RGA) MarshalPackedDeltaSinceFrameV2(base snapshot.Snapshot) ([]byte, error) {
	return r.MarshalPackedDeltaSinceFrameV2WithLimits(base, frame.DefaultLimits())
}

// MarshalPackedDeltaSinceFrameV2WithLimits is MarshalPackedDeltaSinceFrameV2
// with explicit bounds for both base validation and final v2 output.
func (r *RGA) MarshalPackedDeltaSinceFrameV2WithLimits(base snapshot.Snapshot, limits frame.DecoderLimits) ([]byte, error) {
	decoded, err := NewSnapshotBaseWithLimits(base, limits)
	if err != nil {
		return nil, err
	}
	return r.MarshalPackedDeltaSinceFrameV2BaseWithLimits(decoded, limits)
}

// MarshalPackedDeltaSinceFrameV2Base is MarshalPackedDeltaSinceFrameV2 for a
// validated snapshot base that can be reused across anti-entropy rounds.
func (r *RGA) MarshalPackedDeltaSinceFrameV2Base(base SnapshotBase) ([]byte, error) {
	return r.MarshalPackedDeltaSinceFrameV2BaseWithLimits(base, frame.DefaultLimits())
}

// MarshalPackedDeltaSinceFrameV2BaseWithLimits encodes a delta against a
// cached packed-v3 snapshot base without building an intermediate v1 envelope.
func (r *RGA) MarshalPackedDeltaSinceFrameV2BaseWithLimits(base SnapshotBase, limits frame.DecoderLimits) ([]byte, error) {
	if !base.valid || base.stateType != crdt.TypeIDRGAPackedState {
		return nil, ErrInvalidSnapshot
	}
	delta, err := r.deltaSinceBase(base)
	if err != nil {
		return nil, err
	}
	return marshalRGAPackedFrameV2(crdt.TypeIDRGAPackedDelta, delta.nodes, delta.tombstones, limits)
}

func (r *RGA) deltaSinceBase(base SnapshotBase) (Delta, error) {
	if r == nil || r.clock == nil {
		return Delta{}, ErrNilText
	}
	if !base.valid {
		return Delta{}, ErrInvalidSnapshot
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.pending) > 0 {
		return Delta{}, ErrIncompleteState
	}
	return deltaBetweenRGAStates(r.nodes, r.tombstones, base.nodes, base.tombstones)
}

func rgaSnapshotState(base snapshot.Snapshot, limits frame.DecoderLimits) (uint64, map[Position]node, map[Position]struct{}, error) {
	data := base.Bytes()
	var (
		nodes      map[Position]node
		tombstones map[Position]struct{}
		err        error
	)
	switch base.TypeID {
	case crdt.TypeIDRGAState:
		nodes, tombstones, err = unmarshalRGA(data, crdt.TypeIDRGAState, limits, true)
	case crdt.TypeIDRGARunState:
		nodes, tombstones, err = unmarshalRGARun(data, crdt.TypeIDRGARunState, limits, true)
	case crdt.TypeIDRGAPackedState:
		nodes, tombstones, err = unmarshalRGAPacked(data, crdt.TypeIDRGAPackedState, limits, true)
	default:
		return 0, nil, nil, ErrInvalidSnapshot
	}
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%w: %w", ErrInvalidSnapshot, err)
	}
	return base.TypeID, nodes, tombstones, nil
}

func deltaBetweenRGAStates(currentNodes map[Position]node, currentTombstones map[Position]struct{}, baseNodes map[Position]node, baseTombstones map[Position]struct{}) (Delta, error) {
	for id, baseItem := range baseNodes {
		currentItem, exists := currentNodes[id]
		if !exists {
			return Delta{}, ErrIncompatibleSnapshot
		}
		if currentItem != baseItem {
			return Delta{}, ErrTagConflict
		}
	}
	for id := range baseTombstones {
		if _, exists := currentTombstones[id]; !exists {
			return Delta{}, ErrIncompatibleSnapshot
		}
	}
	delta := Delta{
		nodes:      make(map[Position]node),
		tombstones: make(map[Position]struct{}),
	}
	for id := range currentNodes {
		if _, known := baseNodes[id]; known {
			continue
		}
		if _, removed := baseTombstones[id]; removed {
			continue
		}
		if err := includeRGAAncestorChain(delta.nodes, currentNodes, baseNodes, id); err != nil {
			return Delta{}, err
		}
	}
	for id := range currentTombstones {
		if _, known := baseTombstones[id]; !known {
			delta.tombstones[id] = struct{}{}
		}
	}
	if err := validateDelta(delta); err != nil {
		return Delta{}, err
	}
	return delta, nil
}

func includeRGAAncestorChain(destination map[Position]node, currentNodes map[Position]node, baseNodes map[Position]node, id Position) error {
	for steps := 0; ; steps++ {
		if steps >= len(currentNodes) {
			return ErrInvalidDelta
		}
		if _, alreadyIncluded := destination[id]; alreadyIncluded {
			return nil
		}
		item, exists := currentNodes[id]
		if !exists {
			return ErrIncompleteState
		}
		destination[id] = item
		if !item.parent.Valid() {
			return nil
		}
		if _, known := baseNodes[item.parent]; known {
			return nil
		}
		id = item.parent
	}
}
