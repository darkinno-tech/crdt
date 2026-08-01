// Package history provides bounded, local undo/redo and version-history
// metadata for CRDT applications. It is deliberately outside the replicated
// CRDT frame protocols: undo emits new compensating operations through a host
// supplied executor, while version records reference complete local snapshots.
package history

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"sync"
	"unicode"
	"unicode/utf8"

	frame "github.com/DarkInno/crdt/encoding"
)

var (
	// ErrInvalidOptions reports an unusable local-history resource policy.
	ErrInvalidOptions = errors.New("history: invalid options")
	// ErrInvalidCommand reports an empty, oversized, or otherwise invalid
	// local command. Commands are opaque to this package after this check.
	ErrInvalidCommand = errors.New("history: invalid command")
	// ErrInvalidState reports a malformed, non-canonical, or over-limit local
	// history record. It is intentionally distinct from a CRDT frame error.
	ErrInvalidState = errors.New("history: invalid state")
	// ErrNoUndo reports that no local command is available for undo.
	ErrNoUndo = errors.New("history: no undo operation")
	// ErrNoRedo reports that no undone local command is available for redo.
	ErrNoRedo = errors.New("history: no redo operation")
	// ErrExecutor reports a nil or panicking command executor.
	ErrExecutor = errors.New("history: executor failure")
	// ErrResourceLimit reports a configured local history limit.
	ErrResourceLimit = errors.New("history: resource limit exceeded")
)

const (
	historyFormatVersion byte = 1
)

var historyMagic = [...]byte{'C', 'R', 'D', 'H'}

// Options bounds one process-local undo/redo stack. The defaults are intended
// for interactive documents, not as a substitute for a product retention
// policy. Applications should choose smaller values for untrusted plugins or
// constrained devices.
type Options struct {
	MaxEntries      int
	MaxStateBytes   int
	MaxScopeBytes   int
	MaxCommandBytes int
	MaxResultBytes  int
}

// DefaultOptions returns conservative interactive-history limits.
func DefaultOptions() Options {
	return Options{
		MaxEntries:      10_000,
		MaxStateBytes:   8 << 20,
		MaxScopeBytes:   256,
		MaxCommandBytes: 1 << 20,
		MaxResultBytes:  16 << 20,
	}
}

func (o Options) valid() bool {
	return o.MaxEntries > 0 && o.MaxStateBytes > 0 && o.MaxScopeBytes > 0 &&
		o.MaxCommandBytes > 0 && o.MaxResultBytes >= 0
}

// Result is the outcome of applying one opaque local command. Reverse is the
// command that compensates exactly the mutation just applied; it may differ
// from the original command because CRDT undo/redo commonly allocates fresh
// tags. Emitted is the canonical local delta or batch a host should persist
// and publish after it has atomically persisted its own CRDT state and this
// manager's MarshalBinary output.
type Result struct {
	Reverse []byte
	Emitted []byte
}

// Executor interprets one opaque command for a named local scope. It must
// either leave the scope unchanged and return an error, or atomically apply the
// command and return the compensating command. It must not retain aliases to
// command bytes. The executor owns CRDT type checks, authorization, and output
// framing; history only owns stack ordering and local-record bounds.
type Executor interface {
	Execute(scope string, command []byte) (Result, error)
}

// ExecutorFunc adapts a function to Executor.
type ExecutorFunc func(scope string, command []byte) (Result, error)

// Execute calls f.
func (f ExecutorFunc) Execute(scope string, command []byte) (Result, error) {
	if f == nil {
		return Result{}, ErrExecutor
	}
	return f(scope, command)
}

// Event describes one successfully applied local command. Emitted is copied so
// a caller can append it to a durable outbox without exposing manager storage.
type Event struct {
	Scope   string
	Command []byte
	Emitted []byte
}

type entry struct {
	scope string
	undo  []byte
	redo  []byte
}

