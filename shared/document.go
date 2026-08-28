// Package shared provides a small, Yjs-style document facade over the bounded
// document-tree-v2 CRDT. It intentionally keeps authentication, authorization,
// transport, durable outboxes, and recovery storage with the host application.
package shared

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"unicode/utf8"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/clock"
	"github.com/darkinno-tech/crdt/documenttree"
	frame "github.com/darkinno-tech/crdt/encoding"
)

var (
	// ErrNilDocument reports an operation on a nil shared document.
	ErrNilDocument = errors.New("shared: nil document")
	// ErrNilMap reports an operation on a nil shared map.
	ErrNilMap = errors.New("shared: nil map")
	// ErrNilArray reports an operation on a nil shared array.
	ErrNilArray = errors.New("shared: nil array")
	// ErrNilUpdateHandler reports a nil local-update subscriber.
	ErrNilUpdateHandler = errors.New("shared: nil update handler")
	// ErrValueKind reports an attempt to read an object or subdocument as bytes.
	ErrValueKind = errors.New("shared: value is not bytes")
)

// Options owns the local retention and frame budgets for one shared document.
// FrameLimits applies to both locally emitted updates and untrusted received
// updates. Production peers must negotiate compatible values and reject an
// oversized transport body before calling ApplyUpdate.
type Options struct {
	DocumentOptions documenttree.Options
	FrameLimits     frame.DecoderLimits
}

// DefaultOptions returns conservative per-document and frame limits for local
// development. A production group should use NewWithOptions with limits tied
// to its authenticated schema, tenant quota, and transport body limit.
func DefaultOptions() Options {
	return Options{
		DocumentOptions: documenttree.DefaultOptions(),
		FrameLimits:     frame.DefaultLimits(),
	}
}

// UpdateHandler receives one owned, canonical document-tree delta frame after
// a local successful mutation. It is called without an internal lock, may
// unsubscribe itself, and should hand the bytes to an outbox rather than doing
// blocking network I/O inline.
//
// The handler runs after the local state mutation. A host must durably record
// its own outbox and checkpoint according to its delivery guarantees.
type UpdateHandler func(update []byte)

// Document owns named Map and Array roots plus local update subscriptions. It
// is document-tree-v2, not a Yjs wire-compatible document. Its high-level
// methods intentionally hide delta construction; OnUpdate exposes the already
// bounded canonical frame at the transport boundary.
type Document struct {
	document    *documenttree.Document
	options     Options
	emptyUpdate []byte

	mu          sync.RWMutex
	nextHandler uint64
	handlers    map[uint64]UpdateHandler
}

// New creates a shared document with conservative local defaults. It is useful
// for a prototype or a process-local document. Networked applications should
// use NewWithLimits or NewWithOptions so the receive budget is deliberate.
func New(replicaID string) (*Document, error) {
	return NewWithOptions(replicaID, DefaultOptions())
}

// NewWithLimits creates a document with default retained-state limits and one
// explicit frame budget for both updates it emits and updates it receives.
func NewWithLimits(replicaID string, limits frame.DecoderLimits) (*Document, error) {
	options := DefaultOptions()
	options.FrameLimits = limits
	return NewWithOptions(replicaID, options)
}

// NewWithOptions creates a document with explicit retention and frame limits.
// An invalid frame budget is rejected before the document can be used.
func NewWithOptions(replicaID string, options Options) (*Document, error) {
	document, err := documenttree.NewWithOptionsAndOutputLimits(replicaID, options.DocumentOptions, options.FrameLimits)
	if err != nil {
		return nil, fmt.Errorf("shared: invalid document or frame limits: %w", err)
	}
	emptyUpdate, err := document.MarshalDeltaWithLimits(documenttree.Delta{}, options.FrameLimits)
	if err != nil {
		return nil, fmt.Errorf("shared: invalid frame limits: %w", err)
	}
	return newDocument(document, options, emptyUpdate), nil
}

// Restore creates a document from a complete checkpoint made by Checkpoint.
// The checkpoint and its HLC state must have been persisted atomically before
// reusing the same replica ID.
func Restore(checkpoint Checkpoint, options Options) (*Document, error) {
	document, err := documenttree.NewFromClockWithOptionsAndOutputLimits(checkpoint.ClockState, options.DocumentOptions, options.FrameLimits)
	if err != nil {
		return nil, fmt.Errorf("shared: invalid document or frame limits: %w", err)
	}
	if err := document.UnmarshalBinaryWithLimits(checkpoint.State, options.FrameLimits); err != nil {
		return nil, err
	}
	emptyUpdate, err := document.MarshalDeltaWithLimits(documenttree.Delta{}, options.FrameLimits)
	if err != nil {
		return nil, fmt.Errorf("shared: invalid frame limits: %w", err)
	}
	return newDocument(document, options, emptyUpdate), nil
}

