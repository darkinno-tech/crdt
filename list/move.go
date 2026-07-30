package list

import (
	"sort"
	"strings"
	"sync"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

// MoveRGA is a move-capable, generic sequence CRDT. It is intentionally a
// different protocol from RGA: existing List RGA frames retain their immutable
// insert/delete-only semantics.
//
// Element identities are immutable. A move writes a per-element placement
// register whose HLC operation ID provides last-writer-wins conflict resolution.
// Concurrent moves can create an attachment cycle; projection deterministically
// drops the lower-priority cycle-closing attachment instead of mutating shared
// state. Therefore replicas converge without pretending that every concurrent
// intent can be satisfied simultaneously.
type MoveRGA[T any] struct {
	mu        sync.RWMutex
	replicaID string
	clock     *clock.HLC
	codec     ElementCodec[T]
	codecID   string
	options   Options

	nodes      map[Position]node
	tombstones map[Position]struct{}
	moves      map[Position]moveRecord
	version    uint64
}

type moveRecord struct {
	tag    Position
	anchor Position
	rank   uint64
}

// MoveDelta is an opaque, joinable partial MoveRGA state. Local operations and
// the bounded decoder are its only constructors.
type MoveDelta struct {
	codecID    string
	nodes      map[Position]node
	tombstones map[Position]struct{}
	moves      map[Position]moveRecord
}

var _ crdt.CRDT[*MoveRGA[any]] = (*MoveRGA[any])(nil)
var _ crdt.DeltaCapable[*MoveRGA[any], MoveDelta] = (*MoveRGA[any])(nil)

// NewMoveRGA constructs a move-capable sequence using conservative defaults.
func NewMoveRGA[T any](replicaID string, codec ElementCodec[T]) (*MoveRGA[T], error) {
	return NewMoveRGAWithOptions(replicaID, codec, DefaultOptions())
}

// NewMoveRGAWithOptions constructs a move-capable sequence with explicit
// retained-state limits.
func NewMoveRGAWithOptions[T any](replicaID string, codec ElementCodec[T], options Options) (*MoveRGA[T], error) {
	return NewMoveRGAFromClockWithOptions(clock.State{ReplicaID: replicaID}, codec, options)
}

// NewMoveRGAFromClockWithOptions restores a replica clock that was persisted
// atomically with a MoveRGA snapshot.
func NewMoveRGAFromClockWithOptions[T any](state clock.State, codec ElementCodec[T], options Options) (*MoveRGA[T], error) {
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
	return &MoveRGA[T]{
		replicaID:  state.ReplicaID,
		clock:      hlc,
		codec:      codec,
		codecID:    codecID,
		options:    options,
		nodes:      make(map[Position]node),
		tombstones: make(map[Position]struct{}),
		moves:      make(map[Position]moveRecord),
	}, nil
}

// ClockState returns the HLC state that must be persisted with a snapshot.
func (r *MoveRGA[T]) ClockState() clock.State {
	if r == nil || r.clock == nil {
		return clock.State{}
	}
	return r.clock.Snapshot()
}

// Insert inserts values before offset. Each value receives a permanent
// identity; later Move calls relocate that identity rather than cloning it.
func (r *MoveRGA[T]) Insert(offset int, values []T) (MoveDelta, error) {
	if r == nil || r.clock == nil {
		return MoveDelta{}, ErrNilList
	}
	if offset < 0 {
		return MoveDelta{}, ErrRange
	}
	r.mu.RLock()
	positions := r.visiblePositionsLocked()
	if offset > len(positions) {
		r.mu.RUnlock()
		return MoveDelta{}, ErrRange
	}
	anchor := Position{}
	if offset > 0 {
		anchor = positions[offset-1]
	}
	r.mu.RUnlock()
	encoded := make([][]byte, len(values))
	for index, value := range values {
		canonical, err := r.canonical(value)
		if err != nil {
			return MoveDelta{}, err
		}
		encoded[index] = canonical
	}
	delta := MoveDelta{codecID: r.codecID, nodes: make(map[Position]node, len(encoded)), tombstones: map[Position]struct{}{}, moves: map[Position]moveRecord{}}
	for _, value := range encoded {
		id, err := r.clock.Now()
		if err != nil {
			return MoveDelta{}, err
		}
		delta.nodes[id] = node{parent: anchor, value: value}
		anchor = id
	}
	if len(delta.nodes) == 0 {
		return delta, nil
	}
	if err := r.ApplyDelta(delta); err != nil {
		return MoveDelta{}, err
	}
	return delta, nil
}

// Append appends values to the current visible tail.
func (r *MoveRGA[T]) Append(values []T) (MoveDelta, error) {
	if r == nil {
		return MoveDelta{}, ErrNilList
	}
	return r.Insert(r.State().ElementCount, values)
}

// Delete tombstones count visible elements starting at offset. Tombstones retain
// their placement identity until the same application-level checkpoint and
// acknowledgement lifecycle used by other structural sequences permits GC.
func (r *MoveRGA[T]) Delete(offset, count int) (MoveDelta, error) {
	if r == nil {
		return MoveDelta{}, ErrNilList
	}
	if offset < 0 || count < 0 {
		return MoveDelta{}, ErrRange
	}
	r.mu.RLock()
	positions := r.visiblePositionsLocked()
	r.mu.RUnlock()
	if offset > len(positions) || count > len(positions)-offset {
		return MoveDelta{}, ErrRange
	}
	delta := MoveDelta{codecID: r.codecID, nodes: map[Position]node{}, tombstones: make(map[Position]struct{}, count), moves: map[Position]moveRecord{}}
	for _, position := range positions[offset : offset+count] {
		delta.tombstones[position] = struct{}{}
	}
	if err := r.ApplyDelta(delta); err != nil {
		return MoveDelta{}, err
	}
	return delta, nil
}

// Move relocates count elements beginning at from. to is an offset in the
// visible list after the selected range has been removed, so Move(2, 1, 0)
// moves the third item to the front. A single operation tag and rank preserve
// the moved block's order under concurrent delivery.
func (r *MoveRGA[T]) Move(from, count, to int) (MoveDelta, error) {
	if r == nil || r.clock == nil {
		return MoveDelta{}, ErrNilList
	}
	if from < 0 || count < 0 || to < 0 {
		return MoveDelta{}, ErrRange
	}
	r.mu.RLock()
	positions := r.visiblePositionsLocked()
	r.mu.RUnlock()
	if from > len(positions) || count > len(positions)-from || to > len(positions)-count {
		return MoveDelta{}, ErrRange
	}
	if count == 0 {
		return MoveDelta{codecID: r.codecID, nodes: map[Position]node{}, tombstones: map[Position]struct{}{}, moves: map[Position]moveRecord{}}, nil
	}
	selected := append([]Position(nil), positions[from:from+count]...)
	remaining := make([]Position, 0, len(positions)-count)
	remaining = append(remaining, positions[:from]...)
	remaining = append(remaining, positions[from+count:]...)
	anchor := Position{}
	if to > 0 {
		anchor = remaining[to-1]
	}
	tag, err := r.clock.Now()
	if err != nil {
		return MoveDelta{}, err
	}
	delta := MoveDelta{codecID: r.codecID, nodes: map[Position]node{}, tombstones: map[Position]struct{}{}, moves: make(map[Position]moveRecord, len(selected))}
	for rank, position := range selected {
		delta.moves[position] = moveRecord{tag: tag, anchor: anchor, rank: uint64(rank)}
		anchor = position
	}
	if err := r.ApplyDelta(delta); err != nil {
		return MoveDelta{}, err
	}
	return delta, nil
}

// Values returns a fresh decoded visible projection.
func (r *MoveRGA[T]) Values() ([]T, error) {
	if r == nil {
		return nil, ErrNilList
	}
	r.mu.RLock()
	positions := r.visiblePositionsLocked()
	encoded := make([][]byte, len(positions))
	for index, position := range positions {
		encoded[index] = append([]byte(nil), r.nodes[position].value...)
	}
	r.mu.RUnlock()
	values := make([]T, len(encoded))
	for index, value := range encoded {
		decoded, err := r.codec.Unmarshal(value)
		if err != nil {
			return nil, ErrInvalidCodec
		}
		values[index] = decoded
	}
	return values, nil
}

// Positions returns permanent identities in current visible order.
func (r *MoveRGA[T]) Positions() []Position {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Position(nil), r.visiblePositionsLocked()...)
}

