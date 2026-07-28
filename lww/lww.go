// Package lww implements last-write-wins CRDT collections.
//
// The HLC tag is the complete conflict-resolution rule: a higher tag wins.
// Therefore callers that reuse a replica ID must persist ClockState before a
// restart, just as they do for OR-Set.
package lww

import (
	"bytes"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/darkinno/crdt"
	"github.com/darkinno/crdt/clock"
)

var (
	ErrInvalidReplicaID = errors.New("lww: invalid replica ID")
	ErrNilSet           = errors.New("lww: nil set")
	ErrNilMap           = errors.New("lww: nil map")
	ErrInvalidKey       = errors.New("lww: invalid key")
	ErrTagConflict      = errors.New("lww: conflicting values for one tag")
)

type setEntry[T comparable] struct {
	tag     crdt.Tag
	present bool
}

// Set is an LWW element set. Concurrent add/remove operations are resolved by
// the canonical Tag ordering, rather than by an implicit wall-clock tie rule.
type Set[T comparable] struct {
	mu        sync.RWMutex
	replicaID string
	clock     *clock.HLC
	entries   map[T]setEntry[T]
}

var _ crdt.CRDT[*Set[string]] = (*Set[string])(nil)

func NewSet[T comparable](replicaID string) (*Set[T], error) {
	return NewSetFromClock[T](clock.State{ReplicaID: replicaID})
}

func NewSetFromClock[T comparable](state clock.State) (*Set[T], error) {
	if !(crdt.Tag{ReplicaID: state.ReplicaID}).Valid() {
		return nil, ErrInvalidReplicaID
	}
	hlc, err := clock.NewHLCFromState(state)
	if err != nil {
		return nil, err
	}
	return &Set[T]{replicaID: state.ReplicaID, clock: hlc, entries: make(map[T]setEntry[T])}, nil
}

func (s *Set[T]) ClockState() clock.State {
	if s == nil || s.clock == nil {
		return clock.State{}
	}
	return s.clock.Snapshot()
}

func (s *Set[T]) Add(value T) error    { return s.write(value, true) }
func (s *Set[T]) Remove(value T) error { return s.write(value, false) }

func (s *Set[T]) write(value T, present bool) error {
	if s == nil || s.clock == nil {
		return ErrNilSet
	}
	tag, err := s.clock.Now()
	if err != nil {
		return err
	}
	s.mu.Lock()
	if current, exists := s.entries[value]; !exists || current.tag.Compare(tag) < 0 {
		s.entries[value] = setEntry[T]{tag: tag, present: present}
	}
	s.mu.Unlock()
	return nil
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
	s.mu.Lock()
	defer s.mu.Unlock()
	for value, incoming := range entries {
		if current, exists := s.entries[value]; exists && current.tag == incoming.tag && current.present != incoming.present {
			return ErrTagConflict
		}
	}
	if tag, ok := greatestSetTag(entries); ok {
		if err := s.clock.Witness(tag); err != nil {
			return err
		}
	}
	for value, incoming := range entries {
		current, exists := s.entries[value]
		switch {
		case !exists || current.tag.Compare(incoming.tag) < 0:
			s.entries[value] = incoming
		}
	}
	return nil
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

type mapEntry struct {
	tag     crdt.Tag
	present bool
	value   []byte
}

// Map is a byte-value LWW map. Returning and accepting copies prevents a
// caller from modifying replicated state through a shared slice. Values are
// deliberately opaque; applications may use a deterministic JSON, protobuf,
// or domain codec above this type.
type Map struct {
	mu        sync.RWMutex
	replicaID string
	clock     *clock.HLC
	entries   map[string]mapEntry
}

var _ crdt.CRDT[*Map] = (*Map)(nil)

func NewMap(replicaID string) (*Map, error) {
	return NewMapFromClock(clock.State{ReplicaID: replicaID})
}

func NewMapFromClock(state clock.State) (*Map, error) {
	if !(crdt.Tag{ReplicaID: state.ReplicaID}).Valid() {
		return nil, ErrInvalidReplicaID
	}
	hlc, err := clock.NewHLCFromState(state)
	if err != nil {
		return nil, err
	}
	return &Map{replicaID: state.ReplicaID, clock: hlc, entries: make(map[string]mapEntry)}, nil
}

func (m *Map) ClockState() clock.State {
	if m == nil || m.clock == nil {
		return clock.State{}
	}
	return m.clock.Snapshot()
}

func (m *Map) Set(key string, value []byte) error { return m.write(key, value, true) }
func (m *Map) Delete(key string) error            { return m.write(key, nil, false) }

func (m *Map) write(key string, value []byte, present bool) error {
	if m == nil || m.clock == nil {
		return ErrNilMap
	}
	if strings.TrimSpace(key) == "" {
		return ErrInvalidKey
	}
	tag, err := m.clock.Now()
	if err != nil {
		return err
	}
	m.mu.Lock()
	if current, exists := m.entries[key]; !exists || current.tag.Compare(tag) < 0 {
		m.entries[key] = mapEntry{tag: tag, present: present, value: append([]byte(nil), value...)}
	}
	m.mu.Unlock()
	return nil
}

func (m *Map) Get(key string) ([]byte, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	entry, ok := m.entries[key]
	m.mu.RUnlock()
	if !ok || !entry.present {
		return nil, false
	}
	return append([]byte(nil), entry.value...), true
}

// Keys returns the visible keys in lexical order, which keeps callers from
// accidentally depending on Go's randomized map iteration order.
func (m *Map) Keys() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	keys := make([]string, 0, len(m.entries))
	for key, entry := range m.entries {
		if entry.present {
			keys = append(keys, key)
		}
	}
	m.mu.RUnlock()
	sort.Strings(keys)
	return keys
}

