package tombstonegc

import (
	"errors"
	"sort"

	"github.com/darkinno-tech/crdt"
)

const (
	// DefaultSimpleMinRetained is the number of tombstone identities kept by
	// DefaultSimplePolicy before simple collection begins. It limits neither
	// total state nor transport history; the caller remains responsible for
	// scheduling collection and for the target's own resource limits.
	DefaultSimpleMinRetained = 256

	// DefaultSimpleMaxBatch is the maximum number of tombstone identities that
	// DefaultSimplePolicy asks a target to compact in one call.
	DefaultSimpleMaxBatch = 64

	// MaxSimpleBatch bounds one SimpleCollector compaction request. It protects
	// a local cleanup job from accidentally turning a large retained set into an
	// unbounded mutation. Tombstone enumeration is still target-defined.
	MaxSimpleBatch = 8192
)

var (
	ErrInvalidSimplePolicy = errors.New("tombstonegc: invalid simple collection policy")
	ErrNilSimpleCollector  = errors.New("tombstonegc: nil simple collector")
)

// SimplePolicy bounds local-only collection. MinRetained is a count, not a
// time-based retention promise: TombstoneTags are ordered by CRDT tag, and a
// tag's order need not equal the time at which its deletion was observed.
//
// SimplePolicy must only be used for disposable, single-authority state where
// every earlier delta has been retired before Collect runs. Typical examples
// are a local recommendation cache or a server-owned derived default that is
// rebuilt from current source data. It must not be used for a replicated CRDT,
// an offline client, a durable outbox, or any target that could merge delayed
// operations. Use Coordinator for those cases.
type SimplePolicy struct {
	// MinRetained keeps this many canonical tombstone identities before
	// collection starts. It may retain more when a target cannot safely compact
	// a structural tombstone yet.
	MinRetained int
	// MaxBatch caps the number of selected identities per Collect call.
	MaxBatch int
}

// DefaultSimplePolicy returns the recommended bounded policy when the caller
// has already established that local-only collection is appropriate. It does
// not change Coordinator, which remains the default for replicated data.
func DefaultSimplePolicy() SimplePolicy {
	return SimplePolicy{
		MinRetained: DefaultSimpleMinRetained,
		MaxBatch:    DefaultSimpleMaxBatch,
	}
}

// SimpleCollector performs bounded local-only tombstone collection without
// membership receipts. Creating one is an explicit acknowledgement that the
// caller, not this package, has established the local-only lifecycle stated by
// SimplePolicy. It is immutable and safe for concurrent calls when its target
// is safe for concurrent calls.
type SimpleCollector struct {
	policy SimplePolicy
}

// NewSimpleCollector validates a local-only collection policy. The zero value
// is rejected so callers must deliberately choose DefaultSimplePolicy or name
// their retention and batch limits.
func NewSimpleCollector(policy SimplePolicy) (*SimpleCollector, error) {
	if policy.MinRetained < 0 || policy.MaxBatch <= 0 || policy.MaxBatch > MaxSimpleBatch {
		return nil, ErrInvalidSimplePolicy
	}
	return &SimpleCollector{policy: policy}, nil
}

// Policy returns the immutable policy used by c. A nil collector returns the
// zero policy.
func (c *SimpleCollector) Policy() SimplePolicy {
	if c == nil {
		return SimplePolicy{}
	}
	return c.policy
}

// Collect removes at most Policy().MaxBatch canonical tombstone identities
// while preserving Policy().MinRetained identities. It uses a target's
// optional eligible compactor to make structural leaf-to-root progress.
//
// Collect establishes no replication safety. In particular, a frontier, a
// later HLC tag, a checksum, or this method's successful return does not prove
// that another replica received a deletion. For replicated data, use
// Coordinator and authenticated exact acknowledgements instead.
func (c *SimpleCollector) Collect(target TombstoneTarget) (int, error) {
	if c == nil {
		return 0, ErrNilSimpleCollector
	}
	if target == nil {
		return 0, ErrNilTarget
	}
	tags := target.TombstoneTags()
	if err := validateTombstones(tags); err != nil {
		return 0, err
	}
	tags = canonicalSimpleTombstones(tags)
	if len(tags) <= c.policy.MinRetained {
		return 0, nil
	}
	count := len(tags) - c.policy.MinRetained
	if count > c.policy.MaxBatch {
		count = c.policy.MaxBatch
	}
	selected := tags[:count]
	if target, ok := target.(eligibleTombstoneTarget); ok {
		return target.CompactEligibleTombstones(selected)
	}
	return target.CompactTombstones(selected)
}

// canonicalSimpleTombstones preserves the common no-allocation path for the
// TombstoneTarget contract. A defensive copy and canonicalization keeps a
// malformed custom target from turning duplicates or non-canonical ordering
// into a misleading retention count or duplicate compaction request.
func canonicalSimpleTombstones(tags []crdt.Tag) []crdt.Tag {
	for index := 1; index < len(tags); index++ {
		if tags[index-1].Compare(tags[index]) >= 0 {
			tags = append([]crdt.Tag(nil), tags...)
			sort.Slice(tags, func(left, right int) bool {
				return tags[left].Compare(tags[right]) < 0
			})
			break
		}
	}
	if len(tags) < 2 {
		return tags
	}
	for index := 1; index < len(tags); index++ {
		if tags[index-1] == tags[index] {
			unique := tags[:1]
			for _, tag := range tags[1:] {
				if unique[len(unique)-1] != tag {
					unique = append(unique, tag)
				}
			}
			return unique
		}
	}
	return tags
}
