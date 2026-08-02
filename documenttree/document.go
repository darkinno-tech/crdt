// Package documenttree implements a bounded, framed, fully nested
// document-tree CRDT.
//
// A document tree combines LWW maps and RGA arrays through single-owner child
// objects. It intentionally does not reuse the wire formats of lww.Map,
// list.RGA, or Yjs: a manifest selects this complete protocol as one unit.
package documenttree

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

var (
	ErrNilDocument     = errors.New("documenttree: nil document")
	ErrInvalidReplica  = errors.New("documenttree: invalid replica ID")
	ErrInvalidRoot     = errors.New("documenttree: invalid root name")
	ErrInvalidKey      = errors.New("documenttree: invalid map key")
	ErrInvalidValue    = errors.New("documenttree: invalid value")
	ErrInvalidDelta    = errors.New("documenttree: invalid document-tree delta")
	ErrInvalidState    = errors.New("documenttree: invalid document-tree state")
	ErrIncompleteState = errors.New("documenttree: incomplete document-tree state")
	ErrTypeMismatch    = errors.New("documenttree: object type mismatch")
	ErrUnknownObject   = errors.New("documenttree: unknown object")
	ErrRange           = errors.New("documenttree: array range outside visible projection")
	ErrTagConflict     = errors.New("documenttree: conflicting use of a mutation tag")
	ErrResourceLimit   = errors.New("documenttree: resource limit exceeded")
)

// SemanticsVersion is the immutable document-tree-v2 protocol contract.
const SemanticsVersion uint64 = crdt.SemanticsVersionDocumentTree

// StableFrameType returns the state/delta pair selected by a document-tree-v2
// replication manifest. The protocol owns its HLC because roots, map writes,
// and array positions all use HLC tags.
func StableFrameType() crdt.FrameType {
	return crdt.FrameType{
		StateID:          crdt.TypeIDDocumentTreeState,
		DeltaID:          crdt.TypeIDDocumentTreeDelta,
		SemanticsVersion: SemanticsVersion,
		UsesHLC:          true,
	}
}

// ObjectID is the immutable identity of a created child object or array
// position. The zero ID is reserved as the RGA array root anchor.
type ObjectID = crdt.Tag

// Kind is the fixed CRDT interpretation of one object.
type Kind uint8

const (
	KindMap Kind = iota + 1
	KindArray
)

func (k Kind) valid() bool { return k == KindMap || k == KindArray }

// ObjectRef names a child object. It is returned as a Value and is immutable:
// an object can have exactly one parent creation record and cannot be moved or
// inserted a second time.
type ObjectRef struct {
	ID   ObjectID
	Kind Kind
}

// ValueKind classifies the payload retained by maps and array positions.
type ValueKind uint8

const (
	ValueBytes ValueKind = iota + 1
	ValueObject
)

// Value is an owned projection value. Bytes is copied on both input and
// output; applications must treat its content as schema-selected opaque data.
type Value struct {
	Kind   ValueKind
	Bytes  []byte
	Object ObjectRef
}

func Bytes(value []byte) Value {
	return Value{Kind: ValueBytes, Bytes: append([]byte(nil), value...)}
}

// Options bounds retained and incomplete document state. Limits apply to one
// document-tree group; hosts must still reject oversized transport bodies
// before allocating them and authorize the one complete replication group.
type Options struct {
	MaxRoots             int
	MaxObjects           int
	MaxMapEntries        int
	MaxArrayNodes        int
	MaxArrayTombstones   int
	MaxPendingOperations int
	MaxPendingBytes      int
	MaxDepth             int
	MaxKeyBytes          int
	MaxValueBytes        int
}

// DefaultOptions returns conservative per-document retention limits.
func DefaultOptions() Options {
	return Options{
		MaxRoots:             1 << 12,
		MaxObjects:           1 << 16,
		MaxMapEntries:        1 << 20,
		MaxArrayNodes:        1 << 20,
		MaxArrayTombstones:   1 << 20,
		MaxPendingOperations: 1 << 16,
		MaxPendingBytes:      4 << 20,
		MaxDepth:             128,
		MaxKeyBytes:          1 << 12,
		MaxValueBytes:        1 << 20,
	}
}

func (o Options) valid() bool {
	return o.MaxRoots > 0 && o.MaxObjects > 0 && o.MaxMapEntries > 0 &&
		o.MaxArrayNodes > 0 && o.MaxArrayTombstones > 0 &&
		o.MaxPendingOperations > 0 && o.MaxPendingBytes > 0 && o.MaxDepth > 0 &&
		o.MaxKeyBytes > 0 && o.MaxValueBytes > 0
}

type ownerKind uint8