func (m *Map) Merge(other *Map) error {
	if m == nil || other == nil {
		return ErrNilMap
	}
	if m == other {
		return nil
	}
	other.mu.RLock()
	entries := cloneMapEntries(other.entries)
	other.mu.RUnlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, incoming := range entries {
		if current, exists := m.entries[key]; exists && current.tag == incoming.tag && (current.present != incoming.present || !bytes.Equal(current.value, incoming.value)) {
			return ErrTagConflict
		}
	}
	if tag, ok := greatestMapTag(entries); ok {
		if err := m.clock.Witness(tag); err != nil {
			return err
		}
	}
	for key, incoming := range entries {
		current, exists := m.entries[key]
		switch {
		case !exists || current.tag.Compare(incoming.tag) < 0:
			m.entries[key] = incoming
		}
	}
	return nil
}

func (m *Map) State() crdt.StateSnapshot {
	if m == nil {
		return crdt.StateSnapshot{Type: "lww-map"}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	present := 0
	for _, entry := range m.entries {
		if entry.present {
			present++
		}
	}
	return crdt.StateSnapshot{Type: "lww-map", ReplicaID: m.replicaID, ElementCount: present, TombstoneCount: len(m.entries) - present}
}

func cloneSetEntries[T comparable](source map[T]setEntry[T]) map[T]setEntry[T] {
	out := make(map[T]setEntry[T], len(source))
	for value, entry := range source {
		out[value] = entry
	}
	return out
}
func cloneMapEntries(source map[string]mapEntry) map[string]mapEntry {
	out := make(map[string]mapEntry, len(source))
	for key, entry := range source {
		// Map values are copied at Set and never mutated internally. Sharing the
		// immutable backing slice here avoids a second full value copy per Merge.
		out[key] = entry
	}
	return out
}
func greatestSetTag[T comparable](entries map[T]setEntry[T]) (crdt.Tag, bool) {
	var greatest crdt.Tag
	ok := false
	for _, entry := range entries {
		if !ok || greatest.Compare(entry.tag) < 0 {
			greatest, ok = entry.tag, true
		}
	}
	return greatest, ok
}
func greatestMapTag(entries map[string]mapEntry) (crdt.Tag, bool) {
	var greatest crdt.Tag
	ok := false
	for _, entry := range entries {
		if !ok || greatest.Compare(entry.tag) < 0 {
			greatest, ok = entry.tag, true
		}
	}
	return greatest, ok
}
