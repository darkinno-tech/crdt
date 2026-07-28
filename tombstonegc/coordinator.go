// Package tombstonegc coordinates safe, automatic OR-Set tombstone collection.
// It deliberately does not provide membership discovery, authentication, or
// persistence; applications must supply an authoritative membership view and
// authenticate acknowledgement messages before passing them to Coordinator.
package tombstonegc

import (
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/set"
)

var (
	ErrInvalidMembership = errors.New("tombstonegc: invalid membership")
	ErrInvalidGroup      = errors.New("tombstonegc: invalid replication group")
	ErrGroupMismatch     = errors.New("tombstonegc: acknowledgement replication group mismatch")
	ErrUnknownMember     = errors.New("tombstonegc: acknowledgement from unknown member")
	ErrStaleMembership   = errors.New("tombstonegc: acknowledgement membership epoch is stale")
	ErrInvalidTag        = errors.New("tombstonegc: invalid tombstone tag")
)

// Membership is an immutable view of the active replicas that must
// acknowledge a tombstone before it can be removed.
type Membership struct {
	GroupID string
	Epoch   uint64
	Members []string
}

// Coordinator collects exact tombstone acknowledgements for one replicated
// OR-Set. Its state is deliberately fail-closed: a restart or membership
// replacement clears acknowledgements and can delay collection, but cannot
// make a tombstone eligible prematurely.
//
// Removing a member is only safe after the application has retired it from the
// replication protocol. A removed member that later reconnects must bootstrap
// from a snapshot created after the compaction it missed.
type Coordinator[T comparable] struct {
	mu               sync.Mutex
	groupID          string
	epoch            uint64
	members          map[string]struct{}
	acknowledgements map[string]map[crdt.Tag]struct{}
}

// NewCoordinator creates a coordinator for groupID with the initial active
// membership. groupID must uniquely name this OR-Set replication group and be
// included in authenticated acknowledgement messages. At least one unique,
// non-blank member is required.
func NewCoordinator[T comparable](groupID string, members []string) (*Coordinator[T], error) {
	if strings.TrimSpace(groupID) == "" {
		return nil, ErrInvalidGroup
	}
	memberSet, err := validateMembers(members)
	if err != nil {
		return nil, err
	}
	return &Coordinator[T]{
		groupID:          groupID,
		epoch:            1,
		members:          memberSet,
		acknowledgements: make(map[string]map[crdt.Tag]struct{}, len(memberSet)),
	}, nil
}

// Membership returns a sorted, immutable copy of the active membership view.
func (c *Coordinator[T]) Membership() Membership {
	if c == nil {
		return Membership{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return Membership{GroupID: c.groupID, Epoch: c.epoch, Members: sortedMembers(c.members)}
}

// ReplaceMembership installs an authoritative new membership view. It clears
// all collected acknowledgements and increments the epoch, so reports from an
// earlier view cannot make any tombstone eligible for collection.
func (c *Coordinator[T]) ReplaceMembership(members []string) (Membership, error) {
	if c == nil {
		return Membership{}, ErrInvalidMembership
	}
	memberSet, err := validateMembers(members)
	if err != nil {
		return Membership{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epoch == ^uint64(0) {
		return Membership{}, ErrInvalidMembership
	}
	c.epoch++
	c.members = memberSet
	c.acknowledgements = make(map[string]map[crdt.Tag]struct{}, len(memberSet))
	return Membership{GroupID: c.groupID, Epoch: c.epoch, Members: sortedMembers(memberSet)}, nil
}

// Acknowledge records the exact tombstones present at one member under the
// supplied group ID and membership epoch. Repeated reports are idempotently
// unioned.
func (c *Coordinator[T]) Acknowledge(groupID, member string, epoch uint64, tombstones []crdt.Tag) error {
	if c == nil {
		return ErrInvalidMembership
	}
	for _, tag := range tombstones {
		if !tag.Valid() {
			return ErrInvalidTag
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if groupID != c.groupID {
		return ErrGroupMismatch
	}
	if epoch != c.epoch {
		return ErrStaleMembership
	}
	if _, ok := c.members[member]; !ok {
		return ErrUnknownMember
	}
	if c.acknowledgements[member] == nil {
		c.acknowledgements[member] = make(map[crdt.Tag]struct{}, len(tombstones))
	}
	for _, tag := range tombstones {
		c.acknowledgements[member][tag] = struct{}{}
	}
	return nil
}

// AcknowledgeAndCompact records one exact acknowledgement and immediately
// removes from target only tombstones acknowledged by every current member.
// It is the normal receive-path operation for automatic collection.
func (c *Coordinator[T]) AcknowledgeAndCompact(groupID, member string, epoch uint64, tombstones []crdt.Tag, target *set.ORSet[T]) (int, error) {
	if target == nil {
		return 0, set.ErrNilORSet
	}
	if err := c.Acknowledge(groupID, member, epoch, tombstones); err != nil {
		return 0, err
	}
	return target.CompactTombstones(c.stableTombstones(target.TombstoneTags()))
}

// AcknowledgeSetAndCompact is AcknowledgeAndCompact for a member's complete
// current OR-Set state. The state must be a verified representation received
// from that member, not an inferred frontier.
func (c *Coordinator[T]) AcknowledgeSetAndCompact(groupID, member string, epoch uint64, acknowledged *set.ORSet[T], target *set.ORSet[T]) (int, error) {
	if acknowledged == nil {
		return 0, set.ErrNilORSet
	}
	return c.AcknowledgeAndCompact(groupID, member, epoch, acknowledged.TombstoneTags(), target)
}

func (c *Coordinator[T]) stableTombstones(candidates []crdt.Tag) []crdt.Tag {
	if c == nil || len(candidates) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	stable := make([]crdt.Tag, 0, len(candidates))
	for _, tag := range candidates {
		acknowledgedByAll := true
		for member := range c.members {
			if _, ok := c.acknowledgements[member][tag]; !ok {
				acknowledgedByAll = false
				break
			}
		}
		if acknowledgedByAll {
			stable = append(stable, tag)
		}
	}
	return stable
}

func validateMembers(members []string) (map[string]struct{}, error) {
	if len(members) == 0 {
		return nil, ErrInvalidMembership
	}
	memberSet := make(map[string]struct{}, len(members))
	for _, member := range members {
		if strings.TrimSpace(member) == "" {
			return nil, ErrInvalidMembership
		}
		if _, duplicate := memberSet[member]; duplicate {
			return nil, ErrInvalidMembership
		}
		memberSet[member] = struct{}{}
	}
	return memberSet, nil
}

func sortedMembers(memberSet map[string]struct{}) []string {
	members := make([]string, 0, len(memberSet))
	for member := range memberSet {
		members = append(members, member)
	}
	sort.Strings(members)
	return members
}
