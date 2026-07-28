// Package crdt defines the common types and contracts used by the CRDT
// primitives in this module.
package crdt

import "strings"

// Stable frame type assignments. Values are part of the v1 wire contract and
// must never be reused for a different payload shape.
const (
	TypeIDGCounterState  uint64 = 1
	TypeIDORSetState     uint64 = 2
	TypeIDGCounterDelta  uint64 = 3
	TypeIDORSetDelta     uint64 = 4
	TypeIDPNCounterState uint64 = 5
	TypeIDPNCounterDelta uint64 = 6
	TypeIDLWWSetState    uint64 = 7
	TypeIDLWWSetDelta    uint64 = 8
	TypeIDLWWMapState    uint64 = 9
	TypeIDLWWMapDelta    uint64 = 10
	TypeIDRGAState       uint64 = 11
	TypeIDRGADelta       uint64 = 12
	TypeIDORTreeState    uint64 = 17
	TypeIDORTreeDelta    uint64 = 18
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
	{StateID: TypeIDRGAState, DeltaID: TypeIDRGADelta, UsesHLC: true},
	{StateID: TypeIDORTreeState, DeltaID: TypeIDORTreeDelta, UsesHLC: true},
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
	Type           string
	ReplicaID      string
	ElementCount   int
	TombstoneCount int
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
