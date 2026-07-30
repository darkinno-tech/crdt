package crdt

import (
	"encoding/json"
	"strings"
)

// FrameType describes one fully implemented framed CRDT protocol. The type
// table is deliberately closed: reserving an ID alone must not make a payload
// eligible for batching or recovery before its concrete codec is available.
type FrameType struct {
	StateID          uint64
	DeltaID          uint64
	SemanticsVersion uint64
	UsesHLC          bool
}

// FrameTypeRegistration identifies one implemented state/delta protocol pair.
// It is diagnostic and negotiation metadata only: applications must still bind
// an authenticated manifest, authorization policy, and resource limits before
// accepting a frame.
type FrameTypeRegistration struct {
	Name string
	FrameType
}

// RegisteredFrameTypes returns a copy of every implemented protocol
// registration in stable registry order. Mutating the returned slice cannot
// affect protocol admission or frame decoding.
func RegisteredFrameTypes() []FrameTypeRegistration {
	registrations := make([]FrameTypeRegistration, len(frameTypeRegistrations))
	copy(registrations, frameTypeRegistrations[:])
	return registrations
}

// FrameTypeRegistrationForID returns the implemented registration containing
// typeID as either its state or delta frame ID. Reserved and unknown IDs return
// false. It performs no policy, manifest, or authentication decision.
func FrameTypeRegistrationForID(typeID uint64) (FrameTypeRegistration, bool) {
	for _, registration := range frameTypeRegistrations {
		if registration.StateID == typeID || registration.DeltaID == typeID {
			return registration, true
		}
	}
	return FrameTypeRegistration{}, false
}

// DefaultRGAFrameType returns the compact run-v2 protocol for new RGA
// replication groups. Legacy scalar RGA v1 frames remain a separately selected
// stable migration contract.
func DefaultRGAFrameType() FrameType {
	return FrameType{StateID: TypeIDRGARunState, DeltaID: TypeIDRGARunDelta, SemanticsVersion: SemanticsVersionRGARun, UsesHLC: true}
}

// ProtocolPolicy controls which implemented frame types one replication group
// advertises. It is a local, immutable-by-convention value for connection
// setup; it does not install a process-wide switch or permit runtime protocol
// registration.
//
// Peers must compare FrameTypes before sending state or deltas. A matching
// TypeID remains necessary but is not sufficient: applications still own
// authentication, authorization, limits, and decoder selection.
type ProtocolPolicy struct {
	// AllowExperimental is retained for source compatibility with releases that
	// required an opt-in for collection frames. Every implemented frame type is
	// stable now, so the field has no effect. It is not a substitute for an
	// authenticated manifest, authorization, limits, or tombstone retirement.
	//
	// Deprecated: all implemented protocol pairs are included by the zero value.
	AllowExperimental bool
}

// FrameTypes returns a copy of every protocol enabled by p. The returned slice
// is stable in type-ID order and safe for callers to advertise or modify.
func (p ProtocolPolicy) FrameTypes() []FrameType {
	types := make([]FrameType, 0, len(frameTypeRegistrations))
	for _, registration := range frameTypeRegistrations {
		kind := registration.FrameType
		if p.SupportsFrame(kind.StateID) {
			types = append(types, kind)
		}
	}
	return types
}

// SupportsFrame reports whether typeID is both implemented by this module and
// enabled by p. It applies to either a state or delta frame type ID.
func (p ProtocolPolicy) SupportsFrame(typeID uint64) bool {
	if _, ok := FrameTypeForState(typeID); !ok {
		if _, ok := FrameTypeForDelta(typeID); !ok {
			return false
		}
	}
	return true
}

// IsExperimentalFrame reports whether typeID belongs to an experimental
// protocol. No implemented protocol is experimental; reserved and unknown IDs
// also return false. It remains for source compatibility with earlier policy
// negotiation code.
//
// Deprecated: every implemented frame type is stable.
func IsExperimentalFrame(typeID uint64) bool {
	return false
}

// FrameTypeForState returns the supported protocol associated with stateID.
func FrameTypeForState(stateID uint64) (FrameType, bool) {
	for _, registration := range frameTypeRegistrations {
		kind := registration.FrameType
		if kind.StateID == stateID {
			return kind, true
		}
	}
	return FrameType{}, false
}

// FrameTypeForDelta returns the supported protocol associated with deltaID.
func FrameTypeForDelta(deltaID uint64) (FrameType, bool) {
	for _, registration := range frameTypeRegistrations {
		kind := registration.FrameType
		if kind.DeltaID == deltaID {
			return kind, true
		}
	}
	return FrameType{}, false
}

// CRDT is the common contract for state-based CRDTs.
//
// For every concrete state type T, Merge must be commutative, associative, and
// idempotent. If Merge returns an error, it must leave the receiver unchanged.
type CRDT[T any] interface {
	Merge(other T) error
	State() StateSnapshot
}

// DeltaCapable is implemented by a state-based CRDT that accepts a concrete,
// type-safe delta D. Delta mutators return D directly; the library does not
// maintain an implicitly acknowledged delta buffer.
type DeltaCapable[T any, D any] interface {
	CRDT[T]
	ApplyDelta(delta D) error
}

// StateSnapshot is an immutable summary of a CRDT state for diagnostics and
// observability. It never exposes mutable internal data.
type StateSnapshot struct {
	Type           string `json:"type"`
	ReplicaID      string `json:"replica_id"`
	ElementCount   int    `json:"element_count"`
	TombstoneCount int    `json:"tombstone_count"`
}

// StateReporter exposes an immutable CRDT diagnostic summary.
//
// It intentionally excludes application values, mutation tags, clock state,
// and framed bytes. Use it for observability only, never to persist or
// replicate a CRDT.
type StateReporter interface {
	State() StateSnapshot
}

// MarshalStateJSON returns a compact JSON diagnostic summary for value.
//
// This helper is intended for structured logs and human inspection. It does
// not encode CRDT state, deltas, or opaque application values, so its output
// cannot reconstruct a replica and must not be used as a wire format.
func MarshalStateJSON(value StateReporter) ([]byte, error) {
	return MarshalDiagnosticJSON(value.State())
}

// MarshalDiagnosticJSON encodes a caller-provided diagnostic summary. It is
// useful for CRDT delta log views that use the same schema as StateSnapshot.
// The summary must not contain application values or replication state.
func MarshalDiagnosticJSON(summary StateSnapshot) ([]byte, error) {
	return json.Marshal(summary)
}

// Tag uniquely identifies a CRDT mutation. WallTime, Logical, and ReplicaID
// are compared in that order. ReplicaID must be globally unique among live
// logical replicas; callers that reuse an ID across restarts must persist the
// last emitted clock state.
type Tag struct {
	ReplicaID string
	WallTime  uint64
	Logical   uint64
}

// Valid reports whether t is safe to use as a CRDT mutation identifier.
func (t Tag) Valid() bool {
	return strings.TrimSpace(t.ReplicaID) != ""
}

// Compare returns -1, 0, or 1 according to the canonical ordering of tags.
func (t Tag) Compare(other Tag) int {
	switch {
	case t.WallTime < other.WallTime:
		return -1
	case t.WallTime > other.WallTime:
		return 1
	case t.Logical < other.Logical:
		return -1
	case t.Logical > other.Logical:
		return 1
	case t.ReplicaID < other.ReplicaID:
		return -1
	case t.ReplicaID > other.ReplicaID:
		return 1
	default:
		return 0
	}
}
