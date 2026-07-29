// Package tombstonegc coordinates safe, automatic tombstone collection.
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
	ErrNilTarget         = errors.New("tombstonegc: nil tombstone target")
)

// Membership is an immutable view of the active replicas that must
// acknowledge a tombstone before it can be removed.
type Membership struct {
	GroupID string
	Epoch   uint64
	Members []string
}

// AcknowledgementStats is a point-in-time summary for setting application GC
// policy. Entries counts unique member/tag acknowledgements; Tags counts the
// tags with at least one retained acknowledgement.
type AcknowledgementStats struct {
	GroupID string
	Epoch   uint64
	Members int
	Tags    int
	Entries int
}

// TombstoneTarget is a CRDT whose tombstones can be enumerated and compacted.
// Coordinator only establishes exact acknowledgement eligibility; targets must
// enforce their own structural compaction rules.
type TombstoneTarget interface {
	TombstoneTags() []crdt.Tag
	CompactTombstones([]crdt.Tag) (int, error)
}

// eligibleTombstoneTarget is an optional capability for targets whose
// structural rules can make only a subset of an exact-acknowledged batch
// removable. It must never remove a tag that is not safe for that target.
//
// RGA uses this to make leaf-to-root progress when one batch contains both a
// deleted structural ancestor and its deleted descendants. Targets that do
// not implement it retain the all-or-nothing CompactTombstones behavior.
type eligibleTombstoneTarget interface {
	CompactEligibleTombstones([]crdt.Tag) (int, error)
}

// Coordinator collects exact tombstone acknowledgements for one replicated
// target. Its state is deliberately fail-closed: a restart or membership
// replacement clears acknowledgements and can delay collection, but cannot
// make a tombstone eligible prematurely.
//
// Removing a member is only safe after the application has retired it from the
// replication protocol. A removed member that later reconnects must bootstrap
// from a snapshot created after the compaction it missed.
type Coordinator[T comparable] struct {
	membershipMu           sync.RWMutex
	acknowledgementMu      sync.Mutex
	groupID                string
	epoch                  uint64
	members                map[string]struct{}
	acknowledgements       map[string]map[crdt.Tag]struct{}
	acknowledgementCounts  map[crdt.Tag]uint
	acknowledgementEntries int
}

// NewCoordinator creates a coordinator for groupID with the initial active
// membership. groupID must uniquely name this replication group and be
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
		groupID:               groupID,
		epoch:                 1,
		members:               memberSet,
		acknowledgements:      make(map[string]map[crdt.Tag]struct{}, len(memberSet)),
		acknowledgementCounts: make(map[crdt.Tag]uint),
	}, nil
}

// Membership returns a sorted, immutable copy of the active membership view.
func (c *Coordinator[T]) Membership() Membership {
	if c == nil {
		return Membership{}
	}
	c.membershipMu.RLock()
	defer c.membershipMu.RUnlock()
	return Membership{GroupID: c.groupID, Epoch: c.epoch, Members: sortedMembers(c.members)}
}

// AcknowledgementStats returns counts suitable for monitoring acknowledgement
// memory and deciding when to persist a post-compaction snapshot and prune
// records. It returns an empty value for a nil coordinator.
func (c *Coordinator[T]) AcknowledgementStats() AcknowledgementStats {
	if c == nil {
		return AcknowledgementStats{}
	}
	c.membershipMu.RLock()
	defer c.membershipMu.RUnlock()
	c.acknowledgementMu.Lock()
	defer c.acknowledgementMu.Unlock()
	return AcknowledgementStats{
		GroupID: c.groupID,
		Epoch:   c.epoch,
		Members: len(c.members),
		Tags:    len(c.acknowledgementCounts),
		Entries: c.acknowledgementEntries,
	}
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
	c.membershipMu.Lock()
	defer c.membershipMu.Unlock()
	if c.epoch == ^uint64(0) {
		return Membership{}, ErrInvalidMembership
	}
	c.acknowledgementMu.Lock()
	defer c.acknowledgementMu.Unlock()
	c.epoch++
	c.members = memberSet
	c.acknowledgements = make(map[string]map[crdt.Tag]struct{}, len(memberSet))
	c.acknowledgementCounts = make(map[crdt.Tag]uint)
	c.acknowledgementEntries = 0
	return Membership{GroupID: c.groupID, Epoch: c.epoch, Members: sortedMembers(memberSet)}, nil
}

// Acknowledge records the exact tombstones present at one member under the
// supplied group ID and membership epoch. Repeated reports are idempotently
// unioned.
func (c *Coordinator[T]) Acknowledge(groupID, member string, epoch uint64, tombstones []crdt.Tag) error {
	if c == nil {
		return ErrInvalidMembership
	}
	if err := validateTombstones(tombstones); err != nil {
		return err
	}
	c.membershipMu.RLock()
	defer c.membershipMu.RUnlock()
	c.acknowledgementMu.Lock()
	defer c.acknowledgementMu.Unlock()
	return c.acknowledgeLocked(groupID, member, epoch, tombstones)
}

