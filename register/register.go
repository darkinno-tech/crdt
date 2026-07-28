// Package register implements state-based register CRDTs.
package register

import (
	"bytes"
	"errors"
	"sync"

	"github.com/darkinno/crdt"
	"github.com/darkinno/crdt/clock"
)

var (
	ErrInvalidReplicaID = errors.New("register: invalid replica ID")
	ErrNilLWW           = errors.New("register: nil LWW register")
	ErrNilMax           = errors.New("register: nil max register")
	ErrTagConflict      = errors.New("register: conflicting value for one tag")
)

// LWW stores an opaque byte value selected by the greatest HLC tag. Values are
// copied at both API boundaries so callers cannot mutate converged state.
type LWW struct {
	mu        sync.RWMutex
	clock     *clock.HLC
	replicaID string
	tag       crdt.Tag
	value     []byte
	hasValue  bool
}

var _ crdt.CRDT[*LWW] = (*LWW)(nil)

func NewLWW(replicaID string) (*LWW, error) {
	return NewLWWFromClock(clock.State{ReplicaID: replicaID})
}
func NewLWWFromClock(state clock.State) (*LWW, error) {
	if !(crdt.Tag{ReplicaID: state.ReplicaID}).Valid() {
		return nil, ErrInvalidReplicaID
	}
	hlc, err := clock.NewHLCFromState(state)
	if err != nil {
		return nil, err
	}
	return &LWW{clock: hlc, replicaID: state.ReplicaID}, nil
}
func (r *LWW) ClockState() clock.State {
	if r == nil || r.clock == nil {
		return clock.State{}
	}
	return r.clock.Snapshot()
}
func (r *LWW) Set(value []byte) error {
	if r == nil || r.clock == nil {
		return ErrNilLWW
	}
	tag, err := r.clock.Now()
	if err != nil {
		return err
	}
	r.mu.Lock()
	if !r.hasValue || r.tag.Compare(tag) < 0 {
		r.tag, r.value, r.hasValue = tag, append([]byte(nil), value...), true
	}
	r.mu.Unlock()
	return nil
}
func (r *LWW) Get() ([]byte, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.hasValue {
		return nil, false
	}
	return append([]byte(nil), r.value...), true
}
func (r *LWW) Merge(other *LWW) error {
	if r == nil || other == nil {
		return ErrNilLWW
	}
	if r == other {
		return nil
	}
	other.mu.RLock()
	tag, value, hasValue := other.tag, append([]byte(nil), other.value...), other.hasValue
	other.mu.RUnlock()
	if !hasValue {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hasValue && r.tag == tag && !bytes.Equal(r.value, value) {
		return ErrTagConflict
	}
	if err := r.clock.Witness(tag); err != nil {
		return err
	}
	if !r.hasValue || r.tag.Compare(tag) < 0 {
		r.tag, r.value, r.hasValue = tag, value, true
	}
	return nil
}
func (r *LWW) State() crdt.StateSnapshot {
	if r == nil {
		return crdt.StateSnapshot{Type: "lww-register"}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	if r.hasValue {
		count = 1
	}
	return crdt.StateSnapshot{Type: "lww-register", ReplicaID: r.replicaID, ElementCount: count}
}

// Max is a grow-only register. A write below its current value is a no-op;
// Merge takes the maximum, which is associative, commutative, and idempotent.
type Max struct {
	mu       sync.RWMutex
	value    uint64
	hasValue bool
}

var _ crdt.CRDT[*Max] = (*Max)(nil)

func NewMax() *Max { return &Max{} }
func (r *Max) Set(value uint64) error {
	if r == nil {
		return ErrNilMax
	}
	r.mu.Lock()
	if !r.hasValue || value > r.value {
		r.value, r.hasValue = value, true
	}
	r.mu.Unlock()
	return nil
}
func (r *Max) Get() (uint64, bool) {
	if r == nil {
		return 0, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.value, r.hasValue
}
func (r *Max) Merge(other *Max) error {
	if r == nil || other == nil {
		return ErrNilMax
	}
	if r == other {
		return nil
	}
	other.mu.RLock()
	value, hasValue := other.value, other.hasValue
	other.mu.RUnlock()
	if !hasValue {
		return nil
	}
	return r.Set(value)
}
func (r *Max) State() crdt.StateSnapshot {
	if r == nil {
		return crdt.StateSnapshot{Type: "max-register"}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	if r.hasValue {
		count = 1
	}
	return crdt.StateSnapshot{Type: "max-register", ElementCount: count}
}
