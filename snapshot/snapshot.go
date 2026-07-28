// Package snapshot defines immutable, versioned CRDT state snapshots and
// bounded recovery plans.
package snapshot

import (
	"errors"

	"github.com/darkinno/crdt"
	"github.com/darkinno/crdt/clock"
	frame "github.com/darkinno/crdt/encoding"
)

var (
	ErrInvalid = errors.New("snapshot: invalid snapshot")
	ErrLimit   = errors.New("snapshot: recovery plan limit exceeded")
)

// StateValidator validates a type-specific canonical CRDT state frame. It is
// invoked once while a validated snapshot is created; a panic is treated as an
// invalid state.
type StateValidator func([]byte) error

// Snapshot holds one complete framed state payload and the full per-replica
// tag frontier known when it was saved. Its bytes and frontier are copied on
// both input and output. OR-Set snapshots may also carry the local HLC state
// needed to safely reuse a replica ID after recovery. Concrete CRDT decoders
// remain responsible for validating the type-specific payload before it is
// applied.
type Snapshot struct {
	FormatVersion uint64
	TypeID        uint64
	CodecID       string
	state         []byte
	frontier      map[string]crdt.Tag
	clockState    *clock.State
}

// New validates the state frame envelope and records a cloned frontier. It
// does not decode the type-specific payload; use NewValidated when a snapshot
// must be rejected before persistence unless its concrete CRDT state is valid.
func New(state []byte, frontier map[string]crdt.Tag) (Snapshot, error) {
	decoded, err := frame.UnmarshalFrame(state, frame.DefaultLimits())
	if err != nil || !isStateType(decoded.TypeID) || !validFrontier(frontier) {
		return Snapshot{}, ErrInvalid
	}
	return newSnapshot(decoded, state, frontier, nil), nil
}

// NewValidated creates a snapshot only when both the frame envelope and its
// type-specific state are valid according to validator.
func NewValidated(state []byte, frontier map[string]crdt.Tag, validator StateValidator) (Snapshot, error) {
	decoded, err := frame.UnmarshalFrame(state, frame.DefaultLimits())
	if err != nil || !isStateType(decoded.TypeID) || !validFrontier(frontier) || !validateState(validator, state) {
		return Snapshot{}, ErrInvalid
	}
	return newSnapshot(decoded, state, frontier, nil), nil
}

// NewWithClockState creates an OR-Set snapshot with the local HLC state that
// must be persisted atomically with the state frame before the replica ID is
// reused. G-Counter snapshots do not use an HLC and are rejected here.
func NewWithClockState(state []byte, frontier map[string]crdt.Tag, clockState clock.State) (Snapshot, error) {
	decoded, err := frame.UnmarshalFrame(state, frame.DefaultLimits())
	if err != nil || decoded.TypeID != crdt.TypeIDORSetState || !validFrontier(frontier) || !validClockState(clockState) {
		return Snapshot{}, ErrInvalid
	}
	return newSnapshot(decoded, state, frontier, &clockState), nil
}

// NewValidatedWithClockState creates an OR-Set snapshot only when its frame,
// clock state, and type-specific payload are valid. Use it when accepting an
// externally supplied OR-Set snapshot before persistence.
func NewValidatedWithClockState(state []byte, frontier map[string]crdt.Tag, clockState clock.State, validator StateValidator) (Snapshot, error) {
	decoded, err := frame.UnmarshalFrame(state, frame.DefaultLimits())
	if err != nil || decoded.TypeID != crdt.TypeIDORSetState || !validFrontier(frontier) || !validClockState(clockState) || !validateState(validator, state) {
		return Snapshot{}, ErrInvalid
	}
	return newSnapshot(decoded, state, frontier, &clockState), nil
}

func newSnapshot(decoded frame.Frame, state []byte, frontier map[string]crdt.Tag, clockState *clock.State) Snapshot {
	snapshot := Snapshot{
		FormatVersion: frame.FormatVersion,
		TypeID:        decoded.TypeID,
		CodecID:       decoded.CodecID,
		state:         append([]byte(nil), state...),
		frontier:      cloneFrontier(frontier),
	}
	if clockState != nil {
		cloned := *clockState
		snapshot.clockState = &cloned
	}
	return snapshot
}

// Bytes returns a copy of the canonical state frame.
func (s Snapshot) Bytes() []byte { return append([]byte(nil), s.state...) }