const (
	ownerRoot ownerKind = iota + 1
	ownerMap
	ownerArray
)

type objectOwner struct {
	kind     ownerKind
	parent   ObjectID
	rootName string
	key      string
	position ObjectID
}

type objectDecl struct {
	id    ObjectID
	kind  Kind
	owner objectOwner
}

type rootRecord struct {
	name string
	id   ObjectID
	kind Kind
}

type mapEntry struct {
	tag     crdt.Tag
	present bool
	value   Value
}

type arrayNode struct {
	id     ObjectID
	parent ObjectID
	value  Value
}

type documentState struct {
	roots      map[string]rootRecord
	objects    map[ObjectID]objectDecl
	maps       map[ObjectID]map[string]mapEntry
	arrays     map[ObjectID]map[ObjectID]arrayNode
	tombstones map[ObjectID]map[ObjectID]struct{}
}

func newDocumentState() documentState {
	return documentState{
		roots:      make(map[string]rootRecord),
		objects:    make(map[ObjectID]objectDecl),
		maps:       make(map[ObjectID]map[string]mapEntry),
		arrays:     make(map[ObjectID]map[ObjectID]arrayNode),
		tombstones: make(map[ObjectID]map[ObjectID]struct{}),
	}
}

func (s documentState) clone() documentState {
	result := newDocumentState()
	for key, value := range s.roots {
		result.roots[key] = value
	}
	for id, value := range s.objects {
		result.objects[id] = value
	}
	for target, entries := range s.maps {
		copied := make(map[string]mapEntry, len(entries))
		for key, entry := range entries {
			entry.value = cloneValue(entry.value)
			copied[key] = entry
		}
		result.maps[target] = copied
	}
	for target, nodes := range s.arrays {
		copied := make(map[ObjectID]arrayNode, len(nodes))
		for id, node := range nodes {
			node.value = cloneValue(node.value)
			copied[id] = node
		}
		result.arrays[target] = copied
	}
	for target, tombstones := range s.tombstones {
		copied := make(map[ObjectID]struct{}, len(tombstones))
		for id := range tombstones {
			copied[id] = struct{}{}
		}
		result.tombstones[target] = copied
	}
	return result
}

func cloneValue(value Value) Value {
	value.Bytes = append([]byte(nil), value.Bytes...)
	return value
}

// Delta is an opaque, joinable partial document-tree state. Local mutation or
// UnmarshalDelta are the only ways to construct a meaningful Delta. A local
// delta may retain its already-validated canonical outbox frame so a facade
// can hand it off without serializing the same mutation twice.
type Delta struct {
	state documentState
	frame []byte
}

// Document is a bounded collection of LWW maps and RGA arrays. The document
// lock protects every object so a mutation that creates a child is atomic with
// its parent reference; handles never carry independent mutable state.
type Document struct {
	mu           sync.RWMutex
	replicaID    string
	clock        *clock.HLC
	options      Options
	outputLimits *frame.DecoderLimits
	state        documentState
}

var _ crdt.CRDT[*Document] = (*Document)(nil)
var _ crdt.DeltaCapable[*Document, Delta] = (*Document)(nil)

// New constructs an empty document with the default retention policy.
func New(replicaID string) (*Document, error) {
	return NewWithOptions(replicaID, DefaultOptions())
}

// NewWithOptions constructs an empty document with explicit local limits.
func NewWithOptions(replicaID string, options Options) (*Document, error) {
	return NewFromClockWithOptions(clock.State{ReplicaID: replicaID}, options)
}

// NewWithOptionsAndOutputLimits constructs a document that rejects a local
// mutation before it changes state when its canonical delta cannot fit the
// selected outbox frame budget. Remote decoding remains independently bounded
// by the limits passed to UnmarshalDeltaWithOptions.
func NewWithOptionsAndOutputLimits(replicaID string, options Options, outputLimits frame.DecoderLimits) (*Document, error) {
	return NewFromClockWithOptionsAndOutputLimits(clock.State{ReplicaID: replicaID}, options, outputLimits)
}

// NewFromClockWithOptions restores an empty document using a persisted HLC.
func NewFromClockWithOptions(state clock.State, options Options) (*Document, error) {
	return newFromClockWithOptions(state, options, nil)
}

// NewFromClockWithOptionsAndOutputLimits restores a document with explicit
// retained-state and local outbox budgets. The output budget is checked before
// every local mutation is committed, so an encoding failure cannot create a
// local-only update.
func NewFromClockWithOptionsAndOutputLimits(state clock.State, options Options, outputLimits frame.DecoderLimits) (*Document, error) {
	return newFromClockWithOptions(state, options, &outputLimits)
}