// Manager tracks commands from any number of named scopes in one local stack.
// A scope normally identifies one concrete type instance, such as
// "richtext/body", "list/tasks", or "tree/outline". It does not observe
// direct CRDT mutations: call Execute for every local mutation that must be
// undoable, and route remote mutations directly to the CRDT without recording
// them.
//
// Manager serializes command execution so the stack order and host executor
// order agree. It never holds its state mutex while invoking Executor.
type Manager struct {
	execMu   sync.Mutex
	mu       sync.RWMutex
	executor Executor
	options  Options
	undo     []entry
	redo     []entry
}

// NewManager creates an empty local history manager.
func NewManager(executor Executor, options Options) (*Manager, error) {
	if executor == nil {
		return nil, ErrExecutor
	}
	if !options.valid() {
		return nil, ErrInvalidOptions
	}
	return &Manager{executor: executor, options: options}, nil
}

// NewManagerFromBinary restores a local undo/redo stack after the host has
// restored the corresponding CRDT state. The host must persist both records in
// one transaction; restoring a history against a different CRDT snapshot is a
// semantic error outside this package's ability to repair.
func NewManagerFromBinary(executor Executor, options Options, data []byte) (*Manager, error) {
	manager, err := NewManager(executor, options)
	if err != nil {
		return nil, err
	}
	undo, redo, err := unmarshalManager(data, options)
	if err != nil {
		return nil, err
	}
	manager.undo = undo
	manager.redo = redo
	return manager, nil
}

// Execute applies a new local command and records its returned compensating
// command. A successful new command clears the redo stack. The returned event
// contains the canonical payload to persist/publish; no remote operation is
// ever captured automatically.
func (m *Manager) Execute(scope string, command []byte) (Event, error) {
	if m == nil || m.executor == nil {
		return Event{}, ErrExecutor
	}
	if err := validateScope(scope, m.options.MaxScopeBytes); err != nil || !validCommand(command, m.options.MaxCommandBytes) {
		return Event{}, ErrInvalidCommand
	}
	m.execMu.Lock()
	defer m.execMu.Unlock()

	m.mu.RLock()
	full := len(m.undo) >= m.options.MaxEntries
	m.mu.RUnlock()
	if full {
		return Event{}, ErrResourceLimit
	}
	result, err := m.call(scope, command)
	if err != nil {
		return Event{}, err
	}
	if !validCommand(result.Reverse, m.options.MaxCommandBytes) || len(result.Emitted) > m.options.MaxResultBytes {
		return Event{}, ErrInvalidCommand
	}
	m.mu.Lock()
	m.undo = append(m.undo, entry{scope: scope, undo: cloneBytes(result.Reverse), redo: cloneBytes(command)})
	m.redo = nil
	m.mu.Unlock()
	return Event{Scope: scope, Command: cloneBytes(command), Emitted: cloneBytes(result.Emitted)}, nil
}

// Undo applies the compensating command for the latest captured local change.
// Its executor result replaces the redo command, allowing a type adapter to
// allocate fresh CRDT identities rather than trying to resurrect old ones.
func (m *Manager) Undo() (Event, error) {
	if m == nil || m.executor == nil {
		return Event{}, ErrExecutor
	}
	m.execMu.Lock()
	defer m.execMu.Unlock()
	m.mu.RLock()
	if len(m.undo) == 0 {
		m.mu.RUnlock()
		return Event{}, ErrNoUndo
	}
	current := cloneEntry(m.undo[len(m.undo)-1])
	m.mu.RUnlock()
	result, err := m.call(current.scope, current.undo)
	if err != nil {
		return Event{}, err
	}
	if !validCommand(result.Reverse, m.options.MaxCommandBytes) || len(result.Emitted) > m.options.MaxResultBytes {
		return Event{}, ErrInvalidCommand
	}
	m.mu.Lock()
	m.undo = m.undo[:len(m.undo)-1]
	current.redo = cloneBytes(result.Reverse)
	m.redo = append(m.redo, current)
	m.mu.Unlock()
	return Event{Scope: current.scope, Command: cloneBytes(current.undo), Emitted: cloneBytes(result.Emitted)}, nil
}

