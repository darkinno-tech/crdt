package text

import (
	"sync"
)

// ErrNoUndo reports that no captured local operation can be undone.
var ErrNoUndo = errHistoryEmpty("text: no undo operation")

// ErrNoRedo reports that no previously undone local operation can be redone.
var ErrNoRedo = errHistoryEmpty("text: no redo operation")

type errHistoryEmpty string

func (e errHistoryEmpty) Error() string { return string(e) }

// UndoManager records local RGA edits made through it. Undo and redo emit new
// monotonic RGA deltas; they never rewind shared state, remove tombstones, or
// erase remote edits. This makes a local history safe to replicate through an
// at-least-once, out-of-order transport.
//
// The manager intentionally observes no direct RGA mutations. Use its Insert
// and Delete methods for edits that should be undoable. A caller must Clear
// history before compacting old structural anchors; otherwise replay fails
// closed with ErrUndoAnchorGone.
type UndoManager struct {
	mu     sync.Mutex
	value  *RGA
	undo   []*undoEntry
	redo   []*undoEntry
	owners map[Position]positionOwner
}

// NewUndoManager creates a local history for value.
func NewUndoManager(value *RGA) (*UndoManager, error) {
	if value == nil || value.clock == nil {
		return nil, ErrNilText
	}
	return &UndoManager{value: value, owners: make(map[Position]positionOwner)}, nil
}

// Insert applies one local insertion and captures it as one undo step.
func (m *UndoManager) Insert(offset int, value string) (Delta, error) {
	if m == nil || m.value == nil {
		return Delta{}, ErrNilText
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	predecessor, err := m.value.predecessorAt(offset)
	if err != nil {
		return Delta{}, err
	}
	delta, err := m.value.Insert(offset, value)
	if err != nil || len(delta.nodes) == 0 {
		return delta, err
	}
	entry := &undoEntry{kind: undoInsertion, predecessor: predecessor, value: value, positions: sortedDeltaPositions(delta)}
	m.registerOwnedPositions(entry)
	m.undo = append(m.undo, entry)
	m.redo = nil
	return delta, nil
}

// Delete removes one visible range and captures a reinsertion intent as one
// undo step. Undo does not resurrect deleted positions: it inserts new
// positions after the original retained predecessor.
func (m *UndoManager) Delete(offset, count int) (Delta, error) {
	if m == nil || m.value == nil {
		return Delta{}, ErrNilText
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	predecessor, value, positions, err := m.value.visibleRange(offset, count)
	if err != nil {
		return Delta{}, err
	}
	delta, err := m.value.Delete(offset, count)
	if err != nil || count == 0 {
		return delta, err
	}
	entry := &undoEntry{kind: undoDeletion, predecessor: predecessor, value: value, targets: make([]undoTarget, len(positions)), positions: make([]Position, len(positions))}
	for index, position := range positions {
		if owner, ok := m.owners[position]; ok {
			entry.targets[index] = undoTarget{owner: owner.entry, index: owner.index}
			continue
		}
		entry.targets[index] = undoTarget{external: position}
	}
	m.undo = append(m.undo, entry)
	m.redo = nil
	return delta, nil
}

// Undo emits the inverse of the most recent captured local edit and moves it
// to the redo stack. A failed inverse leaves both stacks unchanged.
func (m *UndoManager) Undo() (Delta, error) {
	if m == nil || m.value == nil {
		return Delta{}, ErrNilText
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.undo) == 0 {
		return Delta{}, ErrNoUndo
	}
	entry := m.undo[len(m.undo)-1]
	delta, err := m.undoEntry(entry)
	if err != nil {
		return Delta{}, err
	}
	m.undo = m.undo[:len(m.undo)-1]
	m.redo = append(m.redo, entry)
	return delta, nil
}

// Redo reapplies the most recently undone local edit and moves it back to the
// undo stack. A failed replay leaves both stacks unchanged.
func (m *UndoManager) Redo() (Delta, error) {
	if m == nil || m.value == nil {
		return Delta{}, ErrNilText
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.redo) == 0 {
		return Delta{}, ErrNoRedo
	}
	entry := m.redo[len(m.redo)-1]
	delta, err := m.redoEntry(entry)
	if err != nil {
		return Delta{}, err
	}
	m.redo = m.redo[:len(m.redo)-1]
	m.undo = append(m.undo, entry)
	return delta, nil
}

// CanUndo reports whether a captured local edit is available for undo.
func (m *UndoManager) CanUndo() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.undo) > 0
}

// CanRedo reports whether a previously undone local edit is available for
// redo.
func (m *UndoManager) CanRedo() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.redo) > 0
}