func newFromClockWithOptions(state clock.State, options Options, outputLimits *frame.DecoderLimits) (*Document, error) {
	if !(crdt.Tag{ReplicaID: state.ReplicaID}).Valid() {
		return nil, ErrInvalidReplica
	}
	if !options.valid() {
		return nil, ErrResourceLimit
	}
	hlc, err := clock.NewHLCFromState(state)
	if err != nil {
		return nil, err
	}
	document := &Document{replicaID: state.ReplicaID, clock: hlc, options: options, state: newDocumentState()}
	if outputLimits != nil {
		limits := *outputLimits
		if _, err := marshalState(crdt.TypeIDDocumentTreeDelta, newDocumentState(), options, limits, true); err != nil {
			return nil, err
		}
		document.outputLimits = &limits
	}
	return document, nil
}

// ClockState returns the state that must be atomically persisted with a
// complete document snapshot before this replica ID is used after restart.
func (d *Document) ClockState() clock.State {
	if d == nil {
		return clock.State{}
	}
	d.mu.RLock()
	hlc := d.clock
	d.mu.RUnlock()
	if hlc == nil {
		return clock.State{}
	}
	return hlc.Snapshot()
}

// CreateRootMap creates one named map root, or returns the current map root
// without a delta when it already exists. Concurrent roots of the same name
// resolve by their creation tag; a schema should not reuse a root name for a
// different Kind.
func (d *Document) CreateRootMap(name string) (*Map, Delta, error) {
	return d.createRoot(name, KindMap)
}

// CreateRootArray creates one named array root, or returns the current array
// root without a delta when it already exists.
func (d *Document) CreateRootArray(name string) (*Array, Delta, error) {
	deltaMap, delta, err := d.createRoot(name, KindArray)
	if err != nil {
		return nil, delta, err
	}
	return &Array{document: d, id: deltaMap.id}, delta, nil
}

