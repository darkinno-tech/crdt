// Package list implements a generic, ordered Replicated Growable Array (RGA).
//
// Each position holds one canonical caller-coded value. Positions, rather than
// offsets, are replicated, so duplicate and out-of-order delivery converge.
package list

import (
	"bytes"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	"github.com/DarkInno/crdt/internal/codecguard"
)

var (
	ErrNilList          = errors.New("list: nil RGA")
	ErrInvalidReplica   = errors.New("list: invalid replica ID")
	ErrInvalidCodec     = errors.New("list: invalid element codec")
	ErrRange            = errors.New("list: range outside visible list")
	ErrInvalidDelta     = errors.New("list: invalid RGA delta")
	ErrTagConflict      = errors.New("list: conflicting node for one tag")
	ErrIncompleteState  = errors.New("list: incomplete RGA state")
	ErrResourceLimit    = errors.New("list: RGA resource limit exceeded")
	ErrUnsafeCompaction = errors.New("list: unsafe RGA tombstone compaction")
)

// SemanticsVersion is the immutable generic list RGA v1 contract. It must
// match the value negotiated in a replica manifest.
const SemanticsVersion uint64 = crdt.SemanticsVersionListRGA

// StableFrameType returns the stable generic list RGA v1 state/delta pair.
func StableFrameType() crdt.FrameType {
	return crdt.FrameType{StateID: crdt.TypeIDListRGAState, DeltaID: crdt.TypeIDListRGADelta, SemanticsVersion: SemanticsVersion, UsesHLC: true}
}

// Position is a stable, opaque list-element identity.
type Position = crdt.Tag

// ElementCodec defines one application value's canonical wire representation.
// Marshal must encode semantically equal values identically. The RGA verifies
// every locally created and remotely decoded value by a decode/re-encode round
// trip before it can enter replicated state.
type ElementCodec[T any] interface {
	ID() string
	Marshal(T) ([]byte, error)
	Unmarshal([]byte) (T, error)
}

type node struct {
	parent Position
	value  []byte
}

// Delta is an opaque, joinable partial list state. It is constructed by local
// mutations or bounded frame decoding only.
type Delta struct {
	codecID    string
	nodes      map[Position]node
	tombstones map[Position]struct{}
}

// Options bounds retained list state. Applications receiving untrusted peers
// should set a group-appropriate bound rather than rely on process memory.
type Options struct {
	MaxNodes        int
	MaxTombstones   int
	MaxPendingNodes int
	MaxPendingBytes int
	MaxValueBytes   int
}

// DefaultOptions returns conservative per-list retention limits.
func DefaultOptions() Options {
	return Options{
		MaxNodes:        1 << 20,
		MaxTombstones:   1 << 20,
		MaxPendingNodes: 1 << 16,
		MaxPendingBytes: 4 << 20,
		MaxValueBytes:   1 << 20,
	}
}

func (o Options) valid() bool {
	return o.MaxNodes > 0 && o.MaxTombstones > 0 && o.MaxPendingNodes > 0 && o.MaxPendingBytes > 0 && o.MaxValueBytes > 0
}

// RGA is a generic collaborative list. Deletion retains a structural
// tombstone: an element deleted before its insertion arrives remains hidden
// when that insertion is eventually delivered.
type RGA[T any] struct {
	mu        sync.RWMutex
	replicaID string
	clock     *clock.HLC
	codec     ElementCodec[T]
	codecID   string
	options   Options

	nodes           map[Position]node
	pending         map[Position]node
	waitingByParent map[Position]map[Position]struct{}
	pendingBytes    int
	tombstones      map[Position]struct{}
	version         uint64
	sequence        *sequenceIndex
	children        childIndex
}

var _ crdt.CRDT[*RGA[any]] = (*RGA[any])(nil)
var _ crdt.DeltaCapable[*RGA[any], Delta] = (*RGA[any])(nil)

// New constructs a list with default retention limits.
func New[T any](replicaID string, codec ElementCodec[T]) (*RGA[T], error) {
	return NewWithOptions(replicaID, codec, DefaultOptions())
}

// NewWithOptions constructs a list with explicit retained-state limits.
func NewWithOptions[T any](replicaID string, codec ElementCodec[T], options Options) (*RGA[T], error) {
	return NewFromClockWithOptions(clock.State{ReplicaID: replicaID}, codec, options)
}

