package text

import (
	"errors"
	"sync"
	"unicode/utf8"
)

var (
	// ErrNoUndo reports that no captured local operation can be undone.
	ErrNoUndo = errHistoryEmpty("text: no undo operation")
	// ErrNoRedo reports that no previously undone local operation can be redone.
	ErrNoRedo = errHistoryEmpty("text: no redo operation")
	// ErrInvalidUndoOptions reports an unusable local undo-history policy.
	ErrInvalidUndoOptions = errors.New("text: invalid undo options")
	// ErrUndoHistoryLimit reports that one local edit cannot fit in the
	// configured history budget. It leaves both the RGA and history unchanged.
	ErrUndoHistoryLimit = errors.New("text: undo history resource limit")
)

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
// closed with ErrUndoAnchorGone. The local stack is bounded: when a successful
// edit would exceed its total retained budget, the manager discards its
// complete local history and records that newest edit as the next undo step.
// An individual edit larger than the configured rune budget is rejected before
// it changes the RGA.
type UndoManager struct {
	mu        sync.Mutex
	value     *RGA
	options   UndoOptions
	undo      []*undoEntry
	redo      []*undoEntry
	undoRunes int
	redoRunes int
	owners    map[Position]positionOwner
}

// UndoOptions bounds one process-local text undo/redo stack. MaxRunes counts
// Unicode scalar values retained by both stacks, rather than UTF-8 bytes, so
// it matches text RGA offsets and position ownership.
type UndoOptions struct {
	MaxEntries int
	MaxRunes   int
}

// DefaultUndoOptions returns conservative interactive-text history limits.
// They bound local metadata only and do not replace RGA, frame, outbox, or
// transport limits.
func DefaultUndoOptions() UndoOptions {
	return UndoOptions{
		MaxEntries: 256,
		MaxRunes:   1 << 20,
	}
}

func (o UndoOptions) valid() bool {
	return o.MaxEntries > 0 && o.MaxRunes > 0
}

// NewUndoManager creates a local history for value with the default bounded
// interactive-text policy.
func NewUndoManager(value *RGA) (*UndoManager, error) {
	return NewUndoManagerWithOptions(value, DefaultUndoOptions())
}

// NewUndoManagerWithOptions creates a local history for value with explicit
// local-only retention limits. It neither changes RGA wire semantics nor
// serializes history for replication.
func NewUndoManagerWithOptions(value *RGA, options UndoOptions) (*UndoManager, error) {
	if value == nil || value.clock == nil {
		return nil, ErrNilText
	}
	if !options.valid() {
		return nil, ErrInvalidUndoOptions
	}
	return &UndoManager{value: value, options: options, owners: make(map[Position]positionOwner)}, nil
}

// Insert applies one local insertion and captures it as one undo step.
func (m *UndoManager) Insert(offset int, value string) (Delta, error) {
	if m == nil || m.value == nil {
		return Delta{}, ErrNilText
	}
	if !utf8.ValidString(value) {
		return Delta{}, ErrInvalidText
	}
	runes := utf8.RuneCountInString(value)
	if runes > m.options.MaxRunes {
		return Delta{}, ErrUndoHistoryLimit
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delta, predecessor, err := m.value.insertWithPredecessor(offset, value)
	if err != nil || len(delta.nodes) == 0 {
		return delta, err
	}
	entry := &undoEntry{kind: undoInsertion, predecessor: predecessor, value: value, positions: sortedDeltaPositions(delta), runes: runes}
	m.recordNewEntry(entry)
	m.registerOwnedPositions(entry)
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
	delta, predecessor, value, positions, err := m.value.deleteWithUndoCapture(offset, count, m.options.MaxRunes)
	if err != nil || count == 0 {
		return delta, err
	}
	entry := &undoEntry{kind: undoDeletion, predecessor: predecessor, value: value, targets: make([]undoTarget, len(positions)), positions: make([]Position, len(positions)), runes: len(positions)}
	for index, position := range positions {
		if owner, ok := m.owners[position]; ok {
			entry.targets[index] = undoTarget{owner: owner.entry, index: owner.index}
			continue
		}
		entry.targets[index] = undoTarget{external: position}
	}
	m.recordNewEntry(entry)
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
	m.undoRunes -= entry.runes
	m.redo = append(m.redo, entry)
	m.redoRunes += entry.runes
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
	m.redoRunes -= entry.runes
	m.undo = append(m.undo, entry)
	m.undoRunes += entry.runes
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

// Len returns the total number of retained local undo and redo entries.
func (m *UndoManager) Len() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.undo) + len(m.redo)
}

