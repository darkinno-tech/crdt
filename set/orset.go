// Package set implements set CRDT primitives.
package set

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

var (
	ErrInvalidCodec     = errors.New("set: invalid element codec")
	ErrInvalidReplicaID = errors.New("set: invalid replica ID")
	ErrNilORSet         = errors.New("set: nil OR-Set")
	ErrCodecMismatch    = errors.New("set: codec ID mismatch")
	ErrInvalidDelta     = errors.New("set: invalid OR-Set delta")
	ErrInvalidFrontier  = errors.New("set: invalid frontier")
	ErrInvalidSnapshot  = errors.New("set: invalid OR-Set snapshot")
)

// ElementCodec identifies and encodes one OR-Set element type. ID and encoded
// bytes must be stable across replicas that exchange state for the same set.
// Implementations must be safe for concurrent Marshal and Unmarshal calls and
// must return errors instead of panicking for invalid input.
type ElementCodec[T comparable] interface {
	ID() string
	Marshal(T) ([]byte, error)
	Unmarshal([]byte) (T, error)
}

// ORSet is an observed-remove, add-wins set. Each add receives a unique tag;
// remove operations tombstone only tags observed by that replica.
type ORSet[T comparable] struct {
	mu         sync.RWMutex
	lockID     uint64
	replicaID  string
	clock      *clock.HLC
	codec      boundElementCodec[T]
	elements   map[T]map[crdt.Tag]struct{}
	tombstones map[crdt.Tag]struct{}
}

// ORSetDelta is a joinable partial OR-Set state.
type ORSetDelta[T comparable] struct {
	adds       map[T]map[crdt.Tag]struct{}
	tombstones map[crdt.Tag]struct{}
}

// orSetMarshalPlan is an owned, map-free snapshot used to encode one OR-Set
// state without holding the set lock while calling an ElementCodec.
type orSetMarshalPlan[T comparable] struct {
	entries    []orSetMarshalPlanEntry[T]
	liveTags   []crdt.Tag
	tombstones []crdt.Tag
}

type orSetMarshalPlanEntry[T comparable] struct {
	element  T
	tagStart int
	tagEnd   int
}

var nextORSetLockID atomic.Uint64

var _ crdt.CRDT[*ORSet[string]] = (*ORSet[string])(nil)
var _ crdt.DeltaCapable[*ORSet[string], ORSetDelta[string]] = (*ORSet[string])(nil)

// NewORSet creates an OR-Set for replicaID with codec.
func NewORSet[T comparable](replicaID string, codec ElementCodec[T]) (*ORSet[T], error) {
	return NewORSetFromClock(clock.State{ReplicaID: replicaID}, codec)
}

// NewORSetFromClock creates an OR-Set using a restored HLC state. Use this
// constructor when reusing a replica ID after restart.
func NewORSetFromClock[T comparable](clockState clock.State, codec ElementCodec[T]) (*ORSet[T], error) {
	bound, err := bindElementCodec(codec)
	if err != nil {
		return nil, err
	}

	hlc, err := clock.NewHLCFromState(clockState)
	if err != nil {
		return nil, err
	}
	return &ORSet[T]{
		lockID:     nextORSetLockID.Add(1),
		replicaID:  clockState.ReplicaID,
		clock:      hlc,
		codec:      bound,
		elements:   make(map[T]map[crdt.Tag]struct{}),
		tombstones: make(map[crdt.Tag]struct{}),
	}, nil
}

// ClockState returns the state that must be persisted before this set's
// replica ID is reused after restart.
func (s *ORSet[T]) ClockState() clock.State {
	if s == nil || s.clock == nil {
		return clock.State{}
	}
	return s.clock.Snapshot()
}

// Frontier returns the greatest known tag for every replica in the current
// live-add and tombstone state. It is useful for diagnostics and for storing
// alongside a snapshot; the returned map is a copy. A frontier derived from
// independently delivered deltas is not, by itself, proof that every earlier
// tag was received, so it must not be used as a tombstone-GC acknowledgement
// unless the replication layer separately guarantees gap-free causal delivery.
func (s *ORSet[T]) Frontier() map[string]crdt.Tag {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return frontierForState(s.elements, s.tombstones)
}

// TombstoneTags returns a sorted copy of every tombstone currently retained by
// s. It is intended for an acknowledgement protocol that confirms individual
// tombstones; callers cannot mutate the returned slice to change s.
func (s *ORSet[T]) TombstoneTags() []crdt.Tag {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return tagsToSortedSlice(s.tombstones)
}