// ApplyDelta atomically joins a bounded, validated MoveRGA delta.
func (r *MoveRGA[T]) ApplyDelta(delta MoveDelta) error {
	if r == nil || r.clock == nil {
		return ErrNilList
	}
	if delta.codecID != r.codecID || validateMoveDelta(delta) != nil {
		return ErrInvalidDelta
	}
	if err := r.validateMoveValues(delta); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, incoming := range delta.nodes {
		if current, exists := r.nodes[id]; exists && !sameNode(current, incoming) {
			return ErrTagConflict
		}
	}
	for id, incoming := range delta.moves {
		if current, exists := r.moves[id]; exists && current.tag == incoming.tag && current != incoming {
			return ErrTagConflict
		}
	}
	newNodes := len(r.nodes)
	for id := range delta.nodes {
		if _, exists := r.nodes[id]; !exists {
			newNodes++
		}
	}
	newTombstones := len(r.tombstones)
	for id := range delta.tombstones {
		if _, exists := r.tombstones[id]; !exists {
			newTombstones++
		}
	}
	newMoves := make(map[Position]moveRecord, len(r.moves)+len(delta.moves))
	for id, record := range r.moves {
		newMoves[id] = record
	}
	for id, incoming := range delta.moves {
		if current, exists := newMoves[id]; !exists || current.tag.Compare(incoming.tag) < 0 {
			newMoves[id] = incoming
		}
	}
	pendingMoves := 0
	for id := range newMoves {
		if _, exists := r.nodes[id]; !exists {
			if _, added := delta.nodes[id]; !added {
				pendingMoves++
			}
		}
	}
	if newNodes > r.options.MaxNodes || newTombstones > r.options.MaxTombstones || pendingMoves > r.options.MaxPendingNodes {
		return ErrResourceLimit
	}
	if greatest, ok := greatestMoveTag(delta); ok {
		if err := r.clock.Witness(greatest); err != nil {
			return err
		}
	}
	for id, item := range delta.nodes {
		r.nodes[id] = item
	}
	for id := range delta.tombstones {
		r.tombstones[id] = struct{}{}
	}
	r.moves = newMoves
	r.version++
	return nil
}

