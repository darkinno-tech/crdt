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

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
)

var (
	ErrInvalidCodec     = errors.New("lww: invalid element codec")
	ErrInvalidReplicaID = errors.New("lww: invalid replica ID")
	ErrNilSet           = errors.New("lww: nil set")
	ErrNilMap           = errors.New("lww: nil map")
	ErrCodecMismatch    = errors.New("lww: codec ID mismatch")
	ErrInvalidSetDelta  = errors.New("lww: invalid LWW-Set delta")
	ErrInvalidSetSnap   = errors.New("lww: invalid LWW-Set snapshot")
	ErrInvalidKey       = errors.New("lww: invalid key")
	ErrInvalidDelta     = errors.New("lww: invalid map delta")
	ErrInvalidSnapshot  = errors.New("lww: invalid map snapshot")
	ErrTagConflict      = errors.New("lww: conflicting values for one tag")
	ErrResourceLimit    = errors.New("lww: resource limit exceeded")
	ErrUnsafeCompaction = errors.New("lww: unsafe tombstone compaction")
)

// SemanticsVersion is the immutable LWW set/map v1 contract. It must match
// the value negotiated in a replica manifest for TypeIDs 7/8 or 9/10.
const SemanticsVersion uint64 = crdt.SemanticsVersionLWWSet

// SetFrameType returns the stable LWW-Set v1 state/delta pair.
func SetFrameType() crdt.FrameType {
	return crdt.FrameType{StateID: crdt.TypeIDLWWSetState, DeltaID: crdt.TypeIDLWWSetDelta, SemanticsVersion: SemanticsVersion, UsesHLC: true}
}

// MapFrameType returns the stable LWW-Map v1 state/delta pair.
func MapFrameType() crdt.FrameType {
	return crdt.FrameType{StateID: crdt.TypeIDLWWMapState, DeltaID: crdt.TypeIDLWWMapDelta, SemanticsVersion: crdt.SemanticsVersionLWWMap, UsesHLC: true}
}

type mapEntry struct {
	tag     crdt.Tag
	present bool
	value   []byte
}

// MapDelta is a joinable partial LWW-Map state. Its contents are deliberately
// opaque so callers cannot mutate an entry after it has been handed to a
// replica or coalescer.
type MapDelta struct{ entries map[string]mapEntry }

// MapOptions bounds retained LWW-Map state, including delete tombstones.
// MaxKeyBytes and MaxValueBytes apply to local writes and accepted deltas, so
// repeated small frames cannot grow one replica beyond its group budget.
type MapOptions struct {
	MaxEntries    int
	MaxKeyBytes   int
	MaxValueBytes int
}

// DefaultMapOptions returns conservative defaults aligned with the default
// framed element and byte limits.
func DefaultMapOptions() MapOptions {
	return MapOptions{MaxEntries: 1 << 20, MaxKeyBytes: 1 << 20, MaxValueBytes: 1 << 20}
}

func (o MapOptions) valid() bool {
	return o.MaxEntries > 0 && o.MaxKeyBytes > 0 && o.MaxValueBytes > 0
}

// Map is a byte-value LWW map. Returning and accepting copies prevents a
// caller from modifying replicated state through a shared slice. Values are
// deliberately opaque; applications may use a deterministic JSON, protobuf,
// or domain codec above this type.
type Map struct {
	mu        sync.RWMutex
	replicaID string
	clock     *clock.HLC
	options   MapOptions
	entries   map[string]mapEntry
	tags      map[crdt.Tag]string
}

var _ crdt.CRDT[*Map] = (*Map)(nil)
var _ crdt.DeltaCapable[*Map, MapDelta] = (*Map)(nil)

func NewMap(replicaID string) (*Map, error) {
	return NewMapWithOptions(replicaID, DefaultMapOptions())
}

// NewMapWithOptions constructs a map with explicit retained-state limits.
func NewMapWithOptions(replicaID string, options MapOptions) (*Map, error) {
	return NewMapFromClockWithOptions(clock.State{ReplicaID: replicaID}, options)
}

func NewMapFromClock(state clock.State) (*Map, error) {
	return NewMapFromClockWithOptions(state, DefaultMapOptions())
}

// NewMapFromClockWithOptions restores a replica clock with explicit retained
// state limits. Persist the clock atomically with a complete snapshot before
// reusing its replica ID.
func NewMapFromClockWithOptions(state clock.State, options MapOptions) (*Map, error) {
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
	return &Map{
		replicaID: state.ReplicaID,
		clock:     hlc,
		options:   options,
		entries:   make(map[string]mapEntry),
		tags:      make(map[crdt.Tag]string),
	}, nil
}