// NewFromClock restores a replica clock with default list limits.
func NewFromClock[T any](state clock.State, codec ElementCodec[T]) (*RGA[T], error) {
	return NewFromClockWithOptions(state, codec, DefaultOptions())
}

// NewFromClockWithOptions restores a replica clock. Persist its state
// atomically with a complete list snapshot before reusing a replica ID.
func NewFromClockWithOptions[T any](state clock.State, codec ElementCodec[T], options Options) (*RGA[T], error) {
	if !(crdt.Tag{ReplicaID: state.ReplicaID}).Valid() {
		return nil, ErrInvalidReplica
	}
	codecID, err := codecIdentifier(codec)
	if err != nil {
		return nil, err
	}
	if !options.valid() {
		return nil, ErrResourceLimit
	}
	hlc, err := clock.NewHLCFromState(state)
	if err != nil {
		return nil, err
	}
	return &RGA[T]{
		replicaID:       state.ReplicaID,
		clock:           hlc,
		codec:           codec,
		codecID:         codecID,
		options:         options,
		nodes:           make(map[Position]node),
		pending:         make(map[Position]node),
		waitingByParent: make(map[Position]map[Position]struct{}),
		tombstones:      make(map[Position]struct{}),
		sequence:        newSequenceIndex(),
		children:        newChildIndex(),
	}, nil
}

func codecIdentifier[T any](codec ElementCodec[T]) (string, error) {
	if codec == nil {
		return "", ErrInvalidCodec
	}
	id, err := codecguard.ID(codec.ID)
	if err != nil {
		return "", ErrInvalidCodec
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", ErrInvalidCodec
	}
	return id, nil
}

// ClockState returns the HLC state that must be persisted with a snapshot.
func (r *RGA[T]) ClockState() clock.State {
	if r == nil || r.clock == nil {
		return clock.State{}
	}
	return r.clock.Snapshot()
}

// Insert inserts values before visible element offset. One value becomes one
// RGA position, even when its canonical encoding is large or multi-byte.
func (r *RGA[T]) Insert(offset int, values []T) (Delta, error) {
	if r == nil || r.clock == nil {
		return Delta{}, ErrNilList
	}
	if offset < 0 {
		return Delta{}, ErrRange
	}
	r.mu.Lock()
	if offset > visibleCount(r.sequence.root) {
		r.mu.Unlock()
		return Delta{}, ErrRange
	}
	parent := Position{}
	if offset > 0 {
		var ok bool
		parent, ok = r.sequence.visibleAt(offset - 1)
		if !ok {
			r.mu.Unlock()
			return Delta{}, ErrRange
		}
	}
	r.mu.Unlock()

	encoded := make([][]byte, len(values))
	for index, value := range values {
		canonical, err := r.canonical(value)
		if err != nil {
			return Delta{}, err
		}
		encoded[index] = canonical
	}
	delta := Delta{codecID: r.codecID, nodes: make(map[Position]node, len(encoded)), tombstones: make(map[Position]struct{})}
	for _, value := range encoded {
		id, err := r.clock.Now()
		if err != nil {
			return Delta{}, err
		}
		delta.nodes[id] = node{parent: parent, value: value}
		parent = id
	}
	if len(delta.nodes) == 0 {
		return delta, nil
	}
	if err := r.ApplyDelta(delta); err != nil {
		return Delta{}, err
	}
	return delta, nil
}

// Append adds values after the current visible tail.
func (r *RGA[T]) Append(values []T) (Delta, error) {
	if r == nil {
		return Delta{}, ErrNilList
	}
	return r.Insert(r.State().ElementCount, values)
}

// Delete marks count visible elements starting at offset as removed.
func (r *RGA[T]) Delete(offset, count int) (Delta, error) {
	if r == nil {
		return Delta{}, ErrNilList
	}
	if offset < 0 || count < 0 {
		return Delta{}, ErrRange
	}
	r.mu.Lock()
	positions := r.visiblePositionsLocked()
	if offset > len(positions) || count > len(positions)-offset {
		r.mu.Unlock()
		return Delta{}, ErrRange
	}
	delta := Delta{codecID: r.codecID, nodes: make(map[Position]node), tombstones: make(map[Position]struct{}, count)}
	for _, position := range positions[offset : offset+count] {
		delta.tombstones[position] = struct{}{}
	}
	r.mu.Unlock()
	if err := r.ApplyDelta(delta); err != nil {
		return Delta{}, err
	}
	return delta, nil
}