// Merge joins the complete state of other. Both lists must use the same codec.
func (r *MoveRGA[T]) Merge(other *MoveRGA[T]) error {
	if r == nil || other == nil || r.codecID != other.codecID {
		return ErrInvalidDelta
	}
	other.mu.RLock()
	delta := MoveDelta{codecID: other.codecID, nodes: cloneNodes(other.nodes), tombstones: cloneTombstones(other.tombstones), moves: cloneMoves(other.moves)}
	other.mu.RUnlock()
	return r.ApplyDelta(delta)
}

// State reports immutable diagnostic metadata only.
func (r *MoveRGA[T]) State() crdt.StateSnapshot {
	if r == nil {
		return crdt.StateSnapshot{Type: "move-rga"}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return crdt.StateSnapshot{Type: "move-rga", ReplicaID: r.replicaID, ElementCount: len(r.visiblePositionsLocked()), TombstoneCount: len(r.tombstones)}
}

func (r *MoveRGA[T]) canonical(value T) ([]byte, error) {
	encoded, err := r.codec.Marshal(value)
	if err != nil || len(encoded) > r.options.MaxValueBytes {
		return nil, ErrInvalidCodec
	}
	decoded, err := r.codec.Unmarshal(encoded)
	if err != nil {
		return nil, ErrInvalidCodec
	}
	canonical, err := r.codec.Marshal(decoded)
	if err != nil || string(canonical) != string(encoded) {
		return nil, ErrInvalidCodec
	}
	return append([]byte(nil), encoded...), nil
}

func (r *MoveRGA[T]) validateMoveValues(delta MoveDelta) error {
	for _, item := range delta.nodes {
		if len(item.value) > r.options.MaxValueBytes {
			return ErrResourceLimit
		}
		decoded, err := r.codec.Unmarshal(item.value)
		if err != nil {
			return ErrInvalidCodec
		}
		canonical, err := r.codec.Marshal(decoded)
		if err != nil || string(canonical) != string(item.value) {
			return ErrInvalidCodec
		}
	}
	return nil
}

func validateMoveDelta(delta MoveDelta) error {
	if strings.TrimSpace(delta.codecID) == "" {
		return ErrInvalidDelta
	}
	for id, item := range delta.nodes {
		if !id.Valid() || id == item.parent || (item.parent.Valid() == false && item.parent != (Position{})) {
			return ErrInvalidDelta
		}
	}
	for id := range delta.tombstones {
		if !id.Valid() {
			return ErrInvalidDelta
		}
	}
	for id, record := range delta.moves {
		if !id.Valid() || !record.tag.Valid() || id == record.anchor || (!record.anchor.Valid() && record.anchor != (Position{})) {
			return ErrInvalidDelta
		}
	}
	return nil
}

func cloneMoves(source map[Position]moveRecord) map[Position]moveRecord {
	result := make(map[Position]moveRecord, len(source))
	for id, record := range source {
		result[id] = record
	}
	return result
}

func greatestMoveTag(delta MoveDelta) (Position, bool) {
	var greatest Position
	found := false
	record := func(tag Position) {
		if !found || greatest.Compare(tag) < 0 {
			greatest, found = tag, true
		}
	}
	for id, item := range delta.nodes {
		record(id)
		if item.parent.Valid() {
			record(item.parent)
		}
	}
	for id := range delta.tombstones {
		record(id)
	}
	for _, recordMove := range delta.moves {
		record(recordMove.tag)
		if recordMove.anchor.Valid() {
			record(recordMove.anchor)
		}
	}
	return greatest, found
}

type movePlacement struct {
	anchor Position
	tag    Position
	rank   uint64
}

func (r *MoveRGA[T]) visiblePositionsLocked() []Position {
	placements := make(map[Position]movePlacement, len(r.nodes))
	for id, item := range r.nodes {
		placements[id] = movePlacement{anchor: item.parent, tag: id}
	}
	for id, record := range r.moves {
		base, exists := placements[id]
		if exists && base.tag.Compare(record.tag) < 0 {
			placements[id] = movePlacement{anchor: record.anchor, tag: record.tag, rank: record.rank}
		}
	}
	ids := make([]Position, 0, len(placements))
	for id := range placements {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool {
		leftPlacement, rightPlacement := placements[ids[left]], placements[ids[right]]
		if comparison := leftPlacement.tag.Compare(rightPlacement.tag); comparison != 0 {
			return comparison > 0
		}
		if leftPlacement.rank != rightPlacement.rank {
			return leftPlacement.rank < rightPlacement.rank
		}
		return ids[left].Compare(ids[right]) < 0
	})
	parents := make(map[Position]Position, len(ids))
	for _, id := range ids {
		anchor := placements[id].anchor
		if !anchor.Valid() || anchor == id {
			parents[id] = Position{}
			continue
		}
		if _, known := placements[anchor]; !known || moveWouldCycle(id, anchor, parents) {
			parents[id] = Position{}
			continue
		}
		parents[id] = anchor
	}
	children := make(map[Position][]Position, len(ids)+1)
	for _, id := range ids {
		parent := parents[id]
		children[parent] = append(children[parent], id)
	}
	for parent := range children {
		siblings := children[parent]
		sort.Slice(siblings, func(left, right int) bool {
			leftPlacement, rightPlacement := placements[siblings[left]], placements[siblings[right]]
			if comparison := leftPlacement.tag.Compare(rightPlacement.tag); comparison != 0 {
				return comparison > 0
			}
			if leftPlacement.rank != rightPlacement.rank {
				return leftPlacement.rank < rightPlacement.rank
			}
			return siblings[left].Compare(siblings[right]) > 0
		})
		children[parent] = siblings
	}
	result := make([]Position, 0, len(r.nodes)-len(r.tombstones))
	type visit struct{ id Position }
	stack := make([]visit, 0, len(ids))
	for index := len(children[Position{}]) - 1; index >= 0; index-- {
		stack = append(stack, visit{id: children[Position{}][index]})
	}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, deleted := r.tombstones[current.id]; !deleted {
			result = append(result, current.id)
		}
		childrenForCurrent := children[current.id]
		for index := len(childrenForCurrent) - 1; index >= 0; index-- {
			stack = append(stack, visit{id: childrenForCurrent[index]})
		}
	}
	return result
}

