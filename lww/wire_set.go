package lww

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/clock"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/snapshot"
)

// MarshalBinary returns the canonical framed LWW-Set state using codec to
// identify and serialize its element type.
func (s *Set[T]) MarshalBinary(codec ElementCodec[T]) ([]byte, error) {
	if s == nil {
		return nil, ErrNilSet
	}
	s.mu.RLock()
	entries := cloneSetEntries(s.entries)
	s.mu.RUnlock()
	return marshalSet(crdt.TypeIDLWWSetState, codec, entries, frame.DefaultLimits())
}

// MarshalBinaryWithClockState captures state and HLC state for atomic
// persistence before a replica ID is reused after restart.
func (s *Set[T]) MarshalBinaryWithClockState(codec ElementCodec[T]) ([]byte, clock.State, error) {
	if s == nil || s.clock == nil {
		return nil, clock.State{}, ErrNilSet
	}
	s.mu.RLock()
	entries, state := cloneSetEntries(s.entries), s.clock.Snapshot()
	s.mu.RUnlock()
	encoded, err := marshalSet(crdt.TypeIDLWWSetState, codec, entries, frame.DefaultLimits())
	return encoded, state, err
}

// Snapshot creates an immutable LWW-Set state snapshot with caller-supplied
// replication frontier and the local HLC state.
func (s *Set[T]) Snapshot(codec ElementCodec[T], frontier map[string]crdt.Tag) (snapshot.Snapshot, error) {
	state, clockState, err := s.MarshalBinaryWithClockState(codec)
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	return snapshot.NewWithClockState(state, frontier, clockState)
}

// SnapshotCurrentState creates a snapshot whose frontier is derived from all
// visible and removed set entries.
func (s *Set[T]) SnapshotCurrentState(codec ElementCodec[T]) (snapshot.Snapshot, error) {
	if s == nil || s.clock == nil {
		return snapshot.Snapshot{}, ErrNilSet
	}
	s.mu.RLock()
	entries, clockState := cloneSetEntries(s.entries), s.clock.Snapshot()
	frontier := setFrontier(entries)
	s.mu.RUnlock()
	state, err := marshalSet(crdt.TypeIDLWWSetState, codec, entries, frame.DefaultLimits())
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	return snapshot.NewWithClockState(state, frontier, clockState)
}

// NewSetFromSnapshot restores a set and its persisted local HLC state.
// Snapshots without clock state are rejected because they cannot safely reuse
// a logical replica ID.
func NewSetFromSnapshot[T comparable](saved snapshot.Snapshot, codec ElementCodec[T]) (*Set[T], error) {
	return NewSetFromSnapshotWithOptions(saved, codec, DefaultSetOptions())
}

// NewSetFromSnapshotWithOptions restores a set and its persisted HLC state
// while retaining the receiving replication group's local entry limit.
func NewSetFromSnapshotWithOptions[T comparable](saved snapshot.Snapshot, codec ElementCodec[T], options SetOptions) (*Set[T], error) {
	if saved.TypeID != crdt.TypeIDLWWSetState {
		return nil, ErrInvalidSetSnap
	}
	if !options.valid() {
		return nil, ErrResourceLimit
	}
	clockState, ok := saved.ClockState()
	if !ok {
		return nil, ErrInvalidSetSnap
	}
	restored, err := NewSetFromClockWithOptions[T](clockState, options)
	if err != nil {
		return nil, err
	}
	if err := restored.UnmarshalBinary(saved.Bytes(), codec); err != nil {
		return nil, err
	}
	if tag, ok := greatestSetFrontierTag(saved.Frontier()); ok {
		if err := restored.clock.Witness(tag); err != nil {
			return nil, err
		}
	}
	return restored, nil
}

// MarshalBinary returns the canonical framed LWW-Set delta using codec.
func (d SetDelta[T]) MarshalBinary(codec ElementCodec[T]) ([]byte, error) {
	return marshalSet(crdt.TypeIDLWWSetDelta, codec, d.entries, frame.DefaultLimits())
}