func newDocument(document *documenttree.Document, options Options, emptyUpdate []byte) *Document {
	return &Document{
		document:    document,
		options:     options,
		emptyUpdate: append([]byte(nil), emptyUpdate...),
		handlers:    make(map[uint64]UpdateHandler),
	}
}

// Options returns the immutable-by-convention local limits selected for d.
func (d *Document) Options() Options {
	if d == nil {
		return Options{}
	}
	return d.options
}

// Profile returns the exact stable profile used by every shared document.
// It is guidance and protocol metadata only: the host still authenticates the
// resulting manifest and authorizes every received update.
func Profile() crdt.ReplicationProfile {
	profile, _ := crdt.ReplicationProfileFor("document/tree-v2")
	return profile
}

// OnUpdate subscribes to locally created update frames. The returned function
// is safe to call multiple times and from an update callback.
func (d *Document) OnUpdate(handler UpdateHandler) (func(), error) {
	if d == nil || d.document == nil {
		return nil, ErrNilDocument
	}
	if handler == nil {
		return nil, ErrNilUpdateHandler
	}
	d.mu.Lock()
	id := d.nextHandler
	d.nextHandler++
	d.handlers[id] = handler
	d.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			delete(d.handlers, id)
			d.mu.Unlock()
		})
	}, nil
}

// Map returns the named shared map, creating its root on first local use.
// Creation emits an update like any other local mutation. A name that already
// denotes an Array returns documenttree.ErrTypeMismatch.
func (d *Document) Map(name string) (*Map, error) {
	if d == nil || d.document == nil {
		return nil, ErrNilDocument
	}
	if value, ok := d.document.RootMap(name); ok {
		return &Map{document: d, value: value}, nil
	}
	value, delta, err := d.document.CreateRootMap(name)
	if err != nil {
		return nil, err
	}
	if err := d.emit(delta); err != nil {
		return nil, err
	}
	return &Map{document: d, value: value}, nil
}

// Array returns the named shared array, creating its root on first local use.
// Creation emits an update like any other local mutation.
func (d *Document) Array(name string) (*Array, error) {
	if d == nil || d.document == nil {
		return nil, ErrNilDocument
	}
	if value, ok := d.document.RootArray(name); ok {
		return &Array{document: d, value: value}, nil
	}
	value, delta, err := d.document.CreateRootArray(name)
	if err != nil {
		return nil, err
	}
	if err := d.emit(delta); err != nil {
		return nil, err
	}
	return &Array{document: d, value: value}, nil
}

// LookupMap returns the current visible named Map without creating a root or
// emitting a local update. It returns false for an absent, incomplete, or
// Array root, and for invalid names.
func (d *Document) LookupMap(name string) (*Map, bool) {
	if d == nil || d.document == nil {
		return nil, false
	}
	value, ok := d.document.RootMap(name)
	if !ok {
		return nil, false
	}
	return &Map{document: d, value: value}, true
}

// LookupArray returns the current visible named Array without creating a root
// or emitting a local update. It returns false for an absent, incomplete, or
// Map root, and for invalid names.
func (d *Document) LookupArray(name string) (*Array, bool) {
	if d == nil || d.document == nil {
		return nil, false
	}
	value, ok := d.document.RootArray(name)
	if !ok {
		return nil, false
	}
	return &Array{document: d, value: value}, true
}

// ApplyUpdate validates and applies an untrusted canonical update frame. The
// host must authenticate and authorize the peer and cap its transport body
// before calling this method; a profile, checksum, or successful decode is not
// authorization. Repeated and reordered accepted updates converge safely.
func (d *Document) ApplyUpdate(update []byte) error {
	if d == nil || d.document == nil {
		return ErrNilDocument
	}
	delta, err := documenttree.UnmarshalDeltaWithOptions(update, d.options.DocumentOptions, d.options.FrameLimits)
	if err != nil {
		return err
	}
	return d.document.ApplyDelta(delta)
}