func (m *Map) ClockState() clock.State {
	if m == nil || m.clock == nil {
		return clock.State{}
	}
	return m.clock.Snapshot()
}

// Set writes a value and preserves the original non-delta API.
func (m *Map) Set(key string, value []byte) error {
	_, err := m.SetWithDelta(key, value)
	return err
}

// Delete removes key and preserves the original non-delta API.
func (m *Map) Delete(key string) error {
	_, err := m.DeleteWithDelta(key)
	return err
}

// SetWithDelta writes a value and returns the joinable delta for this write.
func (m *Map) SetWithDelta(key string, value []byte) (MapDelta, error) {
	return m.writeDelta(key, value, true)
}

// DeleteWithDelta removes key and returns the joinable delete delta.
func (m *Map) DeleteWithDelta(key string) (MapDelta, error) {
	return m.writeDelta(key, nil, false)
}

func (m *Map) writeDelta(key string, value []byte, present bool) (MapDelta, error) {
	if m == nil || m.clock == nil {
		return MapDelta{}, ErrNilMap
	}
	if err := validateMapWrite(key, value, present, m.options); err != nil {
		return MapDelta{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.entries[key]; !exists && len(m.entries) >= m.options.MaxEntries {
		return MapDelta{}, ErrResourceLimit
	}
	tag, err := m.clock.Now()
	if err != nil {
		return MapDelta{}, err
	}
	if owner, exists := m.tags[tag]; exists && owner != key {
		return MapDelta{}, ErrTagConflict
	}
	incoming := mapEntry{tag: tag, present: present}
	if present {
		incoming.value = append([]byte(nil), value...)
	}
	if current, exists := m.entries[key]; !exists || current.tag.Compare(tag) < 0 {
		delete(m.tags, current.tag)
		m.entries[key] = incoming
		m.tags[tag] = key
	}
	return MapDelta{entries: map[string]mapEntry{key: incoming}}, nil
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

// HasEntry reports whether m retains any metadata for key, including a delete
// tombstone. It is intended for bounded wrappers that must distinguish a new
// key from an idempotent replay without exposing the entry's tag or value.
func (m *Map) HasEntry(key string) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	_, ok := m.entries[key]
	m.mu.RUnlock()
	return ok
}

// EntryCount returns the total number of retained map entries, including
// tombstones. It is useful for callers that impose their own retention budget.
func (m *Map) EntryCount() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	count := len(m.entries)
	m.mu.RUnlock()
	return count
}

// EntryKeys returns every retained key, including delete tombstones, in
// lexical order. It is for resource accounting; use Keys when only visible
// application values are needed.
func (m *Map) EntryKeys() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	keys := make([]string, 0, len(m.entries))
	for key := range m.entries {
		keys = append(keys, key)
	}
	m.mu.RUnlock()
	sort.Strings(keys)
	return keys
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

// ApplyDelta joins a validated partial map state into m. It validates every
// entry and detects equal-tag conflicts before mutating the map or HLC.
func (m *Map) ApplyDelta(delta MapDelta) error {
	if m == nil || m.clock == nil {
		return ErrNilMap
	}
	return m.applyOwnedMapEntries(cloneMapEntries(delta.entries))
}

// applyOwnedMapEntries joins entries that are already owned by the caller.
// Merge uses it after taking its one source snapshot, avoiding a second full
// map clone while retaining ApplyDelta's public ownership boundary.
func (m *Map) applyOwnedMapEntries(entries map[string]mapEntry) error {
	if err := validateMapEntries(entries); err != nil {
		return err
	}
	if err := validateMapEntriesWithOptions(entries, m.options); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ensureMapTagsCompatible(m.tags, entries); err != nil {
		return err
	}
	for key, incoming := range entries {
		if current, exists := m.entries[key]; exists && mapEntriesConflict(current, incoming) {
			return ErrTagConflict
		}
	}
	if mapEntriesSubsumed(m.entries, entries) {
		return nil
	}
	if len(m.entries)+newMapEntries(entries, m.entries) > m.options.MaxEntries {
		return ErrResourceLimit
	}
	if tag, ok := greatestMapTag(entries); ok {
		if err := m.clock.Witness(tag); err != nil {
			return err
		}
	}
	for key, incoming := range entries {
		current, exists := m.entries[key]
		if !exists || current.tag.Compare(incoming.tag) < 0 {
			if exists {
				delete(m.tags, current.tag)
			}
			m.entries[key] = incoming
			m.tags[incoming.tag] = key
		}
	}
	return nil
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
	if err := validateMapEntries(entries); err != nil {
		return err
	}
	return m.applyOwnedMapEntries(entries)
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

// Frontier returns the greatest map-entry tag per replica. The returned map
// is owned by the caller and includes delete tombstones.
func (m *Map) Frontier() map[string]crdt.Tag {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return mapFrontier(m.entries)
}

// TombstoneTags returns retained delete tags in canonical order. The list is
// an input to an external exact-acknowledgement epoch; it is not proof that a
// tombstone may be removed by itself.
func (m *Map) TombstoneTags() []crdt.Tag {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	tags := make([]crdt.Tag, 0)
	for _, entry := range m.entries {
		if !entry.present {
			tags = append(tags, entry.tag)
		}
	}
	m.mu.RUnlock()
	sortTags(tags)
	return tags
}

// CompactTombstones removes exactly requested deleted entries. Call it only
// after every active member has acknowledged the exact tags in one
// authenticated membership epoch, a post-compaction snapshot is durable, and
// old deltas have been retired. Unknown tags are ignored; attempting to remove
// a live entry or passing an invalid tag leaves the map unchanged.
func (m *Map) CompactTombstones(tags []crdt.Tag) (int, error) {
	if m == nil {
		return 0, ErrNilMap
	}
	for _, tag := range tags {
		if !tag.Valid() {
			return 0, ErrUnsafeCompaction
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	byTag := make(map[crdt.Tag]string, len(m.entries))
	for key, entry := range m.entries {
		if owner, exists := byTag[entry.tag]; exists && owner != key {
			return 0, ErrUnsafeCompaction
		}
		byTag[entry.tag] = key
	}
	compact := make([]string, 0, len(tags))
	seen := make(map[crdt.Tag]struct{}, len(tags))
	for _, tag := range tags {
		if _, duplicate := seen[tag]; duplicate {
			continue
		}
		seen[tag] = struct{}{}
		key, exists := byTag[tag]
		if !exists {
			continue
		}
		if m.entries[key].present {
			return 0, ErrUnsafeCompaction
		}
		compact = append(compact, key)
	}
	for _, key := range compact {
		entry := m.entries[key]
		delete(m.entries, key)
		delete(m.tags, entry.tag)
	}
	return len(compact), nil
}

// Merge joins two map deltas without modifying either input.
func (d MapDelta) Merge(other MapDelta) (MapDelta, error) {
	if err := validateMapEntries(d.entries); err != nil {
		return MapDelta{}, err
	}
	if err := validateMapEntries(other.entries); err != nil {
		return MapDelta{}, err
	}
	merged := cloneMapEntries(d.entries)
	for key, incoming := range other.entries {
		if current, exists := merged[key]; exists && mapEntriesConflict(current, incoming) {
			return MapDelta{}, ErrTagConflict
		}
		if current, exists := merged[key]; !exists || current.tag.Compare(incoming.tag) < 0 {
			merged[key] = incoming
		}
	}
	if err := validateMapEntries(merged); err != nil {
		return MapDelta{}, err
	}
	return MapDelta{entries: merged}, nil
}

// Keys returns all keys represented by d, including delete tombstones, in
// lexical order. The returned slice is owned by the caller.
func (d MapDelta) Keys() []string {
	keys := make([]string, 0, len(d.entries))
	for key := range d.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ValidateValues validates every visible value in d without exposing its
// private tags or allowing callers to mutate the delta. A nil validator is an
// error so a wrapper cannot accidentally skip schema validation at a network
// boundary.
func (d MapDelta) ValidateValues(validate func(key string, value []byte) error) error {
	if validate == nil {
		return ErrInvalidDelta
	}
	if err := validateMapEntries(d.entries); err != nil {
		return err
	}
	for key, entry := range d.entries {
		if entry.present {
			if err := validate(key, append([]byte(nil), entry.value...)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidateValues validates every visible value in m without exposing its
// private entries. The callback receives a copy and is called without m's
// lock, so it may safely perform ordinary validation work.
func (m *Map) ValidateValues(validate func(key string, value []byte) error) error {
	if m == nil || validate == nil {
		return ErrNilMap
	}
	m.mu.RLock()
	values := make(map[string][]byte, len(m.entries))
	for key, entry := range m.entries {
		if entry.present {
			values[key] = append([]byte(nil), entry.value...)
		}
	}
	m.mu.RUnlock()
	for key, value := range values {
		if err := validate(key, value); err != nil {
			return err
		}
	}
	return nil
}

func cloneSetEntries[T comparable](source map[T]setEntry[T]) map[T]setEntry[T] {
	out := make(map[T]setEntry[T], len(source))
	for value, entry := range source {
		out[value] = entry
	}
	return out
}

func validateSetEntries[T comparable](entries map[T]setEntry[T]) error {
	owners := make(map[crdt.Tag]T, len(entries))
	for value, entry := range entries {
		if !entry.tag.Valid() {
			return ErrInvalidSetDelta
		}
		if owner, exists := owners[entry.tag]; exists && owner != value {
			return ErrTagConflict
		}
		owners[entry.tag] = value
	}
	return nil
}

func ensureSetTagsCompatible[T comparable](tags map[crdt.Tag]T, entries map[T]setEntry[T]) error {
	for value, entry := range entries {
		if owner, exists := tags[entry.tag]; exists && owner != value {
			return ErrTagConflict
		}
	}
	return nil
}

func setEntriesSubsumed[T comparable](existing, incoming map[T]setEntry[T]) bool {
	for value, entry := range incoming {
		current, exists := existing[value]
		if !exists || current.tag.Compare(entry.tag) < 0 || setEntriesConflict(current, entry) {
			return false
		}
	}
	return true
}

func newSetEntries[T comparable](incoming, existing map[T]setEntry[T]) int {
	count := 0
	for value := range incoming {
		if _, exists := existing[value]; !exists {
			count++
		}
	}
	return count
}

func setTagIndex[T comparable](entries map[T]setEntry[T]) (map[crdt.Tag]T, error) {
	if err := validateSetEntries(entries); err != nil {
		return nil, err
	}
	tags := make(map[crdt.Tag]T, len(entries))
	for value, entry := range entries {
		tags[entry.tag] = value
	}
	return tags, nil
}

func setEntriesConflict[T comparable](left, right setEntry[T]) bool {
	return left.tag == right.tag && left.present != right.present
}

func setFrontier[T comparable](entries map[T]setEntry[T]) map[string]crdt.Tag {
	frontier := make(map[string]crdt.Tag)
	for _, entry := range entries {
		if current, ok := frontier[entry.tag.ReplicaID]; !ok || current.Compare(entry.tag) < 0 {
			frontier[entry.tag.ReplicaID] = entry.tag
		}
	}
	return frontier
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

func validateMapEntries(entries map[string]mapEntry) error {
	owners := make(map[crdt.Tag]string, len(entries))
	for key, entry := range entries {
		if strings.TrimSpace(key) == "" || !entry.tag.Valid() || (!entry.present && len(entry.value) != 0) {
			return ErrInvalidDelta
		}
		if owner, exists := owners[entry.tag]; exists && owner != key {
			return ErrTagConflict
		}
		owners[entry.tag] = key
	}
	return nil
}

func validateMapWrite(key string, value []byte, present bool, options MapOptions) error {
	if strings.TrimSpace(key) == "" || len(key) > options.MaxKeyBytes {
		return ErrInvalidKey
	}
	if present && len(value) > options.MaxValueBytes {
		return ErrResourceLimit
	}
	return nil
}

func validateMapEntriesWithOptions(entries map[string]mapEntry, options MapOptions) error {
	for key, entry := range entries {
		if err := validateMapWrite(key, entry.value, entry.present, options); err != nil {
			return err
		}
	}
	return nil
}

func ensureMapTagsCompatible(tags map[crdt.Tag]string, entries map[string]mapEntry) error {
	for key, entry := range entries {
		if owner, exists := tags[entry.tag]; exists && owner != key {
			return ErrTagConflict
		}
	}
	return nil
}

func mapEntriesSubsumed(existing, incoming map[string]mapEntry) bool {
	for key, entry := range incoming {
		current, exists := existing[key]
		if !exists || current.tag.Compare(entry.tag) < 0 || mapEntriesConflict(current, entry) {
			return false
		}
	}
	return true
}

func newMapEntries(incoming, existing map[string]mapEntry) int {
	count := 0
	for key := range incoming {
		if _, exists := existing[key]; !exists {
			count++
		}
	}
	return count
}

func mapTagIndex(entries map[string]mapEntry) (map[crdt.Tag]string, error) {
	if err := validateMapEntries(entries); err != nil {
		return nil, err
	}
	tags := make(map[crdt.Tag]string, len(entries))
	for key, entry := range entries {
		tags[entry.tag] = key
	}
	return tags, nil
}

func mapEntriesConflict(left, right mapEntry) bool {
	return left.tag == right.tag && (left.present != right.present || !bytes.Equal(left.value, right.value))
}

func mapFrontier(entries map[string]mapEntry) map[string]crdt.Tag {
	frontier := make(map[string]crdt.Tag)
	for _, entry := range entries {
		if current, ok := frontier[entry.tag.ReplicaID]; !ok || current.Compare(entry.tag) < 0 {
			frontier[entry.tag.ReplicaID] = entry.tag
		}
	}
	return frontier
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

func sortTags(tags []crdt.Tag) {
	sort.Slice(tags, func(i, j int) bool { return tags[i].Compare(tags[j]) < 0 })
}