// UnmarshalSetDelta decodes one bounded, canonical LWW-Set delta frame.
func UnmarshalSetDelta[T comparable](data []byte, codec ElementCodec[T]) (SetDelta[T], error) {
	return UnmarshalSetDeltaWithLimits(data, codec, frame.DefaultLimits())
}

// UnmarshalSetDeltaWithLimits decodes one bounded, canonical LWW-Set delta
// frame using caller-supplied decoder limits.
func UnmarshalSetDeltaWithLimits[T comparable](data []byte, codec ElementCodec[T], limits frame.DecoderLimits) (SetDelta[T], error) {
	entries, err := unmarshalSet(data, crdt.TypeIDLWWSetDelta, codec, limits)
	if err != nil {
		return SetDelta[T]{}, err
	}
	return SetDelta[T]{entries: entries}, nil
}

// UnmarshalBinary atomically replaces s with a valid complete LWW-Set state.
func (s *Set[T]) UnmarshalBinary(data []byte, codec ElementCodec[T]) error {
	return s.UnmarshalBinaryWithLimits(data, codec, frame.DefaultLimits())
}

// UnmarshalBinaryWithLimits validates data before replacing state. A malformed
// frame leaves both s and its HLC unchanged.
func (s *Set[T]) UnmarshalBinaryWithLimits(data []byte, codec ElementCodec[T], limits frame.DecoderLimits) error {
	if s == nil || s.clock == nil {
		return ErrNilSet
	}
	entries, err := unmarshalSet(data, crdt.TypeIDLWWSetState, codec, limits)
	if err != nil {
		return err
	}
	if len(entries) > s.options.MaxEntries {
		return ErrResourceLimit
	}
	tags, err := setTagIndex(entries)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if tag, ok := greatestSetTag(entries); ok {
		if err := s.clock.Witness(tag); err != nil {
			return err
		}
	}
	s.entries = entries
	s.tags = tags
	return nil
}

func marshalSet[T comparable](typeID uint64, codec ElementCodec[T], entries map[T]setEntry[T], limits frame.DecoderLimits) ([]byte, error) {
	bound, err := bindElementCodec(codec)
	if err != nil {
		return nil, err
	}
	return marshalSetWithCodec(typeID, bound, entries, limits)
}

func marshalSetWithCodec[T comparable](typeID uint64, codec boundElementCodec[T], entries map[T]setEntry[T], limits frame.DecoderLimits) ([]byte, error) {
	if typeID != crdt.TypeIDLWWSetState && typeID != crdt.TypeIDLWWSetDelta {
		return nil, frame.ErrInvalidFrame
	}
	if err := validateSetEntries(entries); err != nil {
		return nil, err
	}
	if len(entries) > limits.MaxElements || len(entries) > limits.MaxTags {
		return nil, frame.ErrFrameLimit
	}
	type item struct {
		encoded []byte
		entry   setEntry[T]
	}
	items := make([]item, 0, len(entries))
	for value, entry := range entries {
		encoded, err := codec.marshal(value)
		if err != nil {
			return nil, fmt.Errorf("%w: marshal element: %v", ErrInvalidCodec, err)
		}
		if len(encoded) > limits.MaxStringBytes || len(entry.tag.ReplicaID) > limits.MaxStringBytes {
			return nil, frame.ErrFrameLimit
		}
		items = append(items, item{encoded: encoded, entry: entry})
	}
	slices.SortFunc(items, func(left, right item) int { return bytes.Compare(left.encoded, right.encoded) })
	for index := 1; index < len(items); index++ {
		if bytes.Equal(items[index-1].encoded, items[index].encoded) {
			return nil, fmt.Errorf("%w: codec produced duplicate element bytes", ErrInvalidCodec)
		}
	}
	payloadSize := frame.UvarintSize(uint64(len(items)))
	for _, item := range items {
		additional := frame.UvarintSize(uint64(len(item.encoded))) + len(item.encoded) + frame.TagSize(item.entry.tag) + frame.UvarintSize(1)
		if additional > limits.MaxPayload-payloadSize {
			return nil, frame.ErrFrameLimit
		}
		payloadSize += additional
	}
	return frame.MarshalFrameWithPayload(typeID, codec.id, payloadSize, func(payload []byte) error {
		output := frame.AppendUvarint(payload[:0], uint64(len(items)))
		for _, item := range items {
			output = frame.AppendUvarint(output, uint64(len(item.encoded)))
			output = append(output, item.encoded...)
			output = frame.AppendTag(output, item.entry.tag)
			if item.entry.present {
				output = frame.AppendUvarint(output, 1)
			} else {
				output = frame.AppendUvarint(output, 0)
			}
		}
		if len(output) != payloadSize {
			return frame.ErrInvalidFrame
		}
		return nil
	})
}