// Redo re-applies the most recently undone local change. Its executor result
// becomes the next undo command for the same reason described by Undo.
func (m *Manager) Redo() (Event, error) {
	if m == nil || m.executor == nil {
		return Event{}, ErrExecutor
	}
	m.execMu.Lock()
	defer m.execMu.Unlock()
	m.mu.RLock()
	if len(m.redo) == 0 {
		m.mu.RUnlock()
		return Event{}, ErrNoRedo
	}
	current := cloneEntry(m.redo[len(m.redo)-1])
	m.mu.RUnlock()
	result, err := m.call(current.scope, current.redo)
	if err != nil {
		return Event{}, err
	}
	if !validCommand(result.Reverse, m.options.MaxCommandBytes) || len(result.Emitted) > m.options.MaxResultBytes {
		return Event{}, ErrInvalidCommand
	}
	m.mu.Lock()
	m.redo = m.redo[:len(m.redo)-1]
	current.undo = cloneBytes(result.Reverse)
	m.undo = append(m.undo, current)
	m.mu.Unlock()
	return Event{Scope: current.scope, Command: cloneBytes(current.redo), Emitted: cloneBytes(result.Emitted)}, nil
}

// CanUndo reports whether one captured local command is available.
func (m *Manager) CanUndo() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.undo) > 0
}

// CanRedo reports whether one previously undone local command is available.
func (m *Manager) CanRedo() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.redo) > 0
}

// Len returns the total number of retained undo and redo entries.
func (m *Manager) Len() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.undo) + len(m.redo)
}

// Clear discards local history without modifying any CRDT scope.
func (m *Manager) Clear() {
	if m == nil {
		return
	}
	m.execMu.Lock()
	m.mu.Lock()
	m.undo = nil
	m.redo = nil
	m.mu.Unlock()
	m.execMu.Unlock()
}

// MarshalBinary returns a canonical, checksummed local-history record. It is
// not a CRDT frame and must not be sent to peers.
func (m *Manager) MarshalBinary() ([]byte, error) {
	if m == nil {
		return nil, ErrExecutor
	}
	m.mu.RLock()
	undo := cloneEntries(m.undo)
	redo := cloneEntries(m.redo)
	m.mu.RUnlock()
	return marshalManager(undo, redo, m.options)
}