// Frontier returns a copy of the full per-replica tag frontier.
func (s Snapshot) Frontier() map[string]crdt.Tag { return cloneFrontier(s.frontier) }

// ClockState returns the local HLC state carried by an OR-Set snapshot. The
// boolean is false for snapshots without HLC state, including G-Counters and
// legacy snapshots created with New.
func (s Snapshot) ClockState() (clock.State, bool) {
	if s.clockState == nil {
		return clock.State{}, false
	}
	return *s.clockState, true
}

// RecoveryPlan contains a validated state snapshot followed by compatible
// encoded deltas. Applying Deltas in any order is safe because they are CRDT
// joins; callers decide persistence and retry policy.
type RecoveryPlan struct {
	Snapshot Snapshot
	deltas   [][]byte
}

// NewRecoveryPlan validates compatible delta frames and bounds their combined
// byte size before creating an immutable recovery plan.
func NewRecoveryPlan(snapshot Snapshot, deltas [][]byte, maxDeltaBytes int) (RecoveryPlan, error) {
	if maxDeltaBytes <= 0 || !snapshot.valid() {
		return RecoveryPlan{}, ErrInvalid
	}
	expectedTypeID, ok := deltaTypeForState(snapshot.TypeID)
	if !ok {
		return RecoveryPlan{}, ErrInvalid
	}
	total := 0
	cloned := make([][]byte, 0, len(deltas))
	for _, delta := range deltas {
		if len(delta) == 0 || len(delta) > maxDeltaBytes-total {
			return RecoveryPlan{}, ErrLimit
		}
		decoded, err := frame.UnmarshalFrame(delta, frame.DefaultLimits())
		if err != nil || decoded.TypeID != expectedTypeID || decoded.CodecID != snapshot.CodecID {
			return RecoveryPlan{}, ErrInvalid
		}
		cloned = append(cloned, append([]byte(nil), delta...))
		total += len(delta)
	}
	decodedSnapshot, err := frame.UnmarshalFrame(snapshot.state, frame.DefaultLimits())
	if err != nil {
		return RecoveryPlan{}, ErrInvalid
	}
	clonedSnapshot := newSnapshot(decodedSnapshot, snapshot.state, snapshot.frontier, snapshot.clockState)
	return RecoveryPlan{Snapshot: clonedSnapshot, deltas: cloned}, nil
}

// Deltas returns copies of the recovery delta frames.
func (p RecoveryPlan) Deltas() [][]byte {
	cloned := make([][]byte, len(p.deltas))
	for i, delta := range p.deltas {
		cloned[i] = append([]byte(nil), delta...)
	}
	return cloned
}

func (s Snapshot) valid() bool {
	decoded, err := frame.UnmarshalFrame(s.state, frame.DefaultLimits())
	if err != nil || s.FormatVersion != frame.FormatVersion || s.TypeID != decoded.TypeID ||
		s.CodecID != decoded.CodecID || !isStateType(s.TypeID) || !validFrontier(s.frontier) ||
		(s.clockState != nil && (s.TypeID != crdt.TypeIDORSetState || !validClockState(*s.clockState))) {
		return false
	}
	return true
}

func isStateType(typeID uint64) bool {
	return typeID == crdt.TypeIDGCounterState || typeID == crdt.TypeIDORSetState
}

func deltaTypeForState(typeID uint64) (uint64, bool) {
	switch typeID {
	case crdt.TypeIDGCounterState:
		return crdt.TypeIDGCounterDelta, true
	case crdt.TypeIDORSetState:
		return crdt.TypeIDORSetDelta, true
	default:
		return 0, false
	}
}

func validFrontier(frontier map[string]crdt.Tag) bool {
	for replicaID, tag := range frontier {
		if replicaID == "" || replicaID != tag.ReplicaID || !tag.Valid() {
			return false
		}
	}
	return true
}

func cloneFrontier(frontier map[string]crdt.Tag) map[string]crdt.Tag {
	clone := make(map[string]crdt.Tag, len(frontier))
	for replicaID, tag := range frontier {
		clone[replicaID] = tag
	}
	return clone
}

func validClockState(state clock.State) bool {
	_, err := clock.NewHLCFromState(state)
	return err == nil
}

func validateState(validator StateValidator, state []byte) (valid bool) {
	if validator == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			valid = false
		}
	}()
	return validator(append([]byte(nil), state...)) == nil
}