// NewORSetFromSnapshot restores an OR-Set and its persisted local HLC state.
// Snapshots without a clock state are rejected because they cannot prove that
// this logical replica will not reuse a mutation tag after restart.
func NewORSetFromSnapshot[T comparable](saved snapshot.Snapshot, codec ElementCodec[T]) (*ORSet[T], error) {
	if saved.TypeID != crdt.TypeIDORSetState {
		return nil, ErrInvalidSnapshot
	}
	clockState, ok := saved.ClockState()
	if !ok {
		return nil, ErrInvalidSnapshot
	}
	restored, err := NewORSetFromClock(clockState, codec)
	if err != nil {
		return nil, err
	}
	if err := restored.UnmarshalBinary(saved.Bytes()); err != nil {
		return nil, err
	}
	if tag, ok := greatestFrontierTag(saved.Frontier()); ok {
		if err := restored.clock.Witness(tag); err != nil {
			return nil, err
		}
	}
	return restored, nil
}

// Add inserts element and returns the add-tag delta.
func (s *ORSet[T]) Add(element T) (ORSetDelta[T], error) {
	if s == nil {
		return ORSetDelta[T]{}, ErrNilORSet
	}

	tag, err := s.clock.Now()
	if err != nil {
		return ORSetDelta[T]{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, removed := s.tombstones[tag]; removed {
		return ORSetDelta[T]{}, ErrInvalidDelta
	}
	if s.elements[element] == nil {
		s.elements[element] = make(map[crdt.Tag]struct{})
	}
	s.elements[element][tag] = struct{}{}
	return ORSetDelta[T]{adds: map[T]map[crdt.Tag]struct{}{element: {tag: {}}}}, nil
}

// Remove removes every tag for element that is currently observed by s and
// returns the tombstone delta. Concurrent, unknown adds survive the merge.
func (s *ORSet[T]) Remove(element T) (ORSetDelta[T], error) {
	if s == nil {
		return ORSetDelta[T]{}, ErrNilORSet
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	observed := s.elements[element]
	if len(observed) == 0 {
		return ORSetDelta[T]{adds: make(map[T]map[crdt.Tag]struct{}), tombstones: make(map[crdt.Tag]struct{})}, nil
	}

	tombstones := make(map[crdt.Tag]struct{}, len(observed))
	for tag := range observed {
		s.tombstones[tag] = struct{}{}
		tombstones[tag] = struct{}{}
	}
	delete(s.elements, element)
	return ORSetDelta[T]{adds: make(map[T]map[crdt.Tag]struct{}), tombstones: tombstones}, nil
}

// Compact removes tombstones that every active replica has acknowledged. The
// caller must provide a frontier whose every prefix is proven complete for
// every active replica. A greatest-observed-tag frontier from independently
// delivered, out-of-order deltas does not meet that requirement. Rejoining
// replicas must bootstrap from a post-compaction snapshot.
func (s *ORSet[T]) Compact(stableFrontier map[string]crdt.Tag) (int, error) {
	if s == nil {
		return 0, ErrNilORSet
	}
	if !validFrontier(stableFrontier) {
		return 0, ErrInvalidFrontier
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for tag := range s.tombstones {
		if acknowledged, ok := stableFrontier[tag.ReplicaID]; ok && tag.Compare(acknowledged) <= 0 {
			delete(s.tombstones, tag)
			removed++
		}
	}
	return removed, nil
}

// CompactTombstones removes exactly the supplied tombstones. It is safe for a
// coordinator that has independently proved acknowledgement for each tag; a
// tag that is not currently a tombstone is ignored. The input is completely
// validated before s is modified.
func (s *ORSet[T]) CompactTombstones(acknowledged []crdt.Tag) (int, error) {
	if s == nil {
		return 0, ErrNilORSet
	}
	for _, tag := range acknowledged {
		if !tag.Valid() {
			return 0, ErrInvalidFrontier
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for _, tag := range acknowledged {
		if _, exists := s.tombstones[tag]; exists {
			delete(s.tombstones, tag)
			removed++
		}
	}
	return removed, nil
}

// Contains reports whether an element has at least one live add-tag.
func (s *ORSet[T]) Contains(element T) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.elements[element]) > 0
}

// Elements returns a copy of the currently visible elements. Its order is not
// specified; deterministic wire order is provided by the encoding layer.
func (s *ORSet[T]) Elements() []T {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]T, 0, len(s.elements))
	for element := range s.elements {
		result = append(result, element)
	}
	return result
}

// Merge joins other into s without cloning the complete source state. It holds
// a receiver write lock and a source read lock in one per-instance order, so
// concurrent opposite-direction merges cannot deadlock.
func (s *ORSet[T]) Merge(other *ORSet[T]) error {
	if s == nil || other == nil {
		return ErrNilORSet
	}
	if s.codec.id != other.codec.id {
		return ErrCodecMismatch
	}
	if s == other {
		return nil
	}

	destinationFirst := lockORSetMerge(s, other)
	defer unlockORSetMerge(s, other, destinationFirst)
	if err := validateState(other.elements, other.tombstones); err != nil {
		return err
	}
	if subsumesState(s.elements, s.tombstones, other.elements, other.tombstones) {
		return nil
	}
	if tag, ok := greatestStateTag(other.elements, other.tombstones); ok {
		if err := s.clock.Witness(tag); err != nil {
			return err
		}
	}
	joinState(s.elements, s.tombstones, other.elements, other.tombstones)
	return nil
}

// ApplyDelta joins delta into s. A delta already represented by s returns
// without advancing the local HLC or taking the receiver write lock.
func (s *ORSet[T]) ApplyDelta(delta ORSetDelta[T]) error {
	if s == nil {
		return ErrNilORSet
	}
	if err := validateDeltaState(delta.adds, delta.tombstones); err != nil {
		return err
	}
	if s.subsumes(delta.adds, delta.tombstones) {
		return nil
	}
	if tag, ok := greatestStateTag(delta.adds, delta.tombstones); ok {
		if err := s.clock.Witness(tag); err != nil {
			return err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	joinState(s.elements, s.tombstones, delta.adds, delta.tombstones)
	return nil
}

// Merge joins other into d and returns a new delta without modifying either
// input delta.
func (d ORSetDelta[T]) Merge(other ORSetDelta[T]) (ORSetDelta[T], error) {
	if err := validateDeltaState(d.adds, d.tombstones); err != nil {
		return ORSetDelta[T]{}, err
	}
	if err := validateDeltaState(other.adds, other.tombstones); err != nil {
		return ORSetDelta[T]{}, err
	}
	adds := cloneAdds(d.adds)
	tombstones := cloneTags(d.tombstones)
	joinState(adds, tombstones, other.adds, other.tombstones)
	return ORSetDelta[T]{adds: adds, tombstones: tombstones}, nil
}

// State returns an immutable diagnostic summary.
func (s *ORSet[T]) State() crdt.StateSnapshot {
	if s == nil {
		return crdt.StateSnapshot{Type: "orset"}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return crdt.StateSnapshot{Type: "orset", ReplicaID: s.replicaID, ElementCount: len(s.elements), TombstoneCount: len(s.tombstones)}
}

// MarshalBinary returns a deterministic framed representation of s. Element
// bytes, tags, and tombstones are all sorted, so map iteration cannot affect
// the result.
func (s *ORSet[T]) MarshalBinary() ([]byte, error) {
	if s == nil {
		return nil, ErrNilORSet
	}
	s.mu.RLock()
	codec := s.codec
	plan, err := newORSetMarshalPlan(s.elements, s.tombstones)
	s.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	return marshalORSetPlan(crdt.TypeIDORSetState, codec, plan)
}

// MarshalBinaryWithClockState returns an OR-Set state frame and the local HLC
// state required to reuse this replica ID safely after restart. Persist both
// values atomically. The returned frame and clock state do not alias s.
func (s *ORSet[T]) MarshalBinaryWithClockState() ([]byte, clock.State, error) {
	if s == nil || s.clock == nil {
		return nil, clock.State{}, ErrNilORSet
	}
	s.mu.RLock()
	codec := s.codec
	plan, err := newORSetMarshalPlan(s.elements, s.tombstones)
	clockState := s.clock.Snapshot()
	s.mu.RUnlock()
	if err != nil {
		return nil, clock.State{}, err
	}
	encoded, err := marshalORSetPlan(crdt.TypeIDORSetState, codec, plan)
	if err != nil {
		return nil, clock.State{}, err
	}
	return encoded, clockState, nil
}

// Snapshot returns an immutable OR-Set snapshot containing both its state
// frame and local HLC state. Persist the returned object atomically with the
// supplied frontier before restoring this replica ID.
func (s *ORSet[T]) Snapshot(frontier map[string]crdt.Tag) (snapshot.Snapshot, error) {
	state, clockState, err := s.MarshalBinaryWithClockState()
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	return snapshot.NewWithClockState(state, frontier, clockState)
}

// SnapshotCurrentState returns a snapshot with the frontier derived from the
// same OR-Set state. Use Snapshot when a replication layer has a broader,
// externally acknowledged frontier to persist instead.
func (s *ORSet[T]) SnapshotCurrentState() (snapshot.Snapshot, error) {
	if s == nil || s.clock == nil {
		return snapshot.Snapshot{}, ErrNilORSet
	}
	s.mu.RLock()
	codec := s.codec
	plan, err := newORSetMarshalPlan(s.elements, s.tombstones)
	clockState := s.clock.Snapshot()
	s.mu.RUnlock()
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	frontier := frontierForORSetMarshalPlan(plan)
	state, err := marshalORSetPlan(crdt.TypeIDORSetState, codec, plan)
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	return snapshot.NewWithClockState(state, frontier, clockState)
}

// MarshalBinary returns a deterministic framed representation of d using
// codec to identify and serialize its element type.
func (d ORSetDelta[T]) MarshalBinary(codec ElementCodec[T]) ([]byte, error) {
	return marshalORSet(crdt.TypeIDORSetDelta, codec, d.adds, d.tombstones)
}

// UnmarshalORSetDelta validates and returns one OR-Set delta frame.
func UnmarshalORSetDelta[T comparable](data []byte, codec ElementCodec[T]) (ORSetDelta[T], error) {
	return UnmarshalORSetDeltaWithLimits(data, codec, frame.DefaultLimits())
}

// UnmarshalORSetDeltaWithLimits validates and returns one OR-Set delta frame
// using caller-supplied decoder limits.
func UnmarshalORSetDeltaWithLimits[T comparable](data []byte, codec ElementCodec[T], limits frame.DecoderLimits) (ORSetDelta[T], error) {
	adds, tombstones, err := unmarshalORSet(data, crdt.TypeIDORSetDelta, codec, limits)
	if err != nil {
		return ORSetDelta[T]{}, err
	}
	return ORSetDelta[T]{adds: adds, tombstones: tombstones}, nil
}

func marshalORSet[T comparable](typeID uint64, codec ElementCodec[T], adds map[T]map[crdt.Tag]struct{}, tombstones map[crdt.Tag]struct{}) ([]byte, error) {
	return marshalORSetWithLimits(typeID, codec, adds, tombstones, frame.DefaultLimits())
}

func marshalORSetWithLimits[T comparable](typeID uint64, codec ElementCodec[T], adds map[T]map[crdt.Tag]struct{}, tombstones map[crdt.Tag]struct{}, limits frame.DecoderLimits) ([]byte, error) {
	bound, err := bindElementCodec(codec)
	if err != nil {
		return nil, err
	}
	plan, err := newORSetMarshalPlan(adds, tombstones)
	if err != nil {
		return nil, err
	}
	return marshalORSetPlanWithLimits(typeID, bound, plan, limits)
}

func newORSetMarshalPlan[T comparable](adds map[T]map[crdt.Tag]struct{}, tombstones map[crdt.Tag]struct{}) (orSetMarshalPlan[T], error) {
	if err := validateState(adds, tombstones); err != nil {
		return orSetMarshalPlan[T]{}, err
	}
	liveTagCount := 0
	for _, tags := range adds {
		liveTagCount += len(tags)
	}
	plan := orSetMarshalPlan[T]{
		entries:    make([]orSetMarshalPlanEntry[T], 0, len(adds)),
		liveTags:   make([]crdt.Tag, 0, liveTagCount),
		tombstones: make([]crdt.Tag, 0, len(tombstones)),
	}
	for element, tags := range adds {
		tagStart := len(plan.liveTags)
		for tag := range tags {
			plan.liveTags = append(plan.liveTags, tag)
		}
		plan.entries = append(plan.entries, orSetMarshalPlanEntry[T]{
			element:  element,
			tagStart: tagStart,
			tagEnd:   len(plan.liveTags),
		})
	}
	for tag := range tombstones {
		plan.tombstones = append(plan.tombstones, tag)
	}
	return plan, nil
}

func marshalORSetPlan[T comparable](typeID uint64, codec boundElementCodec[T], plan orSetMarshalPlan[T]) ([]byte, error) {
	return marshalORSetPlanWithLimits(typeID, codec, plan, frame.DefaultLimits())
}

func marshalORSetPlanWithLimits[T comparable](typeID uint64, codec boundElementCodec[T], plan orSetMarshalPlan[T], limits frame.DecoderLimits) ([]byte, error) {
	codecID := codec.id
	if strings.TrimSpace(codecID) == "" || len(codecID) > limits.MaxCodecID {
		return nil, ErrInvalidCodec
	}
	type entry struct {
		encoded  []byte
		tagStart int
		tagEnd   int
	}
	entries := make([]entry, 0, len(plan.entries))
	for _, source := range plan.entries {
		encoded, err := codec.marshal(source.element)
		if err != nil {
			return nil, fmt.Errorf("%w: marshal element: %v", ErrInvalidCodec, err)
		}
		if len(encoded) > limits.MaxStringBytes {
			return nil, frame.ErrFrameLimit
		}
		if source.tagStart == source.tagEnd {
			return nil, ErrInvalidDelta
		}
		slices.SortFunc(plan.liveTags[source.tagStart:source.tagEnd], func(left, right crdt.Tag) int {
			return left.Compare(right)
		})
		entries = append(entries, entry{encoded: encoded, tagStart: source.tagStart, tagEnd: source.tagEnd})
	}
	slices.SortFunc(entries, func(left, right entry) int { return bytes.Compare(left.encoded, right.encoded) })
	for i := 1; i < len(entries); i++ {
		if bytes.Equal(entries[i-1].encoded, entries[i].encoded) {
			return nil, fmt.Errorf("%w: codec produced duplicate element bytes", ErrInvalidCodec)
		}
	}
	if len(entries) > limits.MaxElements {
		return nil, frame.ErrFrameLimit
	}

	payloadSize := frame.UvarintSize(uint64(len(entries)))
	tagCount := 0
	for _, item := range entries {
		tagCountForElement := item.tagEnd - item.tagStart
		additional := frame.UvarintSize(uint64(len(item.encoded))) + len(item.encoded) + frame.UvarintSize(uint64(tagCountForElement))
		if additional > limits.MaxPayload-payloadSize {
			return nil, frame.ErrFrameLimit
		}
		payloadSize += additional
		for _, tag := range plan.liveTags[item.tagStart:item.tagEnd] {
			if tagCount == limits.MaxTags {
				return nil, frame.ErrFrameLimit
			}
			if len(tag.ReplicaID) > limits.MaxStringBytes {
				return nil, frame.ErrFrameLimit
			}
			additional = frame.TagSize(tag)
			if additional > limits.MaxPayload-payloadSize {
				return nil, frame.ErrFrameLimit
			}
			payloadSize += additional
			tagCount++
		}
	}
	tags := plan.tombstones
	slices.SortFunc(tags, func(left, right crdt.Tag) int { return left.Compare(right) })
	additional := frame.UvarintSize(uint64(len(tags)))
	if additional > limits.MaxPayload-payloadSize {
		return nil, frame.ErrFrameLimit
	}
	payloadSize += additional
	for _, tag := range tags {
		if tagCount == limits.MaxTags {
			return nil, frame.ErrFrameLimit
		}
		if len(tag.ReplicaID) > limits.MaxStringBytes {
			return nil, frame.ErrFrameLimit
		}
		additional = frame.TagSize(tag)
		if additional > limits.MaxPayload-payloadSize {
			return nil, frame.ErrFrameLimit
		}
		payloadSize += additional
		tagCount++
	}
	return frame.MarshalFrameWithPayload(typeID, codecID, payloadSize, func(payload []byte) error {
		payload = frame.AppendUvarint(payload[:0], uint64(len(entries)))
		for _, item := range entries {
			payload = appendBytes(payload, item.encoded)
			payload = frame.AppendUvarint(payload, uint64(item.tagEnd-item.tagStart))
			for _, tag := range plan.liveTags[item.tagStart:item.tagEnd] {
				payload = frame.AppendTag(payload, tag)
			}
		}
		payload = frame.AppendUvarint(payload, uint64(len(tags)))
		for _, tag := range tags {
			payload = frame.AppendTag(payload, tag)
		}
		if len(payload) != payloadSize {
			return frame.ErrInvalidFrame
		}
		return nil
	})
}

// UnmarshalBinary validates data completely before atomically replacing s's
// state. It only accepts the canonical order required by MarshalBinary.
func (s *ORSet[T]) UnmarshalBinary(data []byte) error {
	return s.UnmarshalBinaryWithLimits(data, frame.DefaultLimits())
}

// UnmarshalBinaryWithLimits validates data completely before atomically
// replacing s's state using caller-supplied decoder limits.
func (s *ORSet[T]) UnmarshalBinaryWithLimits(data []byte, limits frame.DecoderLimits) error {
	if s == nil {
		return ErrNilORSet
	}
	adds, tombstones, err := unmarshalORSetWithCodec(data, crdt.TypeIDORSetState, s.codec, limits)
	if err != nil {
		return err
	}
	if tag, ok := greatestStateTag(adds, tombstones); ok {
		if err := s.clock.Witness(tag); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.elements = adds
	s.tombstones = tombstones
	return nil
}

func unmarshalORSet[T comparable](data []byte, expectedTypeID uint64, codec ElementCodec[T], limits frame.DecoderLimits) (map[T]map[crdt.Tag]struct{}, map[crdt.Tag]struct{}, error) {
	bound, err := bindElementCodec(codec)
	if err != nil {
		return nil, nil, err
	}
	return unmarshalORSetWithCodec(data, expectedTypeID, bound, limits)
}

func unmarshalORSetWithCodec[T comparable](data []byte, expectedTypeID uint64, codec boundElementCodec[T], limits frame.DecoderLimits) (map[T]map[crdt.Tag]struct{}, map[crdt.Tag]struct{}, error) {
	decoded, err := frame.UnmarshalFrame(data, limits)
	if err != nil {
		return nil, nil, err
	}
	if decoded.TypeID != expectedTypeID || decoded.CodecID != codec.id {
		return nil, nil, ErrCodecMismatch
	}

	pos := 0
	elementCount, next, ok := frame.ReadUvarint(decoded.Payload, pos)
	if !ok || elementCount > uint64(limits.MaxElements) {
		return nil, nil, frame.ErrInvalidFrame
	}
	pos = next
	adds := make(map[T]map[crdt.Tag]struct{}, int(elementCount))
	liveTags := make(map[crdt.Tag]struct{})
	var previousElement []byte
	tagCount := 0
	for i := uint64(0); i < elementCount; i++ {
		elementBytes, next, ok := frame.ReadBytes(decoded.Payload, pos, limits.MaxStringBytes)
		if !ok || (i > 0 && bytes.Compare(previousElement, elementBytes) >= 0) {
			return nil, nil, frame.ErrInvalidFrame
		}
		pos = next
		element, err := codec.unmarshal(elementBytes)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: unmarshal element: %v", ErrInvalidCodec, err)
		}
		canonical, err := codec.marshal(element)
		if err != nil || !bytes.Equal(canonical, elementBytes) {
			return nil, nil, ErrInvalidCodec
		}
		if _, exists := adds[element]; exists {
			return nil, nil, frame.ErrInvalidFrame
		}
		previousElement = elementBytes

		count, next, ok := frame.ReadUvarint(decoded.Payload, pos)
		if !ok || count == 0 || count > uint64(limits.MaxTags-tagCount) {
			return nil, nil, frame.ErrInvalidFrame
		}
		pos = next
		tags := make(map[crdt.Tag]struct{}, int(count))
		var previousTag crdt.Tag
		for j := uint64(0); j < count; j++ {
			tag, next, ok := frame.ReadTag(decoded.Payload, pos, limits.MaxStringBytes)
			if !ok || (j > 0 && previousTag.Compare(tag) >= 0) {
				return nil, nil, frame.ErrInvalidFrame
			}
			if _, exists := liveTags[tag]; exists {
				return nil, nil, frame.ErrInvalidFrame
			}
			pos = next
			tags[tag] = struct{}{}
			liveTags[tag] = struct{}{}
			previousTag = tag
			tagCount++
		}
		adds[element] = tags
	}

	tombstoneCount, next, ok := frame.ReadUvarint(decoded.Payload, pos)
	if !ok || tombstoneCount > uint64(limits.MaxTags-tagCount) {
		return nil, nil, frame.ErrInvalidFrame
	}
	pos = next
	tombstones := make(map[crdt.Tag]struct{}, int(tombstoneCount))
	var previousTombstone crdt.Tag
	for i := uint64(0); i < tombstoneCount; i++ {
		tag, next, ok := frame.ReadTag(decoded.Payload, pos, limits.MaxStringBytes)
		if !ok || (i > 0 && previousTombstone.Compare(tag) >= 0) {
			return nil, nil, frame.ErrInvalidFrame
		}
		if _, live := liveTags[tag]; live {
			return nil, nil, frame.ErrInvalidFrame
		}
		pos = next
		tombstones[tag] = struct{}{}
		previousTombstone = tag
		tagCount++
	}
	if pos != len(decoded.Payload) {
		return nil, nil, frame.ErrInvalidFrame
	}
	return adds, tombstones, nil
}

func joinState[T comparable](destinationAdds map[T]map[crdt.Tag]struct{}, destinationTombstones map[crdt.Tag]struct{}, sourceAdds map[T]map[crdt.Tag]struct{}, sourceTombstones map[crdt.Tag]struct{}) {
	for tag := range sourceTombstones {
		destinationTombstones[tag] = struct{}{}
	}
	for element, tags := range sourceAdds {
		for tag := range tags {
			if _, removed := destinationTombstones[tag]; removed {
				continue
			}
			if destinationAdds[element] == nil {
				destinationAdds[element] = make(map[crdt.Tag]struct{})
			}
			destinationAdds[element][tag] = struct{}{}
		}
	}
	if len(sourceTombstones) == 0 {
		return
	}
	for element, tags := range destinationAdds {
		for tag := range tags {
			if _, removed := destinationTombstones[tag]; removed {
				delete(tags, tag)
			}
		}
		if len(tags) == 0 {
			delete(destinationAdds, element)
		}
	}
}

// subsumes reports whether every mutation in source is already represented by
// s. A local tombstone also covers an incoming live add with the same tag.
func (s *ORSet[T]) subsumes(sourceAdds map[T]map[crdt.Tag]struct{}, sourceTombstones map[crdt.Tag]struct{}) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return subsumesState(s.elements, s.tombstones, sourceAdds, sourceTombstones)
}

// subsumesState reports whether each source mutation is represented by the
// destination. The caller must hold a lock that keeps both states stable.
func subsumesState[T comparable](destinationAdds map[T]map[crdt.Tag]struct{}, destinationTombstones map[crdt.Tag]struct{}, sourceAdds map[T]map[crdt.Tag]struct{}, sourceTombstones map[crdt.Tag]struct{}) bool {
	for tag := range sourceTombstones {
		if _, exists := destinationTombstones[tag]; !exists {
			return false
		}
	}
	for element, tags := range sourceAdds {
		destinationTags := destinationAdds[element]
		for tag := range tags {
			if _, removed := destinationTombstones[tag]; removed {
				continue
			}
			if _, exists := destinationTags[tag]; !exists {
				return false
			}
		}
	}
	return true
}

// lockORSetMerge locks destination for mutation and source for reading in a
// single total order. The lock ID is assigned by the constructor instead of
// using pointer addresses, which keeps ordering independent of runtime memory
// movement and avoids relying on unsafe operations.
func lockORSetMerge[T comparable](destination, source *ORSet[T]) bool {
	if destination.lockID < source.lockID {
		destination.mu.Lock()
		source.mu.RLock()
		return true
	}
	source.mu.RLock()
	destination.mu.Lock()
	return false
}

func unlockORSetMerge[T comparable](destination, source *ORSet[T], destinationFirst bool) {
	if destinationFirst {
		source.mu.RUnlock()
		destination.mu.Unlock()
		return
	}
	destination.mu.Unlock()
	source.mu.RUnlock()
}

func validateState[T comparable](adds map[T]map[crdt.Tag]struct{}, tombstones map[crdt.Tag]struct{}) error {
	for _, tags := range adds {
		for tag := range tags {
			if !tag.Valid() {
				return ErrInvalidDelta
			}
		}
	}
	for tag := range tombstones {
		if !tag.Valid() {
			return ErrInvalidDelta
		}
	}
	return nil
}

// validateDeltaState additionally rejects tag conflicts that cannot occur in a
// delta produced by this package or decoded from a canonical frame. Keeping the
// stronger check at the delta boundary protects direct package use without
// adding a tag-index allocation to complete-state Merge and Marshal paths.
func validateDeltaState[T comparable](adds map[T]map[crdt.Tag]struct{}, tombstones map[crdt.Tag]struct{}) error {
	if err := validateState(adds, tombstones); err != nil {
		return err
	}
	trackTags := len(adds) > 1 || (len(adds) > 0 && len(tombstones) > 0)
	if !trackTags {
		return nil
	}
	seen := make(map[crdt.Tag]struct{})
	for _, tags := range adds {
		for tag := range tags {
			if _, duplicate := seen[tag]; duplicate {
				return ErrInvalidDelta
			}
			seen[tag] = struct{}{}
		}
	}
	for tag := range tombstones {
		if _, duplicate := seen[tag]; duplicate {
			return ErrInvalidDelta
		}
		seen[tag] = struct{}{}
	}
	return nil
}

func validFrontier(frontier map[string]crdt.Tag) bool {
	for replicaID, tag := range frontier {
		if replicaID == "" || replicaID != tag.ReplicaID || !tag.Valid() {
			return false
		}
	}
	return true
}

func frontierForState[T comparable](adds map[T]map[crdt.Tag]struct{}, tombstones map[crdt.Tag]struct{}) map[string]crdt.Tag {
	frontier := make(map[string]crdt.Tag)
	for _, tags := range adds {
		for tag := range tags {
			if current, ok := frontier[tag.ReplicaID]; !ok || current.Compare(tag) < 0 {
				frontier[tag.ReplicaID] = tag
			}
		}
	}
	for tag := range tombstones {
		if current, ok := frontier[tag.ReplicaID]; !ok || current.Compare(tag) < 0 {
			frontier[tag.ReplicaID] = tag
		}
	}
	return frontier
}

func frontierForORSetMarshalPlan[T comparable](plan orSetMarshalPlan[T]) map[string]crdt.Tag {
	frontier := make(map[string]crdt.Tag)
	for _, tag := range plan.liveTags {
		if current, ok := frontier[tag.ReplicaID]; !ok || current.Compare(tag) < 0 {
			frontier[tag.ReplicaID] = tag
		}
	}
	for _, tag := range plan.tombstones {
		if current, ok := frontier[tag.ReplicaID]; !ok || current.Compare(tag) < 0 {
			frontier[tag.ReplicaID] = tag
		}
	}
	return frontier
}

func greatestStateTag[T comparable](adds map[T]map[crdt.Tag]struct{}, tombstones map[crdt.Tag]struct{}) (crdt.Tag, bool) {
	var greatest crdt.Tag
	found := false
	for _, tags := range adds {
		for tag := range tags {
			if !found || greatest.Compare(tag) < 0 {
				greatest, found = tag, true
			}
		}
	}
	for tag := range tombstones {
		if !found || greatest.Compare(tag) < 0 {
			greatest, found = tag, true
		}
	}
	return greatest, found
}

func greatestFrontierTag(frontier map[string]crdt.Tag) (crdt.Tag, bool) {
	var greatest crdt.Tag
	found := false
	for _, tag := range frontier {
		if !found || greatest.Compare(tag) < 0 {
			greatest, found = tag, true
		}
	}
	return greatest, found
}

func cloneAdds[T comparable](adds map[T]map[crdt.Tag]struct{}) map[T]map[crdt.Tag]struct{} {
	clone := make(map[T]map[crdt.Tag]struct{}, len(adds))
	for element, tags := range adds {
		clone[element] = cloneTags(tags)
	}
	return clone
}

func cloneTags(tags map[crdt.Tag]struct{}) map[crdt.Tag]struct{} {
	clone := make(map[crdt.Tag]struct{}, len(tags))
	for tag := range tags {
		clone[tag] = struct{}{}
	}
	return clone
}

func tagsToSortedSlice(tags map[crdt.Tag]struct{}) []crdt.Tag {
	result := make([]crdt.Tag, 0, len(tags))
	for tag := range tags {
		result = append(result, tag)
	}
	slices.SortFunc(result, func(left, right crdt.Tag) int { return left.Compare(right) })
	return result
}

func appendBytes(dst, value []byte) []byte {
	dst = frame.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

// Kept as package-private test seams while the canonical implementation lives
// in encoding alongside the frame primitives.
func appendTag(dst []byte, tag crdt.Tag) []byte { return frame.AppendTag(dst, tag) }

func readTag(data []byte, pos, maxStringBytes int) (crdt.Tag, int, bool) {
	return frame.ReadTag(data, pos, maxStringBytes)
}
