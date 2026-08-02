package lww

import (
	"sync"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
)

type setEntry[T comparable] struct {
	tag     crdt.Tag
	present bool
}

// ElementCodec identifies and encodes one LWW-Set element type. Its ID and
// encoded bytes must be stable across replicas that exchange frames. Codec
// implementations must be safe for concurrent calls and return errors rather
// than panicking for invalid input.
type ElementCodec[T comparable] interface {
	ID() string
	Marshal(T) ([]byte, error)
	Unmarshal([]byte) (T, error)
}

// SetDelta is a joinable partial LWW-Set state. Its entries are unexported so
// callers cannot alter a delta after handing it to replication code.
type SetDelta[T comparable] struct{ entries map[T]setEntry[T] }

// SetOptions bounds retained LWW-Set entries, including delete tombstones.
// Applications accepting untrusted or long-lived replication streams should
// select a limit for each replication group instead of relying on process-wide
// memory availability.
type SetOptions struct {
	MaxEntries int
}

// DefaultSetOptions returns a conservative default aligned with the default
// framed element limit.
func DefaultSetOptions() SetOptions { return SetOptions{MaxEntries: 1 << 20} }

func (o SetOptions) valid() bool { return o.MaxEntries > 0 }

// Set is an LWW element set. Concurrent add/remove operations are resolved by
// the canonical Tag ordering, rather than by an implicit wall-clock tie rule.
type Set[T comparable] struct {
	mu        sync.RWMutex
	replicaID string
	clock     *clock.HLC
	options   SetOptions
	entries   map[T]setEntry[T]
	tags      map[crdt.Tag]T
}

var _ crdt.CRDT[*Set[string]] = (*Set[string])(nil)
var _ crdt.DeltaCapable[*Set[string], SetDelta[string]] = (*Set[string])(nil)

func NewSet[T comparable](replicaID string) (*Set[T], error) {
	return NewSetWithOptions[T](replicaID, DefaultSetOptions())
}

// NewSetWithOptions constructs a set with explicit retained-entry limits.
func NewSetWithOptions[T comparable](replicaID string, options SetOptions) (*Set[T], error) {
	return NewSetFromClockWithOptions[T](clock.State{ReplicaID: replicaID}, options)
}

func NewSetFromClock[T comparable](state clock.State) (*Set[T], error) {
	return NewSetFromClockWithOptions[T](state, DefaultSetOptions())
}

// NewSetFromClockWithOptions restores a replica clock with explicit retained
// entry limits. Persist the clock atomically with a complete snapshot before
// reusing its replica ID.
func NewSetFromClockWithOptions[T comparable](state clock.State, options SetOptions) (*Set[T], error) {
	if !(crdt.Tag{ReplicaID: state.ReplicaID}).Valid() {
		return nil, ErrInvalidReplicaID
	}
	if !options.valid() {
		return nil, ErrResourceLimit
	}
	hlc, err := clock.NewHLCFromState(state)
	if err != nil {
		return nil, err
	}
	return &Set[T]{
		replicaID: state.ReplicaID,
		clock:     hlc,
		options:   options,
		entries:   make(map[T]setEntry[T]),
		tags:      make(map[crdt.Tag]T),
	}, nil
}

func (s *Set[T]) ClockState() clock.State {
	if s == nil || s.clock == nil {
		return clock.State{}
	}
	return s.clock.Snapshot()
}

// Add inserts value and preserves the original non-delta API.
func (s *Set[T]) Add(value T) error {
	_, err := s.AddWithDelta(value)
	return err
}

// Remove removes value and preserves the original non-delta API.
func (s *Set[T]) Remove(value T) error {
	_, err := s.RemoveWithDelta(value)
	return err
}

// AddWithDelta inserts value and returns the joinable delta for this write.
func (s *Set[T]) AddWithDelta(value T) (SetDelta[T], error) {
	return s.writeDelta(value, true)
}

// RemoveWithDelta removes value and returns the joinable delta for this write.
func (s *Set[T]) RemoveWithDelta(value T) (SetDelta[T], error) {
	return s.writeDelta(value, false)
}

func (s *Set[T]) writeDelta(value T, present bool) (SetDelta[T], error) {
	if s == nil || s.clock == nil {
		return SetDelta[T]{}, ErrNilSet
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[value]; !exists && len(s.entries) >= s.options.MaxEntries {
		return SetDelta[T]{}, ErrResourceLimit
	}
	tag, err := s.clock.Now()
	if err != nil {
		return SetDelta[T]{}, err
	}
	if owner, exists := s.tags[tag]; exists && owner != value {
		return SetDelta[T]{}, ErrTagConflict
	}
	incoming := setEntry[T]{tag: tag, present: present}
	if current, exists := s.entries[value]; !exists || current.tag.Compare(tag) < 0 {
		delete(s.tags, current.tag)
		s.entries[value] = incoming
		s.tags[tag] = value
	}
	return SetDelta[T]{entries: map[T]setEntry[T]{value: incoming}}, nil
}

func (s *Set[T]) Contains(value T) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.entries[value].present
}

func (s *Set[T]) Elements() []T {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make([]T, 0, len(s.entries))
	for value, entry := range s.entries {
		if entry.present {
			values = append(values, value)
		}
	}
	return values
}