// Values returns a fresh decoded projection in visible order.
func (r *RGA[T]) Values() ([]T, error) {
	if r == nil {
		return nil, ErrNilList
	}
	r.mu.Lock()
	positions := append([]Position(nil), r.visiblePositionsLocked()...)
	encoded := make([][]byte, len(positions))
	for index, position := range positions {
		encoded[index] = append([]byte(nil), r.nodes[position].value...)
	}
	r.mu.Unlock()
	values := make([]T, len(encoded))
	for index, value := range encoded {
		decoded, err := unmarshalCodec(r.codec, value)
		if err != nil {
			return nil, ErrInvalidCodec
		}
		values[index] = decoded
	}
	return values, nil
}

// At returns one visible element by offset.
func (r *RGA[T]) At(offset int) (T, error) {
	var zero T
	if r == nil {
		return zero, ErrNilList
	}
	r.mu.Lock()
	positions := r.visiblePositionsLocked()
	if offset < 0 || offset >= len(positions) {
		r.mu.Unlock()
		return zero, ErrRange
	}
	encoded := append([]byte(nil), r.nodes[positions[offset]].value...)
	r.mu.Unlock()
	value, err := unmarshalCodec(r.codec, encoded)
	if err != nil {
		return zero, ErrInvalidCodec
	}
	return value, nil
}

// Positions returns a copy of visible stable IDs in list order.
func (r *RGA[T]) Positions() []Position {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Position(nil), r.visiblePositionsLocked()...)
}

// ApplyDelta atomically integrates a locally created or decoded delta.
func (r *RGA[T]) ApplyDelta(delta Delta) error {
	if r == nil || r.clock == nil {
		return ErrNilList
	}
	if delta.codecID != r.codecID || validateDelta(delta) != nil {
		return ErrInvalidDelta
	}
	if err := r.validateValues(delta); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, incoming := range delta.nodes {
		if current, exists := r.nodes[id]; exists && !sameNode(current, incoming) {
			return ErrTagConflict
		}
		if current, exists := r.pending[id]; exists && !sameNode(current, incoming) {
			return ErrTagConflict
		}
	}
	if r.subsumesLocked(delta) {
		return nil
	}
	newNodes := make(map[Position]node, len(delta.nodes))
	for id, item := range delta.nodes {
		if _, exists := r.nodes[id]; !exists {
			if _, pending := r.pending[id]; !pending {
				newNodes[id] = item
			}
		}
	}
	if !acyclicAgainst(newNodes, r.pending) {
		return ErrInvalidDelta
	}
	resolved, pending := r.classifyNewNodesLocked(newNodes)
	pendingByteCount := 0
	for id := range pending {
		pendingByteCount += nodeBytes(id, newNodes[id])
	}
	if len(r.nodes)+len(r.pending)+len(newNodes) > r.options.MaxNodes ||
		len(r.pending)+len(pending) > r.options.MaxPendingNodes ||
		r.pendingBytes+pendingByteCount > r.options.MaxPendingBytes ||
		len(r.tombstones)+newTombstones(delta.tombstones, r.tombstones) > r.options.MaxTombstones {
		return ErrResourceLimit
	}
	if greatest, ok := greatestTag(delta); ok {
		if err := r.clock.Witness(greatest); err != nil {
			return err
		}
	}
	for _, id := range resolved {
		item := newNodes[id]
		r.nodes[id] = item
		r.integrateNode(id, item)
	}
	for id := range pending {
		r.enqueuePending(id, newNodes[id])
	}
	changed := len(newNodes) > 0
	if r.integrateReadyLocked() {
		changed = true
	}
	for id := range delta.tombstones {
		if _, exists := r.tombstones[id]; !exists {
			r.tombstones[id] = struct{}{}
			r.sequence.setVisible(id, false)
			changed = true
		}
	}
	if changed {
		r.version++
	}
	return nil
}

