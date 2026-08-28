// Package clock implements a hybrid logical clock for CRDT mutation tags.
package clock

import (
	"errors"
	"math"
	"sync"
	"time"

	"github.com/im10furry/crdt"
)

var (
	// ErrInvalidReplicaID indicates that a clock was created without a usable
	// logical replica identifier.
	ErrInvalidReplicaID = errors.New("clock: invalid replica ID")
	// ErrInvalidRemoteTag indicates that Witness received an invalid tag.
	ErrInvalidRemoteTag = errors.New("clock: invalid remote tag")
	// ErrClockExhausted indicates that the complete tag space is exhausted.
	ErrClockExhausted = errors.New("clock: timestamp space exhausted")
)

// State is the persistable portion of an HLC. Persist it atomically after a
// successful Now call before reusing the same ReplicaID across a restart.
type State struct {
	ReplicaID string
	WallTime  uint64
	Logical   uint64
}

// HLC generates monotonically ordered tags for one logical replica.
//
// A caller that reuses its replica ID after a process restart must persist and
// restore the clock state. Otherwise it must use a fresh replica ID.
type HLC struct {
	mu        sync.Mutex
	replicaID string
	wallTime  uint64
	logical   uint64
	now       func() time.Time
}

// NewHLC creates a clock for replicaID.
func NewHLC(replicaID string) (*HLC, error) {
	return NewHLCFromState(State{ReplicaID: replicaID})
}

// NewHLCFromState restores a clock that previously used State.ReplicaID.
func NewHLCFromState(state State) (*HLC, error) {
	if !(crdt.Tag{ReplicaID: state.ReplicaID}).Valid() {
		return nil, ErrInvalidReplicaID
	}

	return &HLC{
		replicaID: state.ReplicaID,
		wallTime:  state.WallTime,
		logical:   state.Logical,
		now:       time.Now,
	}, nil
}

// ReplicaID returns the logical replica ID associated with h.
func (h *HLC) ReplicaID() string {
	if h == nil {
		return ""
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	return h.replicaID
}

// Snapshot returns the persistable state needed to safely reuse h's replica
// ID after restart.
func (h *HLC) Snapshot() State {
	if h == nil {
		return State{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return State{ReplicaID: h.replicaID, WallTime: h.wallTime, Logical: h.logical}
}

// Now returns a fresh, monotonically ordered local tag.
func (h *HLC) Now() (crdt.Tag, error) {
	if h == nil {
		return crdt.Tag{}, ErrClockExhausted
	}

	physical, err := h.physicalMillis()
	if err != nil {
		return crdt.Tag{}, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	return h.advanceLocal(physical)
}

// Witness incorporates remote into the local clock. It never returns remote
// as a local tag; subsequent calls to Now use h's replica ID.
func (h *HLC) Witness(remote crdt.Tag) error {
	if h == nil {
		return ErrClockExhausted
	}
	if !remote.Valid() {
		return ErrInvalidRemoteTag
	}

	physical, err := h.physicalMillis()
	if err != nil {
		return err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	maxWall := max(h.wallTime, remote.WallTime, physical)
	switch {
	case maxWall == h.wallTime && maxWall == remote.WallTime:
		wallTime, logical, err := nextTimestamp(maxWall, max(h.logical, remote.Logical))
		if err != nil {
			return err
		}
		h.wallTime = wallTime
		h.logical = logical
	case maxWall == h.wallTime:
		wallTime, logical, err := nextTimestamp(h.wallTime, h.logical)
		if err != nil {
			return err
		}
		h.wallTime = wallTime
		h.logical = logical
	case maxWall == remote.WallTime:
		wallTime, logical, err := nextTimestamp(remote.WallTime, remote.Logical)
		if err != nil {
			return err
		}
		h.wallTime = wallTime
		h.logical = logical
	default:
		h.wallTime = maxWall
		h.logical = 0
	}

	return nil
}

func (h *HLC) advanceLocal(physical uint64) (crdt.Tag, error) {
	if physical > h.wallTime {
		h.wallTime = physical
		h.logical = 0
	} else {
		wallTime, logical, err := nextTimestamp(h.wallTime, h.logical)
		if err != nil {
			return crdt.Tag{}, err
		}
		h.wallTime = wallTime
		h.logical = logical
	}

	return crdt.Tag{
		ReplicaID: h.replicaID,
		WallTime:  h.wallTime,
		Logical:   h.logical,
	}, nil
}

func (h *HLC) physicalMillis() (uint64, error) {
	if h.now == nil {
		return 0, ErrClockExhausted
	}

	millis := h.now().UnixMilli()
	if millis < 0 {
		return 0, ErrClockExhausted
	}
	return uint64(millis), nil
}

func nextTimestamp(wallTime, logical uint64) (uint64, uint64, error) {
	if logical != math.MaxUint64 {
		return wallTime, logical + 1, nil
	}
	if wallTime == math.MaxUint64 {
		return 0, 0, ErrClockExhausted
	}
	return wallTime + 1, 0, nil
}