// Checkpoint returns a complete state frame and HLC state that must be stored
// atomically before this replica ID is reused after restart.
func (d *Document) Checkpoint() (Checkpoint, error) {
	if d == nil || d.document == nil {
		return Checkpoint{}, ErrNilDocument
	}
	state, clockState, err := d.document.MarshalBinaryWithClockStateAndLimits(d.options.FrameLimits)
	if err != nil {
		return Checkpoint{}, err
	}
	return Checkpoint{State: state, ClockState: clockState}, nil
}

// State reports structural diagnostics without exposing document values.
func (d *Document) State() crdt.StateSnapshot {
	if d == nil || d.document == nil {
		return crdt.StateSnapshot{Type: "shared-document"}
	}
	return d.document.State()
}

func (d *Document) emit(delta documenttree.Delta) error {
	update, err := d.document.MarshalLocalDelta(delta)
	if err != nil {
		return fmt.Errorf("shared: encode local update: %w", err)
	}
	if bytes.Equal(update, d.emptyUpdate) {
		return nil
	}
	d.mu.RLock()
	handlers := make([]UpdateHandler, 0, len(d.handlers))
	for _, handler := range d.handlers {
		handlers = append(handlers, handler)
	}
	d.mu.RUnlock()
	for _, handler := range handlers {
		handler(append([]byte(nil), update...))
	}
	return nil
}

// Checkpoint is one recovery unit for a shared document. State and ClockState
// are both required; storing only State can reuse HLC tags after a restart.
type Checkpoint struct {
	State      []byte
	ClockState clock.State
}

// Map is a named or nested shared map. All mutating methods emit one local
// update after the underlying document-tree operation succeeds.
type Map struct {
	document *Document
	value    *documenttree.Map
}

// Set stores a copied byte value under key.
func (m *Map) Set(key string, value []byte) error {
	if m == nil || m.document == nil || m.value == nil {
		return ErrNilMap
	}
	delta, err := m.value.Set(key, value)
	if err != nil {
		return err
	}
	return m.document.emit(delta)
}

// SetString stores a UTF-8 string under key.
func (m *Map) SetString(key, value string) error {
	return m.Set(key, []byte(value))
}

// SetJSON marshals value as JSON before storing it under key.
func (m *Map) SetJSON(key string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("shared: marshal JSON: %w", err)
	}
	return m.Set(key, encoded)
}

// Delete removes key with an LWW tombstone.
func (m *Map) Delete(key string) error {
	if m == nil || m.document == nil || m.value == nil {
		return ErrNilMap
	}
	delta, err := m.value.Delete(key)
	if err != nil {
		return err
	}
	return m.document.emit(delta)
}

// CreateMap creates a nested shared map at key.
func (m *Map) CreateMap(key string) (*Map, error) {
	if m == nil || m.document == nil || m.value == nil {
		return nil, ErrNilMap
	}
	value, delta, err := m.value.CreateMap(key)
	if err != nil {
		return nil, err
	}
	if err := m.document.emit(delta); err != nil {
		return nil, err
	}
	return &Map{document: m.document, value: value}, nil
}

// CreateArray creates a nested shared array at key.
func (m *Map) CreateArray(key string) (*Array, error) {
	if m == nil || m.document == nil || m.value == nil {
		return nil, ErrNilMap
	}
	value, delta, err := m.value.CreateArray(key)
	if err != nil {
		return nil, err
	}
	if err := m.document.emit(delta); err != nil {
		return nil, err
	}
	return &Array{document: m.document, value: value}, nil
}

// Get returns a copied byte value when key is present.
func (m *Map) Get(key string) ([]byte, bool) {
	if m == nil || m.value == nil {
		return nil, false
	}
	value, ok := m.value.Get(key)
	if !ok || value.Kind != documenttree.ValueBytes {
		return nil, false
	}
	return value.Bytes, true
}

// String returns a stored valid UTF-8 byte value as a string.
func (m *Map) String(key string) (string, bool) {
	value, ok := m.Get(key)
	if !ok || !utf8.Valid(value) {
		return "", false
	}
	return string(value), true
}

// JSON decodes key into target. It returns false, nil when the key is absent.
func (m *Map) JSON(key string, target any) (bool, error) {
	if m == nil || m.value == nil {
		return false, ErrNilMap
	}
	value, exists := m.value.Get(key)
	if !exists {
		return false, nil
	}
	if value.Kind != documenttree.ValueBytes {
		return false, ErrValueKind
	}
	if err := json.Unmarshal(value.Bytes, target); err != nil {
		return true, fmt.Errorf("shared: unmarshal JSON: %w", err)
	}
	return true, nil
}