// Merge joins every retained node and tombstone from other.
func (r *RGA[T]) Merge(other *RGA[T]) error {
	if r == nil || other == nil {
		return ErrNilList
	}
	if r == other {
		return nil
	}
	if r.codecID != other.codecID {
		return ErrInvalidCodec
	}
	other.mu.RLock()
	delta := Delta{codecID: other.codecID, nodes: cloneNodes(other.nodes), tombstones: cloneTombstones(other.tombstones)}
	for id, item := range other.pending {
		delta.nodes[id] = cloneNode(item)
	}
	other.mu.RUnlock()
	return r.ApplyDelta(delta)
}

// PendingCount returns unresolved out-of-order nodes. Any non-zero value
// prevents a complete state snapshot from being emitted.
func (r *RGA[T]) PendingCount() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.pending)
}

// MissingParents returns unique unresolved parent IDs in canonical order.
func (r *RGA[T]) MissingParents() []Position {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	parents := make([]Position, 0, len(r.waitingByParent))
	for parent := range r.waitingByParent {
		if parent.Valid() {
			parents = append(parents, parent)
		}
	}
	sortPositions(parents)
	return parents
}

// TombstoneTags returns every retained deletion identity in canonical order.
// It is an exact-acknowledgement input, never proof of safe collection.
func (r *RGA[T]) TombstoneTags() []Position {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	positions := make([]Position, 0, len(r.tombstones))
	for position := range r.tombstones {
		positions = append(positions, position)
	}
	sortPositions(positions)
	return positions
}