func (d *Document) createRoot(name string, kind Kind) (*Map, Delta, error) {
	if d == nil {
		return nil, Delta{}, ErrNilDocument
	}
	if !d.validName(name, d.options.MaxKeyBytes) {
		return nil, Delta{}, ErrInvalidRoot
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.clock == nil {
		return nil, Delta{}, ErrNilDocument
	}
	if current, exists := d.state.roots[name]; exists {
		if current.kind != kind {
			return nil, Delta{}, ErrTypeMismatch
		}
		if _, exists := d.state.objects[current.id]; exists {
			return &Map{document: d, id: current.id}, Delta{}, nil
		}
	}
	if (!rootExists(d.state.roots, name) && len(d.state.roots) >= d.options.MaxRoots) || len(d.state.objects) >= d.options.MaxObjects {
		return nil, Delta{}, ErrResourceLimit
	}
	id, nextClock, err := d.nextLocalTagLocked()
	if err != nil {
		return nil, Delta{}, err
	}
	state := newDocumentState()
	state.roots[name] = rootRecord{name: name, id: id, kind: kind}
	state.objects[id] = objectDecl{id: id, kind: kind, owner: objectOwner{kind: ownerRoot, rootName: name}}
	delta := Delta{state: state}
	if err := d.applyLocalLocked(&delta, nextClock); err != nil {
		return nil, Delta{}, err
	}
	return &Map{document: d, id: id}, delta, nil
}

// RootMap returns the current visible map root. It is false while a root
// declaration is waiting for its object record, or when the root is an array.
func (d *Document) RootMap(name string) (*Map, bool) {
	if d == nil || !d.validName(name, d.options.MaxKeyBytes) {
		return nil, false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	root, ok := d.state.roots[name]
	if !ok || root.kind != KindMap || !d.isObjectKindLocked(root.id, KindMap) {
		return nil, false
	}
	return &Map{document: d, id: root.id}, true
}

// RootArray returns the current visible array root.
func (d *Document) RootArray(name string) (*Array, bool) {
	if d == nil || !d.validName(name, d.options.MaxKeyBytes) {
		return nil, false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	root, ok := d.state.roots[name]
	if !ok || root.kind != KindArray || !d.isObjectKindLocked(root.id, KindArray) {
		return nil, false
	}
	return &Array{document: d, id: root.id}, true
}

// Map returns a handle only for an integrated map object.
func (d *Document) Map(id ObjectID) (*Map, bool) {
	if d == nil || !id.Valid() {
		return nil, false
	}
	d.mu.RLock()
	ok := d.isObjectKindLocked(id, KindMap)
	d.mu.RUnlock()
	return &Map{document: d, id: id}, ok
}

// Array returns a handle only for an integrated array object.
func (d *Document) Array(id ObjectID) (*Array, bool) {
	if d == nil || !id.Valid() {
		return nil, false
	}
	d.mu.RLock()
	ok := d.isObjectKindLocked(id, KindArray)
	d.mu.RUnlock()
	return &Array{document: d, id: id}, ok
}

// ApplyDelta joins a decoded delta atomically. It retains bounded unresolved
// records for parent-before-child reordering but never serializes them as a
// complete state.
func (d *Document) ApplyDelta(delta Delta) error {
	if d == nil {
		return ErrNilDocument
	}
	if err := validateState(delta.state, d.options, true); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.clock == nil {
		return ErrNilDocument
	}
	candidate, changed, greatest, err := joinState(d.state, delta.state, d.options)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if greatest.Valid() {
		if err := d.clock.Witness(greatest); err != nil {
			return err
		}
	}
	d.state = candidate
	return nil
}

func (d *Document) applyLocalLocked(delta *Delta, nextClock *clock.HLC) error {
	if delta == nil {
		return ErrInvalidDelta
	}
	if nextClock == nil {
		return ErrNilDocument
	}
	if d.outputLimits != nil {
		encoded, err := marshalState(crdt.TypeIDDocumentTreeDelta, delta.state, d.options, *d.outputLimits, true)
		if err != nil {
			return err
		}
		delta.frame = encoded
	}
	candidate, changed, _, err := joinState(d.state, delta.state, d.options)
	if err != nil {
		return err
	}
	if changed {
		d.state = candidate
	}
	d.clock = nextClock
	return nil
}

// nextLocalTagLocked creates a candidate clock and tag without changing d.
// The caller installs the candidate only after all local-state and frame
// preflights pass, preserving the checkpoint's state/HLC atomicity on error.
func (d *Document) nextLocalTagLocked() (crdt.Tag, *clock.HLC, error) {
	if d == nil || d.clock == nil {
		return crdt.Tag{}, nil, ErrNilDocument
	}
	nextClock, err := clock.NewHLCFromState(d.clock.Snapshot())
	if err != nil {
		return crdt.Tag{}, nil, err
	}
	tag, err := nextClock.Now()
	if err != nil {
		return crdt.Tag{}, nil, err
	}
	return tag, nextClock, nil
}

// Merge joins another complete or incomplete document state without exposing
// mutable internals. Both documents retain their independent HLC identities.
func (d *Document) Merge(other *Document) error {
	if d == nil || other == nil {
		return ErrNilDocument
	}
	if d == other {
		return nil
	}
	other.mu.RLock()
	state := other.state.clone()
	other.mu.RUnlock()
	return d.ApplyDelta(Delta{state: state})
}

// State returns a diagnostic summary. It excludes user values and object IDs.
func (d *Document) State() crdt.StateSnapshot {
	if d == nil {
		return crdt.StateSnapshot{Type: "document-tree"}
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	objects := 0
	for _, root := range d.state.roots {
		if d.isObjectKindLocked(root.id, root.kind) {
			objects++
		}
	}
	pending, _ := pendingState(d.state)
	tombs := countTombstones(d.state)
	return crdt.StateSnapshot{Type: "document-tree", ReplicaID: d.replicaID, ElementCount: objects, TombstoneCount: tombs + pending}
}

// MarshalBinary returns a canonical complete document-tree state. A state
// with missing parent/object records is intentionally not serializable.
func (d *Document) MarshalBinary() ([]byte, error) {
	return d.MarshalBinaryWithLimits(frame.DefaultLimits())
}

func (d *Document) MarshalBinaryWithLimits(limits frame.DecoderLimits) ([]byte, error) {
	if d == nil {
		return nil, ErrNilDocument
	}
	d.mu.RLock()
	state := d.state.clone()
	d.mu.RUnlock()
	return marshalState(crdt.TypeIDDocumentTreeState, state, d.options, limits, false)
}

// MarshalDeltaWithLimits encodes delta using this document's retained-value
// policy and an explicit transport budget. Documents created with an output
// budget have already checked the same encoding before accepting local delta,
// so this is the safe outbox handoff for those mutations.
func (d *Document) MarshalDeltaWithLimits(delta Delta, limits frame.DecoderLimits) ([]byte, error) {
	if d == nil {
		return nil, ErrNilDocument
	}
	return marshalState(crdt.TypeIDDocumentTreeDelta, delta.state, d.options, limits, true)
}

// MarshalLocalDelta returns the already-validated frame for a delta returned
// by a local mutation on d when d has an output budget. It otherwise encodes
// with that budget. Callers must use it only with a delta from this document;
// untrusted deltas must go through UnmarshalDeltaWithOptions instead.
func (d *Document) MarshalLocalDelta(delta Delta) ([]byte, error) {
	if d == nil {
		return nil, ErrNilDocument
	}
	if delta.frame != nil {
		return append([]byte(nil), delta.frame...), nil
	}
	if d.outputLimits == nil {
		return d.MarshalDeltaWithLimits(delta, frame.DefaultLimits())
	}
	return d.MarshalDeltaWithLimits(delta, *d.outputLimits)
}

// MarshalBinaryWithClockState returns state and the HLC state to persist in
// one transaction before a replica ID can be reused.
func (d *Document) MarshalBinaryWithClockState() ([]byte, clock.State, error) {
	return d.MarshalBinaryWithClockStateAndLimits(frame.DefaultLimits())
}

func (d *Document) MarshalBinaryWithClockStateAndLimits(limits frame.DecoderLimits) ([]byte, clock.State, error) {
	if d == nil {
		return nil, clock.State{}, ErrNilDocument
	}
	d.mu.RLock()
	if d.clock == nil {
		d.mu.RUnlock()
		return nil, clock.State{}, ErrNilDocument
	}
	state, clockState := d.state.clone(), d.clock.Snapshot()
	d.mu.RUnlock()
	encoded, err := marshalState(crdt.TypeIDDocumentTreeState, state, d.options, limits, false)
	return encoded, clockState, err
}

// SnapshotCurrentState creates a validated HLC-backed recovery snapshot.
func (d *Document) SnapshotCurrentState() (snapshot.Snapshot, error) {
	return d.SnapshotCurrentStateWithLimits(frame.DefaultLimits())
}

func (d *Document) SnapshotCurrentStateWithLimits(limits frame.DecoderLimits) (snapshot.Snapshot, error) {
	if d == nil {
		return snapshot.Snapshot{}, ErrNilDocument
	}
	d.mu.RLock()
	if d.clock == nil {
		d.mu.RUnlock()
		return snapshot.Snapshot{}, ErrNilDocument
	}
	state, clockState := d.state.clone(), d.clock.Snapshot()
	d.mu.RUnlock()
	encoded, err := marshalState(crdt.TypeIDDocumentTreeState, state, d.options, limits, false)
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	return snapshot.NewValidatedWithClockState(encoded, documentFrontier(state), clockState, validateDocumentState)
}

// NewFromSnapshot restores a complete snapshot using default local limits.
func NewFromSnapshot(saved snapshot.Snapshot) (*Document, error) {
	return NewFromSnapshotWithOptions(saved, DefaultOptions(), frame.DefaultLimits())
}

// NewFromSnapshotWithOptions restores under caller-selected retention and
// decoder limits; a snapshot cannot widen either local budget.
func NewFromSnapshotWithOptions(saved snapshot.Snapshot, options Options, limits frame.DecoderLimits) (*Document, error) {
	if saved.TypeID != crdt.TypeIDDocumentTreeState {
		return nil, ErrInvalidState
	}
	clockState, ok := saved.ClockState()
	if !ok {
		return nil, ErrInvalidState
	}
	document, err := NewFromClockWithOptions(clockState, options)
	if err != nil {
		return nil, err
	}
	if err := document.UnmarshalBinaryWithLimits(saved.Bytes(), limits); err != nil {
		return nil, err
	}
	if greatest, ok := greatestFrontierTag(saved.Frontier()); ok {
		if err := document.clock.Witness(greatest); err != nil {
			return nil, err
		}
	}
	return document, nil
}

// UnmarshalBinary atomically replaces a document with one complete canonical
// state frame. It never accepts pending state into a recovery boundary.
func (d *Document) UnmarshalBinary(data []byte) error {
	return d.UnmarshalBinaryWithLimits(data, frame.DefaultLimits())
}

func (d *Document) UnmarshalBinaryWithLimits(data []byte, limits frame.DecoderLimits) error {
	if d == nil {
		return ErrNilDocument
	}
	state, err := unmarshalState(data, crdt.TypeIDDocumentTreeState, d.options, limits, false)
	if err != nil {
		return err
	}
	greatest := greatestStateTag(state)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.clock == nil {
		return ErrNilDocument
	}
	if greatest.Valid() {
		if err := d.clock.Witness(greatest); err != nil {
			return err
		}
	}
	d.state = state
	return nil
}

// Map is a lightweight handle to one integrated map object.
type Map struct {
	document *Document
	id       ObjectID
}

func (m *Map) ID() ObjectID {
	if m == nil {
		return ObjectID{}
	}
	return m.id
}

func (m *Map) Set(key string, value []byte) (Delta, error) {
	if m == nil || m.document == nil {
		return Delta{}, ErrNilDocument
	}
	return m.document.setMapValue(m.id, key, Bytes(value))
}

func (m *Map) Delete(key string) (Delta, error) {
	if m == nil || m.document == nil {
		return Delta{}, ErrNilDocument
	}
	return m.document.deleteMapValue(m.id, key)
}

func (m *Map) CreateMap(key string) (*Map, Delta, error) {
	if m == nil || m.document == nil {
		return nil, Delta{}, ErrNilDocument
	}
	return m.document.createMapChild(m.id, key, KindMap)
}

func (m *Map) CreateArray(key string) (*Array, Delta, error) {
	if m == nil || m.document == nil {
		return nil, Delta{}, ErrNilDocument
	}
	child, delta, err := m.document.createMapChild(m.id, key, KindArray)
	if err != nil {
		return nil, delta, err
	}
	return &Array{document: m.document, id: child.id}, delta, nil
}

func (m *Map) Get(key string) (Value, bool) {
	if m == nil || m.document == nil || !m.document.validName(key, m.document.options.MaxKeyBytes) {
		return Value{}, false
	}
	m.document.mu.RLock()
	defer m.document.mu.RUnlock()
	entries := m.document.state.maps[m.id]
	entry, ok := entries[key]
	if !ok || !entry.present {
		return Value{}, false
	}
	return cloneValue(entry.value), true
}

func (m *Map) Map(key string) (*Map, bool) {
	value, ok := m.Get(key)
	if !ok || value.Kind != ValueObject || value.Object.Kind != KindMap {
		return nil, false
	}
	return m.document.Map(value.Object.ID)
}

func (m *Map) Array(key string) (*Array, bool) {
	value, ok := m.Get(key)
	if !ok || value.Kind != ValueObject || value.Object.Kind != KindArray {
		return nil, false
	}
	return m.document.Array(value.Object.ID)
}

func (m *Map) Keys() []string {
	if m == nil || m.document == nil {
		return nil
	}
	m.document.mu.RLock()
	entries := m.document.state.maps[m.id]
	keys := make([]string, 0, len(entries))
	for key, entry := range entries {
		if entry.present {
			keys = append(keys, key)
		}
	}
	m.document.mu.RUnlock()
	sort.Strings(keys)
	return keys
}

// Array is a lightweight handle to one RGA array object.
type Array struct {
	document *Document
	id       ObjectID
}

func (a *Array) ID() ObjectID {
	if a == nil {
		return ObjectID{}
	}
	return a.id
}

func (a *Array) Len() int {
	if a == nil || a.document == nil {
		return 0
	}
	a.document.mu.RLock()
	values := a.document.visibleArrayNodesLocked(a.id)
	a.document.mu.RUnlock()
	return len(values)
}

func (a *Array) Get(index int) (Value, bool) {
	if a == nil || a.document == nil || index < 0 {
		return Value{}, false
	}
	a.document.mu.RLock()
	nodes := a.document.visibleArrayNodesLocked(a.id)
	if index >= len(nodes) {
		a.document.mu.RUnlock()
		return Value{}, false
	}
	value := cloneValue(nodes[index].value)
	a.document.mu.RUnlock()
	return value, true
}

func (a *Array) Insert(index int, value []byte) (Delta, error) {
	if a == nil || a.document == nil {
		return Delta{}, ErrNilDocument
	}
	return a.document.insertArrayValue(a.id, index, Bytes(value), Kind(0))
}

func (a *Array) InsertMap(index int) (*Map, Delta, error) {
	if a == nil || a.document == nil {
		return nil, Delta{}, ErrNilDocument
	}
	return a.document.insertArrayChild(a.id, index, KindMap)
}

func (a *Array) InsertArray(index int) (*Array, Delta, error) {
	if a == nil || a.document == nil {
		return nil, Delta{}, ErrNilDocument
	}
	child, delta, err := a.document.insertArrayChild(a.id, index, KindArray)
	if err != nil {
		return nil, delta, err
	}
	return &Array{document: a.document, id: child.id}, delta, nil
}

func (a *Array) Delete(index, count int) (Delta, error) {
	if a == nil || a.document == nil {
		return Delta{}, ErrNilDocument
	}
	if index < 0 || count < 0 {
		return Delta{}, ErrRange
	}
	a.document.mu.Lock()
	defer a.document.mu.Unlock()
	if !a.document.isObjectKindLocked(a.id, KindArray) {
		return Delta{}, ErrUnknownObject
	}
	visible := a.document.visibleArrayNodesLocked(a.id)
	if index > len(visible) || count > len(visible)-index {
		return Delta{}, ErrRange
	}
	state := newDocumentState()
	if count > 0 {
		state.tombstones[a.id] = make(map[ObjectID]struct{}, count)
		for _, node := range visible[index : index+count] {
			state.tombstones[a.id][node.id] = struct{}{}
		}
	}
	delta := Delta{state: state}
	if err := a.document.applyLocalLocked(&delta, a.document.clock); err != nil {
		return Delta{}, err
	}
	return delta, nil
}

func (a *Array) Map(index int) (*Map, bool) {
	value, ok := a.Get(index)
	if !ok || value.Kind != ValueObject || value.Object.Kind != KindMap {
		return nil, false
	}
	return a.document.Map(value.Object.ID)
}

func (a *Array) Array(index int) (*Array, bool) {
	value, ok := a.Get(index)
	if !ok || value.Kind != ValueObject || value.Object.Kind != KindArray {
		return nil, false
	}
	return a.document.Array(value.Object.ID)
}

func (d *Document) setMapValue(target ObjectID, key string, value Value) (Delta, error) {
	if d == nil {
		return Delta{}, ErrNilDocument
	}
	if !d.validName(key, d.options.MaxKeyBytes) {
		return Delta{}, ErrInvalidKey
	}
	if err := d.validateValue(value); err != nil {
		return Delta{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.clock == nil {
		return Delta{}, ErrNilDocument
	}
	if !d.isObjectKindLocked(target, KindMap) {
		return Delta{}, ErrUnknownObject
	}
	entries := d.state.maps[target]
	if _, exists := entries[key]; !exists && countMapEntries(d.state) >= d.options.MaxMapEntries {
		return Delta{}, ErrResourceLimit
	}
	tag, nextClock, err := d.nextLocalTagLocked()
	if err != nil {
		return Delta{}, err
	}
	state := newDocumentState()
	state.maps[target] = map[string]mapEntry{key: {tag: tag, present: true, value: cloneValue(value)}}
	delta := Delta{state: state}
	if err := d.applyLocalLocked(&delta, nextClock); err != nil {
		return Delta{}, err
	}
	return delta, nil
}

func (d *Document) deleteMapValue(target ObjectID, key string) (Delta, error) {
	if d == nil {
		return Delta{}, ErrNilDocument
	}
	if !d.validName(key, d.options.MaxKeyBytes) {
		return Delta{}, ErrInvalidKey
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.clock == nil {
		return Delta{}, ErrNilDocument
	}
	if !d.isObjectKindLocked(target, KindMap) {
		return Delta{}, ErrUnknownObject
	}
	entries := d.state.maps[target]
	if _, exists := entries[key]; !exists && countMapEntries(d.state) >= d.options.MaxMapEntries {
		return Delta{}, ErrResourceLimit
	}
	tag, nextClock, err := d.nextLocalTagLocked()
	if err != nil {
		return Delta{}, err
	}
	state := newDocumentState()
	state.maps[target] = map[string]mapEntry{key: {tag: tag}}
	delta := Delta{state: state}
	if err := d.applyLocalLocked(&delta, nextClock); err != nil {
		return Delta{}, err
	}
	return delta, nil
}

func (d *Document) createMapChild(target ObjectID, key string, kind Kind) (*Map, Delta, error) {
	if d == nil {
		return nil, Delta{}, ErrNilDocument
	}
	if !d.validName(key, d.options.MaxKeyBytes) {
		return nil, Delta{}, ErrInvalidKey
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.clock == nil {
		return nil, Delta{}, ErrNilDocument
	}
	if !d.isObjectKindLocked(target, KindMap) {
		return nil, Delta{}, ErrUnknownObject
	}
	if len(d.state.objects) >= d.options.MaxObjects {
		return nil, Delta{}, ErrResourceLimit
	}
	if d.childDepthLocked(target) > d.options.MaxDepth {
		return nil, Delta{}, ErrResourceLimit
	}
	entries := d.state.maps[target]
	if _, exists := entries[key]; !exists && countMapEntries(d.state) >= d.options.MaxMapEntries {
		return nil, Delta{}, ErrResourceLimit
	}
	id, nextClock, err := d.nextLocalTagLocked()
	if err != nil {
		return nil, Delta{}, err
	}
	state := newDocumentState()
	state.objects[id] = objectDecl{id: id, kind: kind, owner: objectOwner{kind: ownerMap, parent: target, key: key}}
	state.maps[target] = map[string]mapEntry{key: {tag: id, present: true, value: Value{Kind: ValueObject, Object: ObjectRef{ID: id, Kind: kind}}}}
	delta := Delta{state: state}
	if err := d.applyLocalLocked(&delta, nextClock); err != nil {
		return nil, Delta{}, err
	}
	return &Map{document: d, id: id}, delta, nil
}

func (d *Document) insertArrayValue(target ObjectID, index int, value Value, childKind Kind) (Delta, error) {
	if d == nil {
		return Delta{}, ErrNilDocument
	}
	if childKind != 0 && !childKind.valid() {
		return Delta{}, ErrInvalidValue
	}
	if childKind == 0 {
		if err := d.validateValue(value); err != nil {
			return Delta{}, err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.clock == nil {
		return Delta{}, ErrNilDocument
	}
	if !d.isObjectKindLocked(target, KindArray) {
		return Delta{}, ErrUnknownObject
	}
	visible := d.visibleArrayNodesLocked(target)
	if index < 0 || index > len(visible) {
		return Delta{}, ErrRange
	}
	if countArrayNodes(d.state) >= d.options.MaxArrayNodes || (childKind != 0 && len(d.state.objects) >= d.options.MaxObjects) {
		return Delta{}, ErrResourceLimit
	}
	if childKind != 0 && d.childDepthLocked(target) > d.options.MaxDepth {
		return Delta{}, ErrResourceLimit
	}
	id, nextClock, err := d.nextLocalTagLocked()
	if err != nil {
		return Delta{}, err
	}
	parent := ObjectID{}
	if index > 0 {
		parent = visible[index-1].id
	}
	state := newDocumentState()
	if childKind != 0 {
		value = Value{Kind: ValueObject, Object: ObjectRef{ID: id, Kind: childKind}}
		state.objects[id] = objectDecl{id: id, kind: childKind, owner: objectOwner{kind: ownerArray, parent: target, position: id}}
	}
	state.arrays[target] = map[ObjectID]arrayNode{id: {id: id, parent: parent, value: cloneValue(value)}}
	delta := Delta{state: state}
	if err := d.applyLocalLocked(&delta, nextClock); err != nil {
		return Delta{}, err
	}
	return delta, nil
}

func (d *Document) insertArrayChild(target ObjectID, index int, kind Kind) (*Map, Delta, error) {
	delta, err := d.insertArrayValue(target, index, Value{}, kind)
	if err != nil {
		return nil, Delta{}, err
	}
	for id := range delta.state.objects {
		return &Map{document: d, id: id}, delta, nil
	}
	return nil, Delta{}, ErrInvalidDelta
}

func (d *Document) isObjectKindLocked(id ObjectID, kind Kind) bool {
	object, ok := d.state.objects[id]
	return ok && object.kind == kind
}

// childDepthLocked calculates the depth a newly owned child would have. Its
// parent is already integrated for every local creation, so this is a pure
// preflight before advancing the HLC or touching retained state.
func (d *Document) childDepthLocked(parent ObjectID) int {
	depth := 0
	for current := parent; current.Valid(); {
		object, exists := d.state.objects[current]
		if !exists || object.owner.kind == ownerRoot {
			return depth + 1
		}
		depth++
		current = object.owner.parent
	}
	return depth + 1
}

func (d *Document) visibleArrayNodesLocked(target ObjectID) []arrayNode {
	nodes := d.state.arrays[target]
	if len(nodes) == 0 {
		return nil
	}
	children := make(map[ObjectID][]arrayNode, len(nodes))
	for _, node := range nodes {
		children[node.parent] = append(children[node.parent], node)
	}
	for parent := range children {
		sort.Slice(children[parent], func(i, j int) bool { return children[parent][i].id.Compare(children[parent][j].id) > 0 })
	}
	result := make([]arrayNode, 0, len(nodes))
	var visit func(ObjectID)
	visit = func(parent ObjectID) {
		for _, node := range children[parent] {
			if _, deleted := d.state.tombstones[target][node.id]; !deleted {
				result = append(result, node)
			}
			visit(node.id)
		}
	}
	visit(ObjectID{})
	return result
}

func (d *Document) validName(value string, max int) bool {
	if max <= 0 || !utf8.ValidString(value) || len(value) == 0 || len(value) > max || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func (d *Document) validateValue(value Value) error {
	switch value.Kind {
	case ValueBytes:
		if len(value.Bytes) > d.options.MaxValueBytes {
			return ErrResourceLimit
		}
	case ValueObject:
		return ErrInvalidValue // Public mutation methods create child objects atomically.
	default:
		return ErrInvalidValue
	}
	return nil
}

func countMapEntries(state documentState) int {
	count := 0
	for _, entries := range state.maps {
		count += len(entries)
	}
	return count
}

func countArrayNodes(state documentState) int {
	count := 0
	for _, nodes := range state.arrays {
		count += len(nodes)
	}
	return count
}

func countTombstones(state documentState) int {
	count := 0
	for _, tombstones := range state.tombstones {
		count += len(tombstones)
	}
	return count
}

func rootExists(roots map[string]rootRecord, name string) bool {
	_, exists := roots[name]
	return exists
}