func (m *Manager) call(scope string, command []byte) (result Result, err error) {
	defer func() {
		if recover() != nil {
			result = Result{}
			err = ErrExecutor
		}
	}()
	result, err = m.executor.Execute(scope, cloneBytes(command))
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

func marshalManager(undo, redo []entry, options Options) ([]byte, error) {
	if len(undo)+len(redo) > options.MaxEntries {
		return nil, ErrResourceLimit
	}
	encoded := make([]byte, 0, 256)
	encoded = append(encoded, historyMagic[:]...)
	encoded = append(encoded, historyFormatVersion)
	var err error
	if encoded, err = appendEntries(encoded, undo, options); err != nil {
		return nil, err
	}
	if encoded, err = appendEntries(encoded, redo, options); err != nil {
		return nil, err
	}
	if len(encoded) > options.MaxStateBytes-sha256.Size {
		return nil, ErrResourceLimit
	}
	digest := sha256.Sum256(encoded)
	return append(encoded, digest[:]...), nil
}

func appendEntries(encoded []byte, entries []entry, options Options) ([]byte, error) {
	if len(entries) > options.MaxEntries {
		return nil, ErrResourceLimit
	}
	if len(encoded)+frame.UvarintSize(uint64(len(entries))) > options.MaxStateBytes-sha256.Size {
		return nil, ErrResourceLimit
	}
	encoded = frame.AppendUvarint(encoded, uint64(len(entries)))
	for _, item := range entries {
		if err := validateEntry(item, options); err != nil {
			return nil, err
		}
		var ok bool
		encoded, ok = appendBoundedBytes(encoded, []byte(item.scope), options.MaxStateBytes-sha256.Size)
		if !ok {
			return nil, ErrResourceLimit
		}
		encoded, ok = appendBoundedBytes(encoded, item.undo, options.MaxStateBytes-sha256.Size)
		if !ok {
			return nil, ErrResourceLimit
		}
		encoded, ok = appendBoundedBytes(encoded, item.redo, options.MaxStateBytes-sha256.Size)
		if !ok {
			return nil, ErrResourceLimit
		}
	}
	return encoded, nil
}

func unmarshalManager(data []byte, options Options) ([]entry, []entry, error) {
	if !options.valid() || len(data) < len(historyMagic)+1+sha256.Size || len(data) > options.MaxStateBytes {
		return nil, nil, ErrInvalidState
	}
	payloadEnd := len(data) - sha256.Size
	digest := sha256.Sum256(data[:payloadEnd])
	if !bytes.Equal(digest[:], data[payloadEnd:]) || !bytes.Equal(data[:len(historyMagic)], historyMagic[:]) || data[len(historyMagic)] != historyFormatVersion {
		return nil, nil, ErrInvalidState
	}
	position := len(historyMagic) + 1
	undo, next, err := readEntries(data[:payloadEnd], position, options)
	if err != nil {
		return nil, nil, err
	}
	position = next
	redo, next, err := readEntries(data[:payloadEnd], position, options)
	if err != nil || next != payloadEnd || len(undo)+len(redo) > options.MaxEntries {
		return nil, nil, ErrInvalidState
	}
	return undo, redo, nil
}

func readEntries(data []byte, position int, options Options) ([]entry, int, error) {
	count, next, ok := frame.ReadUvarint(data, position)
	entryCount, bounded := decodeCount(count, options.MaxEntries)
	if !ok || !bounded {
		return nil, 0, ErrInvalidState
	}
	position = next
	entries := make([]entry, 0, entryCount)
	for index := 0; index < entryCount; index++ {
		scope, next, ok := frame.ReadBytes(data, position, options.MaxScopeBytes)
		if !ok {
			return nil, 0, ErrInvalidState
		}
		position = next
		undo, next, ok := frame.ReadBytes(data, position, options.MaxCommandBytes)
		if !ok {
			return nil, 0, ErrInvalidState
		}
		position = next
		redo, next, ok := frame.ReadBytes(data, position, options.MaxCommandBytes)
		if !ok {
			return nil, 0, ErrInvalidState
		}
		position = next
		item := entry{scope: string(scope), undo: cloneBytes(undo), redo: cloneBytes(redo)}
		if err := validateEntry(item, options); err != nil {
			return nil, 0, ErrInvalidState
		}
		entries = append(entries, item)
	}
	return entries, position, nil
}

func validateEntry(item entry, options Options) error {
	if validateScope(item.scope, options.MaxScopeBytes) != nil || !validCommand(item.undo, options.MaxCommandBytes) || !validCommand(item.redo, options.MaxCommandBytes) {
		return ErrInvalidCommand
	}
	return nil
}

func validateScope(scope string, maximum int) error {
	if maximum <= 0 || len(scope) == 0 || len(scope) > maximum || !utf8.ValidString(scope) {
		return ErrInvalidCommand
	}
	for _, value := range scope {
		if unicode.IsControl(value) || unicode.IsSpace(value) {
			return ErrInvalidCommand
		}
	}
	return nil
}

func validCommand(command []byte, maximum int) bool {
	return len(command) > 0 && len(command) <= maximum
}

// decodeCount bounds an attacker-controlled uvarint before converting it to
// int for allocation or loop control.
func decodeCount(value uint64, maximum int) (int, bool) {
	if maximum < 0 {
		return 0, false
	}
	limit := uint64(maximum)
	if value > limit {
		return 0, false
	}
	return int(value), true // #nosec G115 -- value is bounded by the nonnegative int maximum above.
}

func appendBoundedBytes(encoded, value []byte, maximum int) ([]byte, bool) {
	additional := frame.UvarintSize(uint64(len(value))) + len(value)
	if additional < 0 || len(encoded) > maximum || additional > maximum-len(encoded) {
		return nil, false
	}
	encoded = frame.AppendUvarint(encoded, uint64(len(value)))
	return append(encoded, value...), true
}

func cloneBytes(value []byte) []byte { return append([]byte(nil), value...) }

func cloneEntry(value entry) entry {
	return entry{scope: value.scope, undo: cloneBytes(value.undo), redo: cloneBytes(value.redo)}
}

func cloneEntries(values []entry) []entry {
	cloned := make([]entry, len(values))
	for index, value := range values {
		cloned[index] = cloneEntry(value)
	}
	return cloned
}