// CompactTombstones removes exactly requested tombstoned leaves. The caller
// must first obtain exact authenticated acknowledgements for one epoch, save a
// post-compaction checkpoint, and retire old deltas. Any retained child or
// unresolved dependent blocks the entire request.
func (r *RGA[T]) CompactTombstones(tags []Position) (int, error) {
	if r == nil {
		return 0, ErrNilList
	}
	for _, tag := range tags {
		if !tag.Valid() {
			return 0, ErrUnsafeCompaction
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	compact := make([]Position, 0, len(tags))
	seen := make(map[Position]struct{}, len(tags))
	for _, tag := range tags {
		if _, duplicate := seen[tag]; duplicate {
			continue
		}
		seen[tag] = struct{}{}
		if _, tombstoned := r.tombstones[tag]; !tombstoned {
			continue
		}
		if _, exists := r.nodes[tag]; !exists || len(r.waitingByParent[tag]) > 0 {
			return 0, ErrUnsafeCompaction
		}
		if r.children.count(r.sequence.pair(tag)) > 0 {
			return 0, ErrUnsafeCompaction
		}
		compact = append(compact, tag)
	}
	for _, tag := range compact {
		item := r.nodes[tag]
		parent := r.sequence.pair(item.parent)
		child := r.sequence.pair(tag)
		if parent == nil || child == nil || !r.sequence.removeLeaf(tag) || !r.children.remove(parent, child) {
			return 0, ErrUnsafeCompaction
		}
		delete(r.nodes, tag)
		delete(r.tombstones, tag)
	}
	if len(compact) > 0 {
		r.version++
	}
	return len(compact), nil
}

// State returns an immutable diagnostic summary that contains no values,
// positions, clocks, or frame bytes.
func (r *RGA[T]) State() crdt.StateSnapshot {
	if r == nil {
		return crdt.StateSnapshot{Type: "rga-list"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return crdt.StateSnapshot{Type: "rga-list", ReplicaID: r.replicaID, ElementCount: visibleCount(r.sequence.root), TombstoneCount: len(r.tombstones)}
}

func (r *RGA[T]) canonical(value T) ([]byte, error) {
	encoded, err := marshalCodec(r.codec, value)
	if err != nil || len(encoded) > r.options.MaxValueBytes {
		return nil, ErrInvalidCodec
	}
	decoded, err := unmarshalCodec(r.codec, encoded)
	if err != nil {
		return nil, ErrInvalidCodec
	}
	canonical, err := marshalCodec(r.codec, decoded)
	if err != nil || !bytes.Equal(encoded, canonical) {
		return nil, ErrInvalidCodec
	}
	return append([]byte(nil), encoded...), nil
}

func (r *RGA[T]) validateValues(delta Delta) error {
	for _, item := range delta.nodes {
		if len(item.value) > r.options.MaxValueBytes {
			return ErrResourceLimit
		}
		decoded, err := unmarshalCodec(r.codec, item.value)
		if err != nil {
			return ErrInvalidCodec
		}
		canonical, err := marshalCodec(r.codec, decoded)
		if err != nil || !bytes.Equal(item.value, canonical) {
			return ErrInvalidCodec
		}
	}
	return nil
}

func marshalCodec[T any](codec ElementCodec[T], value T) ([]byte, error) {
	return codecguard.Marshal(func() ([]byte, error) { return codec.Marshal(value) })
}

func unmarshalCodec[T any](codec ElementCodec[T], data []byte) (T, error) {
	return codecguard.Unmarshal(func() (T, error) { return codec.Unmarshal(data) })
}

func (r *RGA[T]) classifyNewNodesLocked(nodes map[Position]node) ([]Position, map[Position]struct{}) {
	children := make(map[Position][]Position, len(nodes))
	resolved := make([]Position, 0, len(nodes))
	for id, item := range nodes {
		children[item.parent] = append(children[item.parent], id)
		if !item.parent.Valid() {
			resolved = append(resolved, id)
			continue
		}
		if _, exists := r.nodes[item.parent]; exists {
			resolved = append(resolved, id)
		}
	}
	sortPositions(resolved)
	known := make(map[Position]struct{}, len(nodes))
	for index := 0; index < len(resolved); index++ {
		id := resolved[index]
		if _, seen := known[id]; seen {
			continue
		}
		known[id] = struct{}{}
		childrenForID := children[id]
		sortPositions(childrenForID)
		resolved = append(resolved, childrenForID...)
	}
	pending := make(map[Position]struct{}, len(nodes)-len(known))
	for id := range nodes {
		if _, ok := known[id]; !ok {
			pending[id] = struct{}{}
		}
	}
	return resolved, pending
}

func (r *RGA[T]) enqueuePending(id Position, item node) {
	r.pending[id] = item
	if r.waitingByParent[item.parent] == nil {
		r.waitingByParent[item.parent] = make(map[Position]struct{})
	}
	r.waitingByParent[item.parent][id] = struct{}{}
	r.pendingBytes += nodeBytes(id, item)
}

func (r *RGA[T]) integrateReadyLocked() bool {
	parents := make([]Position, 0, len(r.waitingByParent))
	for parent := range r.waitingByParent {
		if !parent.Valid() {
			parents = append(parents, parent)
			continue
		}
		if _, known := r.nodes[parent]; known {
			parents = append(parents, parent)
		}
	}
	sortPositions(parents)
	ready := make([]Position, 0)
	for _, parent := range parents {
		ready = append(ready, sortedWaiting(r.waitingByParent[parent])...)
	}
	changed := false
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		item, pending := r.pending[id]
		if !pending {
			continue
		}
		if item.parent.Valid() {
			if _, known := r.nodes[item.parent]; !known {
				continue
			}
		}
		delete(r.pending, id)
		r.pendingBytes -= nodeBytes(id, item)
		delete(r.waitingByParent[item.parent], id)
		if len(r.waitingByParent[item.parent]) == 0 {
			delete(r.waitingByParent, item.parent)
		}
		r.nodes[id] = item
		r.integrateNode(id, item)
		changed = true
		ready = append(ready, sortedWaiting(r.waitingByParent[id])...)
	}
	return changed
}

func (r *RGA[T]) visiblePositionsLocked() []Position {
	return r.sequence.visiblePositions()
}

func (r *RGA[T]) integrateNode(id Position, item node) {
	parent := r.sequence.pair(item.parent)
	if parent == nil {
		panic("list: integrating node without integrated parent")
	}
	_, deleted := r.tombstones[id]
	pair := newSequencePair(id, !deleted)
	previous, hasPrevious := r.children.insert(parent, pair)
	anchor := &parent.entry
	if hasPrevious {
		anchor = &previous.exit
	}
	r.sequence.insertPairAfter(anchor, pair)
}

func buildSequence[T any](nodes map[Position]node, tombstones map[Position]struct{}) (*sequenceIndex, childIndex, error) {
	value := &RGA[T]{nodes: nodes, tombstones: tombstones, sequence: newSequenceIndex(), children: newChildIndex()}
	byParent := make(map[Position][]Position, len(nodes))
	for id, item := range nodes {
		byParent[item.parent] = append(byParent[item.parent], id)
	}
	for parent := range byParent {
		sortPositions(byParent[parent])
	}
	ready := append([]Position(nil), byParent[Position{}]...)
	integrated := 0
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		item, exists := nodes[id]
		if !exists || value.sequence.pair(item.parent) == nil {
			return nil, childIndex{}, ErrInvalidDelta
		}
		value.integrateNode(id, item)
		integrated++
		ready = append(ready, byParent[id]...)
	}
	if integrated != len(nodes) {
		return nil, childIndex{}, ErrInvalidDelta
	}
	return value.sequence, value.children, nil
}

func (r *RGA[T]) subsumesLocked(delta Delta) bool {
	for id, incoming := range delta.nodes {
		if current, exists := r.nodes[id]; exists && sameNode(current, incoming) {
			continue
		}
		if current, exists := r.pending[id]; exists && sameNode(current, incoming) {
			continue
		}
		return false
	}
	for id := range delta.tombstones {
		if _, exists := r.tombstones[id]; !exists {
			return false
		}
	}
	return true
}

func validateDelta(delta Delta) error {
	if strings.TrimSpace(delta.codecID) == "" {
		return ErrInvalidDelta
	}
	for id, item := range delta.nodes {
		if !id.Valid() || id == item.parent || (item.parent != (Position{}) && !item.parent.Valid()) {
			return ErrInvalidDelta
		}
	}
	for id := range delta.tombstones {
		if !id.Valid() {
			return ErrInvalidDelta
		}
	}
	return nil
}

func acyclicAgainst(incoming, pending map[Position]node) bool {
	lookup := func(id Position) (node, bool) {
		if item, ok := incoming[id]; ok {
			return item, true
		}
		item, ok := pending[id]
		return item, ok
	}
	const (
		unseen uint8 = iota
		visiting
		complete
	)
	state := make(map[Position]uint8, len(incoming)+len(pending))
	for id := range incoming {
		if state[id] == complete {
			continue
		}
		path := make([]Position, 0)
		for current := id; current.Valid(); {
			switch state[current] {
			case visiting:
				return false
			case complete:
				current = Position{}
				continue
			}
			item, known := lookup(current)
			if !known {
				break
			}
			state[current] = visiting
			path = append(path, current)
			current = item.parent
		}
		for _, position := range path {
			state[position] = complete
		}
	}
	return true
}

func sortPositions(positions []Position) {
	sort.Slice(positions, func(left, right int) bool { return positions[left].Compare(positions[right]) < 0 })
}

func sortedWaiting(waiting map[Position]struct{}) []Position {
	positions := make([]Position, 0, len(waiting))
	for position := range waiting {
		positions = append(positions, position)
	}
	sortPositions(positions)
	return positions
}

func sameNode(left, right node) bool {
	return left.parent == right.parent && bytes.Equal(left.value, right.value)
}

func cloneNode(item node) node {
	return node{parent: item.parent, value: append([]byte(nil), item.value...)}
}

func cloneNodes(source map[Position]node) map[Position]node {
	cloned := make(map[Position]node, len(source))
	for id, item := range source {
		cloned[id] = cloneNode(item)
	}
	return cloned
}

func cloneTombstones(source map[Position]struct{}) map[Position]struct{} {
	cloned := make(map[Position]struct{}, len(source))
	for id := range source {
		cloned[id] = struct{}{}
	}
	return cloned
}

func nodeBytes(id Position, item node) int {
	return 96 + len(id.ReplicaID) + len(item.parent.ReplicaID) + len(item.value)
}

func newTombstones(incoming, existing map[Position]struct{}) int {
	count := 0
	for id := range incoming {
		if _, exists := existing[id]; !exists {
			count++
		}
	}
	return count
}

func greatestTag(delta Delta) (Position, bool) {
	var greatest Position
	found := false
	for id, item := range delta.nodes {
		if !found || greatest.Compare(id) < 0 {
			greatest, found = id, true
		}
		if item.parent.Valid() && greatest.Compare(item.parent) < 0 {
			greatest = item.parent
		}
	}
	for id := range delta.tombstones {
		if !found || greatest.Compare(id) < 0 {
			greatest, found = id, true
		}
	}
	return greatest, found
}
