// Package crdt defines the common types and contracts used by the CRDT
// primitives in this module.
package crdt

import (
	"encoding/json"
	"strings"
)

// Stable frame type assignments. Values are part of the v1 wire contract and
// must never be reused for a different payload shape.
const (
	TypeIDGCounterState   uint64 = 1
	TypeIDORSetState      uint64 = 2
	TypeIDGCounterDelta   uint64 = 3
	TypeIDORSetDelta      uint64 = 4
	TypeIDPNCounterState  uint64 = 5
	TypeIDPNCounterDelta  uint64 = 6
	TypeIDLWWSetState     uint64 = 7
	TypeIDLWWSetDelta     uint64 = 8
	TypeIDLWWMapState     uint64 = 9
	TypeIDLWWMapDelta     uint64 = 10
	TypeIDRGAState        uint64 = 11
	TypeIDRGADelta        uint64 = 12
	TypeIDGSetState       uint64 = 13
	TypeIDGSetDelta       uint64 = 14
	TypeIDMVRegisterState uint64 = 15
	TypeIDMVRegisterDelta uint64 = 16
	TypeIDORTreeState     uint64 = 17
	TypeIDORTreeDelta     uint64 = 18
	// RGA run frames retain scalar Position semantics while compacting linear
	// same-replica insertion chains. They are separately negotiated v2 wire
	// shapes; TypeIDRGAState and TypeIDRGADelta remain immutable v1 contracts.
	TypeIDRGARunState uint64 = 19
	TypeIDRGARunDelta uint64 = 20
)

// FrameType describes one fully implemented framed CRDT protocol. The type
// table is deliberately closed: reserving an ID alone must not make a payload
// eligible for batching or recovery before its concrete codec is available.
type FrameType struct {
	StateID uint64
	DeltaID uint64
	UsesHLC bool
}

var frameTypes = [...]FrameType{
	{StateID: TypeIDGCounterState, DeltaID: TypeIDGCounterDelta},
	{StateID: TypeIDORSetState, DeltaID: TypeIDORSetDelta, UsesHLC: true},
	{StateID: TypeIDPNCounterState, DeltaID: TypeIDPNCounterDelta},
	{StateID: TypeIDLWWMapState, DeltaID: TypeIDLWWMapDelta, UsesHLC: true},
	{StateID: TypeIDRGAState, DeltaID: TypeIDRGADelta, UsesHLC: true},
	{StateID: TypeIDGSetState, DeltaID: TypeIDGSetDelta},
	{StateID: TypeIDMVRegisterState, DeltaID: TypeIDMVRegisterDelta},
	{StateID: TypeIDORTreeState, DeltaID: TypeIDORTreeDelta, UsesHLC: true},
	{StateID: TypeIDRGARunState, DeltaID: TypeIDRGARunDelta, UsesHLC: true},
}

// experimentalFrameTypes are fully framed protocols whose public API and
// tombstone-lifecycle guidance are still evolving. They are never enabled by
// ProtocolPolicy's zero value, so an application must opt in per replication
// group before advertising or accepting them from a peer.
var experimentalFrameTypes = map[uint64]struct{}{
	TypeIDLWWMapState: {},
	TypeIDLWWMapDelta: {},
	TypeIDRGAState:    {},
	TypeIDRGADelta:    {},
	TypeIDORTreeState: {},
	TypeIDORTreeDelta: {},
	TypeIDRGARunState: {},
	TypeIDRGARunDelta: {},
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
	// AllowExperimental includes framed LWW-Map, RGA run-v2, and OR-Tree protocols.
	// Keep it false until the replication group has accepted their experimental
	// API and tombstone-retention lifecycle.
	AllowExperimental bool
}

// FrameTypes returns a copy of every protocol enabled by p. The returned slice
// is stable in type-ID order and safe for callers to advertise or modify.
func (p ProtocolPolicy) FrameTypes() []FrameType {
	types := make([]FrameType, 0, len(frameTypes))
	for _, kind := range frameTypes {
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
	_, experimental := experimentalFrameTypes[typeID]
	return p.AllowExperimental || !experimental
}

// IsExperimentalFrame reports whether typeID belongs to an implemented
// experimental protocol. Reserved or unknown type IDs return false.
func IsExperimentalFrame(typeID uint64) bool {
	_, ok := experimentalFrameTypes[typeID]
	return ok
}

// FrameTypeForState returns the supported protocol associated with stateID.
func FrameTypeForState(stateID uint64) (FrameType, bool) {
	for _, kind := range frameTypes {
		if kind.StateID == stateID {
			return kind, true
		}
	}
	return FrameType{}, false
}

// FrameTypeForDelta returns the supported protocol associated with deltaID.
func FrameTypeForDelta(deltaID uint64) (FrameType, bool) {
	for _, kind := range frameTypes {
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