func unmarshalSet[T comparable](data []byte, expectedTypeID uint64, codec ElementCodec[T], limits frame.DecoderLimits) (map[T]setEntry[T], error) {
	bound, err := bindElementCodec(codec)
	if err != nil {
		return nil, err
	}
	return unmarshalSetWithCodec(data, expectedTypeID, bound, limits)
}

func unmarshalSetWithCodec[T comparable](data []byte, expectedTypeID uint64, codec boundElementCodec[T], limits frame.DecoderLimits) (map[T]setEntry[T], error) {
	decoded, err := frame.UnmarshalFrame(data, limits)
	if err != nil {
		return nil, err
	}
	if decoded.TypeID != expectedTypeID || decoded.CodecID != codec.id {
		return nil, ErrCodecMismatch
	}
	position := 0
	count, next, ok := frame.ReadUvarint(decoded.Payload, position)
	if !ok || count > uint64(limits.MaxElements) || count > uint64(limits.MaxTags) {
		return nil, frame.ErrInvalidFrame
	}
	position = next
	entries := make(map[T]setEntry[T], int(count))
	var previous []byte
	for index := uint64(0); index < count; index++ {
		encoded, next, ok := frame.ReadBytes(decoded.Payload, position, limits.MaxStringBytes)
		if !ok || (index > 0 && bytes.Compare(previous, encoded) >= 0) {
			return nil, frame.ErrInvalidFrame
		}
		position = next
		value, err := codec.unmarshal(encoded)
		if err != nil {
			return nil, fmt.Errorf("%w: unmarshal element: %v", ErrInvalidCodec, err)
		}
		canonical, err := codec.marshal(value)
		if err != nil || !bytes.Equal(canonical, encoded) {
			return nil, ErrInvalidCodec
		}
		if _, exists := entries[value]; exists {
			return nil, frame.ErrInvalidFrame
		}
		tag, next, ok := frame.ReadTag(decoded.Payload, position, limits.MaxStringBytes)
		if !ok {
			return nil, frame.ErrInvalidFrame
		}
		position = next
		present, next, ok := frame.ReadUvarint(decoded.Payload, position)
		if !ok || present > 1 {
			return nil, frame.ErrInvalidFrame
		}
		position = next
		entries[value] = setEntry[T]{tag: tag, present: present == 1}
		previous = encoded
	}
	if position != len(decoded.Payload) {
		return nil, frame.ErrInvalidFrame
	}
	if err := validateSetEntries(entries); err != nil {
		return nil, frame.ErrInvalidFrame
	}
	return entries, nil
}

func greatestSetFrontierTag(frontier map[string]crdt.Tag) (crdt.Tag, bool) {
	var greatest crdt.Tag
	found := false
	for _, tag := range frontier {
		if !found || greatest.Compare(tag) < 0 {
			greatest, found = tag, true
		}
	}
	return greatest, found
}

func isNilSetCodec[T comparable](codec ElementCodec[T]) bool {
	if codec == nil {
		return true
	}
	value := reflect.ValueOf(codec)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