func moveWouldCycle(id, anchor Position, parents map[Position]Position) bool {
	for current := anchor; current.Valid(); {
		if current == id {
			return true
		}
		next, exists := parents[current]
		if !exists {
			return false
		}
		current = next
	}
	return false
}

// MarshalBinary returns a complete canonical MoveRGA state frame. A complete
// snapshot rejects missing node/move dependencies rather than silently losing
// an out-of-order operation.
func (r *MoveRGA[T]) MarshalBinary() ([]byte, error) {
	return r.MarshalBinaryWithLimits(frame.DefaultLimits())
}

func (r *MoveRGA[T]) MarshalBinaryWithLimits(limits frame.DecoderLimits) ([]byte, error) {
	if r == nil {
		return nil, ErrNilList
	}
	r.mu.RLock()
	delta := MoveDelta{codecID: r.codecID, nodes: cloneNodes(r.nodes), tombstones: cloneTombstones(r.tombstones), moves: cloneMoves(r.moves)}
	r.mu.RUnlock()
	return marshalMoveRGA(crdt.TypeIDMoveRGAState, delta, limits)
}

func (d MoveDelta) MarshalBinary() ([]byte, error) {
	return d.MarshalBinaryWithLimits(frame.DefaultLimits())
}

func (d MoveDelta) MarshalBinaryWithLimits(limits frame.DecoderLimits) ([]byte, error) {
	return marshalMoveRGA(crdt.TypeIDMoveRGADelta, d, limits)
}

