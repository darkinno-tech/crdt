package tombstonegc

import (
	"errors"

	"github.com/im10furry/crdt"
)

var (
	// ErrMembershipGroupMismatch means an installed membership belongs to a
	// different replication group than the coordinator.
	ErrMembershipGroupMismatch = errors.New("tombstonegc: membership replication group mismatch")
	// ErrMembershipRollback means an installed membership would move the
	// coordinator to an earlier or conflicting membership epoch.
	ErrMembershipRollback = errors.New("tombstonegc: membership epoch rollback")
)

// NewCoordinatorAtMembership creates a coordinator at an externally assigned,
// persisted membership epoch. Callers must authenticate and durably record the
// membership view before constructing the coordinator. Unlike NewCoordinator,
// this constructor does not invent epoch 1 after a process restart.
func NewCoordinatorAtMembership[T comparable](membership Membership) (*Coordinator[T], error) {
	if membership.Epoch == 0 {
		return nil, ErrInvalidMembership
	}
	if membership.GroupID == "" {
		return nil, ErrInvalidGroup
	}
	members, err := validateMembers(membership.Members)
	if err != nil {
		return nil, err
	}
	return &Coordinator[T]{
		groupID:               membership.GroupID,
		epoch:                 membership.Epoch,
		members:               members,
		acknowledgements:      make(map[string]map[crdt.Tag]struct{}, len(members)),
		acknowledgementCounts: make(map[crdt.Tag]uint),
	}, nil
}

// InstallMembership installs an already authenticated, authoritative
// membership view. It accepts only a strictly newer epoch for the same group
// and clears all receipt state, so a receipt from an old view can never make a
// tombstone eligible for collection. Replaying the currently installed view is
// idempotent when its member set is identical.
func (c *Coordinator[T]) InstallMembership(membership Membership) error {
	if c == nil || membership.Epoch == 0 {
		return ErrInvalidMembership
	}
	members, err := validateMembers(membership.Members)
	if err != nil {
		return err
	}
	c.membershipMu.Lock()
	defer c.membershipMu.Unlock()
	if membership.GroupID != c.groupID {
		return ErrMembershipGroupMismatch
	}
	if membership.Epoch < c.epoch {
		return ErrMembershipRollback
	}
	if membership.Epoch == c.epoch {
		if sameMembers(c.members, members) {
			return nil
		}
		return ErrMembershipRollback
	}
	c.acknowledgementMu.Lock()
	defer c.acknowledgementMu.Unlock()
	c.epoch = membership.Epoch
	c.members = members
	c.acknowledgements = make(map[string]map[crdt.Tag]struct{}, len(members))
	c.acknowledgementCounts = make(map[crdt.Tag]uint)
	c.acknowledgementEntries = 0
	return nil
}

func sameMembers(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for member := range left {
		if _, ok := right[member]; !ok {
			return false
		}
	}
	return true
}