// Clear discards local history. It does not mutate the shared RGA state.
func (m *UndoManager) Clear() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.clearLocked()
	m.mu.Unlock()
}

type undoEntry struct {
	kind        undoKind
	predecessor Position
	value       string
	positions   []Position
	targets     []undoTarget
	runes       int
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
	return m.value.tombstoneRetainedPositions(positions)
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

// recordNewEntry installs one successful local edit. Redo is always local
// history, so a new edit discards it. If the remaining undo stack plus this
// entry exceeds either local cap, releasing the complete history is safer than
// evicting a single entry whose position ownership may still be referenced by
// a later deletion entry.
func (m *UndoManager) recordNewEntry(entry *undoEntry) {
	if len(m.undo) >= m.options.MaxEntries || m.undoRunes > m.options.MaxRunes-entry.runes {
		m.clearLocked()
	} else {
		m.discardRedoLocked()
	}
	m.undo = append(m.undo, entry)
	m.undoRunes += entry.runes
}

func (m *UndoManager) discardRedoLocked() {
	for _, entry := range m.redo {
		if entry.kind == undoInsertion {
			m.unregisterOwnedPositions(entry)
		}
	}
	m.redo = nil
	m.redoRunes = 0
}

func (m *UndoManager) clearLocked() {
	m.undo = nil
	m.redo = nil
	m.undoRunes = 0
	m.redoRunes = 0
	m.owners = make(map[Position]positionOwner)
}

func sortedDeltaPositions(delta Delta) []Position {
	positions := make([]Position, 0, len(delta.nodes))
	for position := range delta.nodes {
		positions = append(positions, position)
	}
	sortPositions(positions)
	return positions
}

// deleteWithUndoCapture captures and applies the exact deletion delta in one
// local operation. It prevents a concurrent remote delta from changing the
// offsets between history capture and local mutation. maxRunes is checked
// before allocating the captured text and positions.
func (r *RGA) deleteWithUndoCapture(offset, count, maxRunes int) (Delta, Position, string, []Position, error) {
	if r == nil || r.clock == nil {
		return Delta{}, Position{}, "", nil, ErrNilText
	}
	if maxRunes <= 0 || count > maxRunes {
		return Delta{}, Position{}, "", nil, ErrUndoHistoryLimit
	}
	r.mu.RLock()
	if offset < 0 || count < 0 || offset > visibleCount(r.sequence.root) || count > visibleCount(r.sequence.root)-offset {
		r.mu.RUnlock()
		return Delta{}, Position{}, "", nil, ErrRange
	}
	predecessor, _ := r.sequence.visibleAt(offset - 1)
	runes := make([]rune, 0, count)
	positions := make([]Position, 0, count)
	for index := 0; index < count; index++ {
		position, ok := r.sequence.visibleAt(offset + index)
		if !ok {
			r.mu.RUnlock()
			return Delta{}, Position{}, "", nil, ErrRange
		}
		runes = append(runes, r.nodes[position].rune)
		positions = append(positions, position)
	}
	r.mu.RUnlock()
	delta := Delta{nodes: make(map[Position]node), tombstones: make(map[Position]struct{}, len(positions))}
	for _, position := range positions {
		delta.tombstones[position] = struct{}{}
	}
	if err := r.ApplyDelta(delta); err != nil {
		return Delta{}, Position{}, "", nil, err
	}
	return delta, predecessor, string(runes), positions, nil
}