// Map returns a nested map when key currently holds one.
func (m *Map) Map(key string) (*Map, bool) {
	if m == nil || m.document == nil || m.value == nil {
		return nil, false
	}
	value, ok := m.value.Map(key)
	return &Map{document: m.document, value: value}, ok
}

// Array returns a nested array when key currently holds one.
func (m *Map) Array(key string) (*Array, bool) {
	if m == nil || m.document == nil || m.value == nil {
		return nil, false
	}
	value, ok := m.value.Array(key)
	return &Array{document: m.document, value: value}, ok
}

// Keys returns the current visible keys in canonical order.
func (m *Map) Keys() []string {
	if m == nil || m.value == nil {
		return nil
	}
	return m.value.Keys()
}

// Array is a named or nested shared array.
type Array struct {
	document *Document
	value    *documenttree.Array
}

// Len returns the current number of visible values.
func (a *Array) Len() int {
	if a == nil || a.value == nil {
		return 0
	}
	return a.value.Len()
}

// Insert stores a copied byte value at index.
func (a *Array) Insert(index int, value []byte) error {
	if a == nil || a.document == nil || a.value == nil {
		return ErrNilArray
	}
	delta, err := a.value.Insert(index, value)
	if err != nil {
		return err
	}
	return a.document.emit(delta)
}

// InsertString stores a UTF-8 string at index.
func (a *Array) InsertString(index int, value string) error {
	return a.Insert(index, []byte(value))
}

// InsertJSON marshals value as JSON before inserting it at index.
func (a *Array) InsertJSON(index int, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("shared: marshal JSON: %w", err)
	}
	return a.Insert(index, encoded)
}

// Delete removes count visible values beginning at index.
func (a *Array) Delete(index, count int) error {
	if a == nil || a.document == nil || a.value == nil {
		return ErrNilArray
	}
	delta, err := a.value.Delete(index, count)
	if err != nil {
		return err
	}
	return a.document.emit(delta)
}

// InsertMap creates a nested map at index.
func (a *Array) InsertMap(index int) (*Map, error) {
	if a == nil || a.document == nil || a.value == nil {
		return nil, ErrNilArray
	}
	value, delta, err := a.value.InsertMap(index)
	if err != nil {
		return nil, err
	}
	if err := a.document.emit(delta); err != nil {
		return nil, err
	}
	return &Map{document: a.document, value: value}, nil
}

// InsertArray creates a nested array at index.
func (a *Array) InsertArray(index int) (*Array, error) {
	if a == nil || a.document == nil || a.value == nil {
		return nil, ErrNilArray
	}
	value, delta, err := a.value.InsertArray(index)
	if err != nil {
		return nil, err
	}
	if err := a.document.emit(delta); err != nil {
		return nil, err
	}
	return &Array{document: a.document, value: value}, nil
}

// Get returns a copied byte value at index.
func (a *Array) Get(index int) ([]byte, bool) {
	if a == nil || a.value == nil {
		return nil, false
	}
	value, ok := a.value.Get(index)
	if !ok || value.Kind != documenttree.ValueBytes {
		return nil, false
	}
	return value.Bytes, true
}

// String returns a stored valid UTF-8 byte value at index.
func (a *Array) String(index int) (string, bool) {
	value, ok := a.Get(index)
	if !ok || !utf8.Valid(value) {
		return "", false
	}
	return string(value), true
}

// JSON decodes index into target. It returns false, nil when index is absent.
func (a *Array) JSON(index int, target any) (bool, error) {
	if a == nil || a.value == nil {
		return false, ErrNilArray
	}
	value, exists := a.value.Get(index)
	if !exists {
		return false, nil
	}
	if value.Kind != documenttree.ValueBytes {
		return false, ErrValueKind
	}
	if err := json.Unmarshal(value.Bytes, target); err != nil {
		return true, fmt.Errorf("shared: unmarshal JSON: %w", err)
	}
	return true, nil
}

// Map returns a nested map at index when the value currently holds one.
func (a *Array) Map(index int) (*Map, bool) {
	if a == nil || a.document == nil || a.value == nil {
		return nil, false
	}
	value, ok := a.value.Map(index)
	return &Map{document: a.document, value: value}, ok
}

// Array returns a nested array at index when the value currently holds one.
func (a *Array) Array(index int) (*Array, bool) {
	if a == nil || a.document == nil || a.value == nil {
		return nil, false
	}
	value, ok := a.value.Array(index)
	return &Array{document: a.document, value: value}, ok
}