// PruneAcknowledgements removes retained acknowledgement proofs for tags in
// the current membership epoch. Call it only after the local compacted target
// has been durably snapshotted. Pruning is fail-closed: it can delay a later
// compaction until members report again, but it cannot make a tag eligible.
//
// A Coordinator shared by multiple local targets must not prune a tag until
// every target that relies on this Coordinator has compacted it or can safely
// wait for another full acknowledgement cycle. The return value is the number
// of member/tag acknowledgement records removed.
func (c *Coordinator[T]) PruneAcknowledgements(groupID string, epoch uint64, tags []crdt.Tag) (int, error) {
	if c == nil {
		return 0, ErrInvalidMembership
	}
	if err := validateTombstones(tags); err != nil {
		return 0, err
	}
	c.membershipMu.RLock()
	defer c.membershipMu.RUnlock()
	c.acknowledgementMu.Lock()
	defer c.acknowledgementMu.Unlock()
	if groupID != c.groupID {
		return 0, ErrGroupMismatch
	}
	if epoch != c.epoch {
		return 0, ErrStaleMembership
	}
	if c.prunesAllAcknowledgementTagsLocked(tags) {
		removed := c.acknowledgementEntries
		c.acknowledgements = make(map[string]map[crdt.Tag]struct{}, len(c.members))
		c.acknowledgementCounts = make(map[crdt.Tag]uint)
		c.acknowledgementEntries = 0
		return removed, nil
	}
	removed := 0
	for _, tag := range tags {
		for _, acknowledged := range c.acknowledgements {
			if _, exists := acknowledged[tag]; !exists {
				continue
			}
			delete(acknowledged, tag)
			removed++
		}
		delete(c.acknowledgementCounts, tag)
	}
	c.acknowledgementEntries -= removed
	return removed, nil
}

// prunesAllAcknowledgementTagsLocked reports whether tags cover exactly the
// current acknowledgement table. The caller must hold membershipMu and
// acknowledgementMu. After this O(tags) coverage check, a full
// post-checkpoint prune can discard the whole table without deleting every
// member/tag pair individually.
func (c *Coordinator[T]) prunesAllAcknowledgementTagsLocked(tags []crdt.Tag) bool {
	if len(tags) < len(c.acknowledgementCounts) {
		return false
	}
	seen := make(map[crdt.Tag]struct{}, len(tags))
	for _, tag := range tags {
		if _, exists := c.acknowledgementCounts[tag]; !exists {
			return false
		}
		seen[tag] = struct{}{}
	}
	return len(seen) == len(c.acknowledgementCounts)
}

// acknowledgeLocked requires membershipMu and acknowledgementMu to be held.
func (c *Coordinator[T]) acknowledgeLocked(groupID, member string, epoch uint64, tombstones []crdt.Tag) error {
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
		if _, alreadyAcknowledged := c.acknowledgements[member][tag]; alreadyAcknowledged {
			continue
		}
		c.acknowledgements[member][tag] = struct{}{}
		c.acknowledgementCounts[tag]++
		c.acknowledgementEntries++
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
	return c.AcknowledgeAndCompactTarget(groupID, member, epoch, tombstones, target)
}

// AcknowledgeAndCompactTarget records one exact acknowledgement and immediately
// removes from target only tombstones acknowledged by every current member.
// It reuses the same fail-closed membership epoch as OR-Set collection, but it
// cannot prove that a post-compaction checkpoint was persisted or that old
// deltas were retired; callers must do both before accepting compaction.
func (c *Coordinator[T]) AcknowledgeAndCompactTarget(groupID, member string, epoch uint64, tombstones []crdt.Tag, target TombstoneTarget) (int, error) {
	if target == nil {
		return 0, ErrNilTarget
	}
	if c == nil {
		return 0, ErrInvalidMembership
	}
	if err := validateTombstones(tombstones); err != nil {
		return 0, err
	}

	// Hold the membership read lock through target compaction so
	// ReplaceMembership cannot install a new epoch between deciding that a tag
	// is stable and deleting that tag. Keep acknowledgement locking narrower:
	// ORSet sorting and compaction do not block independent acknowledgement
	// writes. The lock order is membershipMu before acknowledgementMu.
	c.membershipMu.RLock()
	defer c.membershipMu.RUnlock()
	candidates := target.TombstoneTags()
	c.acknowledgementMu.Lock()
	if err := c.acknowledgeLocked(groupID, member, epoch, tombstones); err != nil {
		c.acknowledgementMu.Unlock()
		return 0, err
	}
	stable := c.stableTombstonesLocked(candidates)
	c.acknowledgementMu.Unlock()
	if target, ok := target.(eligibleTombstoneTarget); ok {
		return target.CompactEligibleTombstones(stable)
	}
	return target.CompactTombstones(stable)
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

// stableTombstonesLocked requires membershipMu and acknowledgementMu to be held.
func (c *Coordinator[T]) stableTombstonesLocked(candidates []crdt.Tag) []crdt.Tag {
	if len(candidates) == 0 {
		return nil
	}
	stable := make([]crdt.Tag, 0, len(candidates))
	for _, tag := range candidates {
		if c.acknowledgementCounts[tag] == uint(len(c.members)) {
			stable = append(stable, tag)
		}
	}
	return stable
}

func validateTombstones(tombstones []crdt.Tag) error {
	for _, tag := range tombstones {
		if !tag.Valid() {
			return ErrInvalidTag
		}
	}
	return nil
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
