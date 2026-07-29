package lww

import (
	"sort"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

// MarshalBinary returns the canonical framed LWW-Map state.
func (m *Map) MarshalBinary() ([]byte, error) {
	if m == nil {
		return nil, ErrNilMap
	}
	m.mu.RLock()
	entries := cloneMapEntries(m.entries)
	m.mu.RUnlock()
	return marshalMap(crdt.TypeIDLWWMapState, entries)
}

// MarshalBinary returns the canonical framed LWW-Map delta.
func (d MapDelta) MarshalBinary() ([]byte, error) {
	return marshalMap(crdt.TypeIDLWWMapDelta, d.entries)
}

func marshalMap(typeID uint64, entries map[string]mapEntry) ([]byte, error) {
	return marshalMapWithLimits(typeID, entries, frame.DefaultLimits())
}

func marshalMapWithLimits(typeID uint64, entries map[string]mapEntry, limits frame.Limits) ([]byte, error) {
	if typeID != crdt.TypeIDLWWMapState && typeID != crdt.TypeIDLWWMapDelta {
		return nil, frame.ErrInvalidFrame
	}
	if err := validateMapEntries(entries); err != nil {
		return nil, err
	}
	if len(entries) > limits.MaxElements || len(entries) > limits.MaxTags {
		return nil, frame.ErrFrameLimit
	}
	keys := sortedMapKeys(entries)
	payloadSize := frame.UvarintSize(uint64(len(keys)))
	for _, key := range keys {
		entry := entries[key]
		if err := addMapBytesSize(&payloadSize, len(key), limits); err != nil {
			return nil, err
		}
		if len(entry.tag.ReplicaID) > limits.MaxStringBytes {
			return nil, frame.ErrFrameLimit
		}
		if err := addMapRawSize(&payloadSize, frame.TagSize(entry.tag)+frame.UvarintSize(0), limits); err != nil {
			return nil, err
		}
		if entry.present {
			if err := addMapBytesSize(&payloadSize, len(entry.value), limits); err != nil {
				return nil, err
			}
		}
	}
	return frame.MarshalFrameWithPayload(typeID, "", payloadSize, func(payload []byte) error {
		output := frame.AppendUvarint(payload[:0], uint64(len(keys)))
		for _, key := range keys {
			entry := entries[key]
			output = frame.AppendUvarint(output, uint64(len(key)))
			output = append(output, key...)
			output = frame.AppendTag(output, entry.tag)
			if entry.present {
				output = frame.AppendUvarint(output, 1)
				output = frame.AppendUvarint(output, uint64(len(entry.value)))
				output = append(output, entry.value...)
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

func addMapBytesSize(payloadSize *int, valueLength int, limits frame.Limits) error {
	if valueLength > limits.MaxStringBytes {
		return frame.ErrFrameLimit
	}
	return addMapRawSize(payloadSize, frame.UvarintSize(uint64(valueLength))+valueLength, limits)
}

func addMapRawSize(payloadSize *int, additional int, limits frame.Limits) error {
	if additional < 0 || additional > limits.MaxPayload-*payloadSize {
		return frame.ErrFrameLimit
	}
	*payloadSize += additional
	return nil
}

// UnmarshalMapDelta decodes one bounded, canonical LWW-Map delta frame.
func UnmarshalMapDelta(data []byte) (MapDelta, error) {
	return UnmarshalMapDeltaWithLimits(data, frame.DefaultLimits())
}

// UnmarshalMapDeltaWithLimits decodes one bounded, canonical LWW-Map delta.
func UnmarshalMapDeltaWithLimits(data []byte, limits frame.Limits) (MapDelta, error) {
	entries, err := unmarshalMap(data, crdt.TypeIDLWWMapDelta, limits)
	if err != nil {
		return MapDelta{}, err
	}
	return MapDelta{entries: entries}, nil
}

// UnmarshalMapDeltaWithOptions decodes one bounded, canonical LWW-Map delta
// while enforcing the receiver's retained-state limits before allocating or
// copying entries beyond them. It is intended for bounded wrappers that must
// validate a delta before constructing a Map receiver.
func UnmarshalMapDeltaWithOptions(data []byte, limits frame.Limits, options MapOptions) (MapDelta, error) {
	if !options.valid() {
		return MapDelta{}, ErrResourceLimit
	}
	entries, err := unmarshalMapWithOptions(data, crdt.TypeIDLWWMapDelta, limits, &options)
	if err != nil {
		return MapDelta{}, err
	}
	return MapDelta{entries: entries}, nil
}

// UnmarshalBinary atomically replaces m with a valid complete LWW-Map state.
func (m *Map) UnmarshalBinary(data []byte) error {
	return m.UnmarshalBinaryWithLimits(data, frame.DefaultLimits())
}

// UnmarshalBinaryWithLimits atomically replaces m with a bounded LWW-Map
// state. A malformed frame leaves both m and its HLC unchanged.
func (m *Map) UnmarshalBinaryWithLimits(data []byte, limits frame.Limits) error {
	if m == nil || m.clock == nil {
		return ErrNilMap
	}
	entries, err := unmarshalMapWithOptions(data, crdt.TypeIDLWWMapState, limits, &m.options)
	if err != nil {
		return err
	}
	if len(entries) > m.options.MaxEntries {
		return ErrResourceLimit
	}
	if err := validateMapEntriesWithOptions(entries, m.options); err != nil {
		return err
	}
	tags, err := mapTagIndex(entries)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if tag, ok := greatestMapTag(entries); ok {
		if err := m.clock.Witness(tag); err != nil {
			return err
		}
	}
	m.entries = entries
	m.tags = tags
	return nil
}

// MarshalBinaryWithClockState captures state and HLC state for atomic
// persistence before a replica ID is reused after restart.
func (m *Map) MarshalBinaryWithClockState() ([]byte, clock.State, error) {
	if m == nil || m.clock == nil {
		return nil, clock.State{}, ErrNilMap
	}
	m.mu.RLock()
	entries, state := cloneMapEntries(m.entries), m.clock.Snapshot()
	m.mu.RUnlock()
	encoded, err := marshalMap(crdt.TypeIDLWWMapState, entries)
	return encoded, state, err
}

// Snapshot creates an immutable map state snapshot with caller-supplied
// replication frontier and the local HLC state.
func (m *Map) Snapshot(frontier map[string]crdt.Tag) (snapshot.Snapshot, error) {
	state, clockState, err := m.MarshalBinaryWithClockState()
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	return snapshot.NewWithClockState(state, frontier, clockState)
}

// SnapshotCurrentState creates a snapshot whose frontier is derived from all
// visible and deleted map entries.
func (m *Map) SnapshotCurrentState() (snapshot.Snapshot, error) {
	if m == nil || m.clock == nil {
		return snapshot.Snapshot{}, ErrNilMap
	}
	m.mu.RLock()
	entries, clockState := cloneMapEntries(m.entries), m.clock.Snapshot()
	frontier := mapFrontier(entries)
	m.mu.RUnlock()
	state, err := marshalMap(crdt.TypeIDLWWMapState, entries)
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	return snapshot.NewWithClockState(state, frontier, clockState)
}

// NewMapFromSnapshot restores a map and its HLC state. Snapshots without a
// clock state are rejected because they cannot safely reuse a replica ID.
func NewMapFromSnapshot(saved snapshot.Snapshot) (*Map, error) {
	return NewMapFromSnapshotWithOptions(saved, DefaultMapOptions())
}

// NewMapFromSnapshotWithOptions restores a map and its HLC state while
// retaining the receiving replication group's local resource limits.
func NewMapFromSnapshotWithOptions(saved snapshot.Snapshot, options MapOptions) (*Map, error) {
	if saved.TypeID != crdt.TypeIDLWWMapState {
		return nil, ErrInvalidSnapshot
	}
	if !options.valid() {
		return nil, ErrResourceLimit
	}
	clockState, ok := saved.ClockState()
	if !ok {
		return nil, ErrInvalidSnapshot
	}
	m, err := NewMapFromClockWithOptions(clockState, options)
	if err != nil {
		return nil, err
	}
	if err := m.UnmarshalBinary(saved.Bytes()); err != nil {
		return nil, err
	}
	if tag, ok := greatestMapFrontierTag(saved.Frontier()); ok {
		if err := m.clock.Witness(tag); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func unmarshalMap(data []byte, expectedType uint64, limits frame.Limits) (map[string]mapEntry, error) {
	return unmarshalMapWithOptions(data, expectedType, limits, nil)
}

const minMapEntryBytes = 7

func unmarshalMapWithOptions(data []byte, expectedType uint64, limits frame.Limits, options *MapOptions) (map[string]mapEntry, error) {
	if options != nil && !options.valid() {
		return nil, ErrResourceLimit
	}
	decoded, err := frame.UnmarshalFrame(data, limits)
	if err != nil {
		return nil, err
	}
	if decoded.TypeID != expectedType || decoded.CodecID != "" {
		return nil, frame.ErrInvalidFrame
	}
	position := 0
	count, next, ok := frame.ReadUvarint(decoded.Payload, position)
	if !ok || count > uint64(limits.MaxElements) || count > uint64(limits.MaxTags) {
		return nil, frame.ErrInvalidFrame
	}
	if options != nil && count > uint64(options.MaxEntries) {
		return nil, ErrResourceLimit
	}
	position = next
	if count > uint64((len(decoded.Payload)-position)/minMapEntryBytes) {
		return nil, frame.ErrInvalidFrame
	}
	entries := make(map[string]mapEntry, int(count))
	previous := ""
	keyLimit := 0
	valueLimit := 0
	if options != nil {
		keyLimit = options.MaxKeyBytes
		valueLimit = options.MaxValueBytes
	}
	for index := uint64(0); index < count; index++ {
		keyBytes, next, err := readMapBytes(decoded.Payload, position, limits.MaxStringBytes, keyLimit)
		if err != nil {
			return nil, err
		}
		key := string(keyBytes)
		if key == "" || (index > 0 && previous >= key) {
			return nil, frame.ErrInvalidFrame
		}
		position = next
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
		entry := mapEntry{tag: tag, present: present == 1}
		if entry.present {
			value, next, err := readMapBytes(decoded.Payload, position, limits.MaxStringBytes, valueLimit)
			if err != nil {
				return nil, err
			}
			entry.value = append([]byte(nil), value...)
			position = next
		}
		if err := validateMapEntries(map[string]mapEntry{key: entry}); err != nil {
			return nil, frame.ErrInvalidFrame
		}
		entries[key] = entry
		previous = key
	}
	if position != len(decoded.Payload) {
		return nil, frame.ErrInvalidFrame
	}
	if err := validateMapEntries(entries); err != nil {
		return nil, frame.ErrInvalidFrame
	}
	return entries, nil
}

// readMapBytes matches frame.ReadBytes while distinguishing a receiver-local
// map budget from an invalid wire frame. It performs the length check before
// slicing or copying the declared bytes.
func readMapBytes(data []byte, position, frameLimit, optionLimit int) ([]byte, int, error) {
	length, next, ok := frame.ReadUvarint(data, position)
	if !ok || next > len(data) || frameLimit < 0 || length > uint64(len(data)-next) || length > uint64(frameLimit) {
		return nil, position, frame.ErrInvalidFrame
	}
	if optionLimit > 0 && length > uint64(optionLimit) {
		return nil, position, ErrResourceLimit
	}
	return data[next : next+int(length)], next + int(length), nil
}

func sortedMapKeys(entries map[string]mapEntry) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func greatestMapFrontierTag(frontier map[string]crdt.Tag) (crdt.Tag, bool) {
	var greatest crdt.Tag
	found := false
	for _, tag := range frontier {
		if !found || greatest.Compare(tag) < 0 {
			greatest, found = tag, true
		}
	}
	return greatest, found
}