// Clear discards local history. It does not mutate the shared RGA state.
func (m *UndoManager) Clear() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.undo = nil
	m.redo = nil
	m.owners = make(map[Position]positionOwner)
	m.mu.Unlock()
}

type undoEntry struct {
	kind        undoKind
	predecessor Position
	value       string
	positions   []Position
	targets     []undoTarget
}

type undoKind uint8

const (
	undoInsertion undoKind = iota
	undoDeletion
)

type undoTarget struct {
	owner    *undoEntry
	index    int
	external Position
}

type positionOwner struct {
	entry *undoEntry
	index int
}

func (m *UndoManager) undoEntry(entry *undoEntry) (Delta, error) {
	switch entry.kind {
	case undoInsertion:
		return m.tombstonePositions(entry.positions)
	case undoDeletion:
		delta, err := m.value.insertAfter(entry.predecessor, entry.value)
		if err != nil {
			return Delta{}, err
		}
		positions := sortedDeltaPositions(delta)
		if len(positions) != len(entry.targets) {
			return Delta{}, ErrInvalidDelta
		}
		for index, position := range positions {
			target := entry.targets[index]
			if target.owner == nil {
				entry.positions[index] = position
				continue
			}
			m.replaceOwnedPosition(target.owner, target.index, position)
		}
		return delta, nil
	default:
		return Delta{}, ErrInvalidDelta
	}
}

func (m *UndoManager) redoEntry(entry *undoEntry) (Delta, error) {
	switch entry.kind {
	case undoInsertion:
		delta, err := m.value.insertAfter(entry.predecessor, entry.value)
		if err != nil {
			return Delta{}, err
		}
		m.unregisterOwnedPositions(entry)
		entry.positions = sortedDeltaPositions(delta)
		m.registerOwnedPositions(entry)
		return delta, nil
	case undoDeletion:
		positions := make([]Position, len(entry.targets))
		for index, target := range entry.targets {
			if target.owner != nil {
				if target.index >= len(target.owner.positions) {
					return Delta{}, ErrInvalidDelta
				}
				positions[index] = target.owner.positions[target.index]
				continue
			}
			positions[index] = entry.positions[index]
		}
		return m.tombstonePositions(positions)
	default:
		return Delta{}, ErrInvalidDelta
	}
}

func (m *UndoManager) tombstonePositions(positions []Position) (Delta, error) {
	delta := Delta{nodes: make(map[Position]node), tombstones: make(map[Position]struct{}, len(positions))}
	for _, position := range positions {
		if position.Valid() {
			delta.tombstones[position] = struct{}{}
		}
	}
	if err := m.value.ApplyDelta(delta); err != nil {
		return Delta{}, err
	}
	return delta, nil
}

func (m *UndoManager) replaceOwnedPosition(entry *undoEntry, index int, position Position) {
	if index >= len(entry.positions) {
		return
	}
	delete(m.owners, entry.positions[index])
	entry.positions[index] = position
	m.owners[position] = positionOwner{entry: entry, index: index}
}

func (m *UndoManager) registerOwnedPositions(entry *undoEntry) {
	for index, position := range entry.positions {
		if position.Valid() {
			m.owners[position] = positionOwner{entry: entry, index: index}
		}
	}
}

func (m *UndoManager) unregisterOwnedPositions(entry *undoEntry) {
	for _, position := range entry.positions {
		delete(m.owners, position)
	}
}

func sortedDeltaPositions(delta Delta) []Position {
	positions := make([]Position, 0, len(delta.nodes))
	for position := range delta.nodes {
		positions = append(positions, position)
	}
	sortPositions(positions)
	return positions
}

func (r *RGA) predecessorAt(offset int) (Position, error) {
	if r == nil {
		return Position{}, ErrNilText
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if offset < 0 || offset > visibleCount(r.sequence.root) {
		return Position{}, ErrRange
	}
	if predecessor, ok := r.sequence.visibleAt(offset - 1); ok {
		return predecessor, nil
	}
	return Position{}, nil
}

func (r *RGA) visibleRange(offset, count int) (Position, string, []Position, error) {
	if r == nil {
		return Position{}, "", nil, ErrNilText
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if offset < 0 || count < 0 || offset > visibleCount(r.sequence.root) || count > visibleCount(r.sequence.root)-offset {
		return Position{}, "", nil, ErrRange
	}
	predecessor, _ := r.sequence.visibleAt(offset - 1)
	runes := make([]rune, 0, count)
	positions := make([]Position, 0, count)
	for index := 0; index < count; index++ {
		position, ok := r.sequence.visibleAt(offset + index)
		if !ok {
			return Position{}, "", nil, ErrRange
		}
		runes = append(runes, r.nodes[position].rune)
		positions = append(positions, position)
	}
	return predecessor, string(runes), positions, nil
}