func (r *MoveRGA[T]) MarshalBinaryWithClockState() ([]byte, clock.State, error) {
	encoded, err := r.MarshalBinary()
	if err != nil {
		return nil, clock.State{}, err
	}
	return encoded, r.ClockState(), nil
}

func (r *MoveRGA[T]) SnapshotCurrentState() (snapshot.Snapshot, error) {
	encoded, state, err := r.MarshalBinaryWithClockState()
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	r.mu.RLock()
	frontier := moveFrontier(r.nodes, r.tombstones, r.moves)
	r.mu.RUnlock()
	return snapshot.NewWithClockState(encoded, frontier, state)
}

func NewMoveRGAFromSnapshot[T any](saved snapshot.Snapshot, codec ElementCodec[T]) (*MoveRGA[T], error) {
	if saved.TypeID != crdt.TypeIDMoveRGAState {
		return nil, ErrInvalidDelta
	}
	state, ok := saved.ClockState()
	if !ok {
		return nil, ErrInvalidDelta
	}
	r, err := NewMoveRGAFromClockWithOptions(state, codec, DefaultOptions())
	if err != nil {
		return nil, err
	}
	if err := r.UnmarshalBinary(saved.Bytes()); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *MoveRGA[T]) UnmarshalBinary(data []byte) error {
	return r.UnmarshalBinaryWithLimits(data, frame.DefaultLimits())
}

func (r *MoveRGA[T]) UnmarshalBinaryWithLimits(data []byte, limits frame.DecoderLimits) error {
	if r == nil || r.clock == nil {
		return ErrNilList
	}
	delta, err := unmarshalMoveRGA(data, crdt.TypeIDMoveRGAState, r.codecID, limits, true)
	if err != nil {
		return err
	}
	if err := r.validateMoveValues(delta); err != nil {
		return err
	}
	if len(delta.nodes) > r.options.MaxNodes || len(delta.tombstones) > r.options.MaxTombstones {
		return ErrResourceLimit
	}
	if greatest, ok := greatestMoveTag(delta); ok {
		if err := r.clock.Witness(greatest); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.nodes, r.tombstones, r.moves = delta.nodes, delta.tombstones, delta.moves
	r.version++
	r.mu.Unlock()
	return nil
}