// Merge selects the highest tag for every element. It snapshots other before
// locking s, so reciprocal concurrent merges cannot deadlock.
func (s *Set[T]) Merge(other *Set[T]) error {
	if s == nil || other == nil {
		return ErrNilSet
	}
	if s == other {
		return nil
	}
	other.mu.RLock()
	entries := cloneSetEntries(other.entries)
	other.mu.RUnlock()
	return s.applyOwnedSetEntries(entries)
}

// ApplyDelta joins a validated partial LWW-Set state into s.
func (s *Set[T]) ApplyDelta(delta SetDelta[T]) error {
	if s == nil || s.clock == nil {
		return ErrNilSet
	}
	return s.applyOwnedSetEntries(cloneSetEntries(delta.entries))
}

// applyOwnedSetEntries joins entries that already belong to the caller.
func (s *Set[T]) applyOwnedSetEntries(entries map[T]setEntry[T]) error {
	if err := validateSetEntries(entries); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureSetTagsCompatible(s.tags, entries); err != nil {
		return err
	}
	for value, incoming := range entries {
		if current, exists := s.entries[value]; exists && current.tag == incoming.tag && current.present != incoming.present {
			return ErrTagConflict
		}
	}
	if setEntriesSubsumed(s.entries, entries) {
		return nil
	}
	if len(s.entries)+newSetEntries(entries, s.entries) > s.options.MaxEntries {
		return ErrResourceLimit
	}
	if tag, ok := greatestSetTag(entries); ok {
		if err := s.clock.Witness(tag); err != nil {
			return err
		}
	}
	for value, incoming := range entries {
		current, exists := s.entries[value]
		if !exists || current.tag.Compare(incoming.tag) < 0 {
			if exists {
				delete(s.tags, current.tag)
			}
			s.entries[value] = incoming
			s.tags[incoming.tag] = value
		}
	}
	return nil
}

// Merge joins two partial LWW-Set states without modifying either delta.
func (d SetDelta[T]) Merge(other SetDelta[T]) (SetDelta[T], error) {
	if err := validateSetEntries(d.entries); err != nil {
		return SetDelta[T]{}, err
	}
	if err := validateSetEntries(other.entries); err != nil {
		return SetDelta[T]{}, err
	}
	merged := cloneSetEntries(d.entries)
	for value, incoming := range other.entries {
		if current, exists := merged[value]; exists && setEntriesConflict(current, incoming) {
			return SetDelta[T]{}, ErrTagConflict
		}
		if current, exists := merged[value]; !exists || current.tag.Compare(incoming.tag) < 0 {
			merged[value] = incoming
		}
	}
	if err := validateSetEntries(merged); err != nil {
		return SetDelta[T]{}, err
	}
	return SetDelta[T]{entries: merged}, nil
}

func (s *Set[T]) State() crdt.StateSnapshot {
	if s == nil {
		return crdt.StateSnapshot{Type: "lww-set"}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	present := 0
	for _, entry := range s.entries {
		if entry.present {
			present++
		}
	}
	return crdt.StateSnapshot{Type: "lww-set", ReplicaID: s.replicaID, ElementCount: present, TombstoneCount: len(s.entries) - present}
}

// Frontier returns the greatest set-entry tag per replica. The returned map
// is owned by the caller and includes removed entries.
func (s *Set[T]) Frontier() map[string]crdt.Tag {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return setFrontier(s.entries)
}

// TombstoneTags returns retained delete tags in canonical order. The list is
// an input to an external exact-acknowledgement epoch; it is not proof that a
// tombstone may be removed by itself.
func (s *Set[T]) TombstoneTags() []crdt.Tag {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	tags := make([]crdt.Tag, 0)
	for _, entry := range s.entries {
		if !entry.present {
			tags = append(tags, entry.tag)
		}
	}
	s.mu.RUnlock()
	sortTags(tags)
	return tags
}

// CompactTombstones removes exactly requested deleted entries. For replicated
// state, call it only after every active member has acknowledged the exact
// tags in one authenticated membership epoch, a post-compaction snapshot is
// durable, and old deltas have been retired. tombstonegc.SimpleCollector may
// call it only for its documented local-only lifecycle. Unknown tags are
// ignored; attempting to remove a live entry or passing an invalid tag leaves
// the set unchanged.
func (s *Set[T]) CompactTombstones(tags []crdt.Tag) (int, error) {
	if s == nil {
		return 0, ErrNilSet
	}
	for _, tag := range tags {
		if !tag.Valid() {
			return 0, ErrUnsafeCompaction
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byTag := make(map[crdt.Tag]T, len(s.entries))
	for value, entry := range s.entries {
		if owner, exists := byTag[entry.tag]; exists && owner != value {
			return 0, ErrUnsafeCompaction
		}
		byTag[entry.tag] = value
	}
	compact := make([]T, 0, len(tags))
	seen := make(map[crdt.Tag]struct{}, len(tags))
	for _, tag := range tags {
		if _, duplicate := seen[tag]; duplicate {
			continue
		}
		seen[tag] = struct{}{}
		value, exists := byTag[tag]
		if !exists {
			continue
		}
		if s.entries[value].present {
			return 0, ErrUnsafeCompaction
		}
		compact = append(compact, value)
	}
	for _, value := range compact {
		entry := s.entries[value]
		delete(s.entries, value)
		delete(s.tags, entry.tag)
	}
	return len(compact), nil
}
