// Package document provides bounded, document-level routing for MoveRGA
// sequences. It deliberately does not define another replication wire format:
// applications authenticate and authorize a document ID before selecting the
// corresponding, separately negotiated MoveRGA stream.
package document

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/DarkInno/crdt/list"
)

var (
	ErrNilManager        = errors.New("document: nil manager")
	ErrInvalidDocumentID = errors.New("document: invalid document ID")
	ErrDocumentExists    = errors.New("document: document already exists")
	ErrDocumentNotFound  = errors.New("document: document not found")
	ErrResourceLimit     = errors.New("document: resource limit exceeded")
)

// Options bounds manager-owned document metadata. ListOptions applies to each
// individual document; it is intentionally not a shared global element budget.
// A replication service should choose both limits from its authenticated tenant
// and group policy rather than treating this manager as an authorization layer.
type Options struct {
	MaxDocuments       int
	MaxDocumentIDBytes int
	ListOptions        list.Options
}

// DefaultOptions returns conservative manager limits suitable for a single
// process. Production services should set limits appropriate to a tenant.
func DefaultOptions() Options {
	return Options{
		MaxDocuments:       16_384,
		MaxDocumentIDBytes: 256,
		ListOptions:        list.DefaultOptions(),
	}
}

func (o Options) valid() bool {
	return o.MaxDocuments > 0 && o.MaxDocumentIDBytes > 0
}

// DocManager owns a fixed, explicitly created set of MoveRGA documents. It
// never creates a document while applying a remote delta: otherwise a peer
// could turn arbitrary document IDs into unbounded retained state.
//
// The manager lock only protects the registry. Each returned MoveRGA has its
// own lock, so operations on different documents do not serialize globally.
type DocManager[T any] struct {
	mu      sync.RWMutex
	codec   list.ElementCodec[T]
	options Options
	docs    map[string]*list.MoveRGA[T]
}

// NewDocManager creates an empty document registry. It validates the shared
// codec and per-document options up front, so an invalid manager cannot become
// partially populated before its first CreateDocument call fails.
func NewDocManager[T any](codec list.ElementCodec[T], options Options) (*DocManager[T], error) {
	if !options.valid() {
		return nil, ErrResourceLimit
	}
	if _, err := list.NewMoveRGAWithOptions("document-manager-validation", codec, options.ListOptions); err != nil {
		return nil, err
	}
	return &DocManager[T]{
		codec:   codec,
		options: options,
		docs:    make(map[string]*list.MoveRGA[T]),
	}, nil
}

// CreateDocument creates a locally authorized document with its own HLC
// replica ID. Call it on every replica before accepting deltas for that
// document. Reusing one logical replica ID after a restart requires restoring
// the document's clock together with its complete snapshot.
func (m *DocManager[T]) CreateDocument(id, replicaID string) (*list.MoveRGA[T], error) {
	if m == nil {
		return nil, ErrNilManager
	}
	if !m.validDocumentID(id) {
		return nil, ErrInvalidDocumentID
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.docs[id]; exists {
		return nil, ErrDocumentExists
	}
	if len(m.docs) >= m.options.MaxDocuments {
		return nil, ErrResourceLimit
	}
	document, err := list.NewMoveRGAWithOptions(replicaID, m.codec, m.options.ListOptions)
	if err != nil {
		return nil, err
	}
	m.docs[id] = document
	return document, nil
}

// Document returns a document only when it was explicitly created. The
// returned CRDT remains independently concurrency-safe.
func (m *DocManager[T]) Document(id string) (*list.MoveRGA[T], bool) {
	if m == nil || !m.validDocumentID(id) {
		return nil, false
	}
	m.mu.RLock()
	document, ok := m.docs[id]
	m.mu.RUnlock()
	return document, ok
}

// ApplyDelta routes one already-decoded MoveRGA delta to an existing document.
// Transport-facing callers must decode with list.UnmarshalMoveDeltaWithLimits
// and complete authentication/authorization before calling this method.
func (m *DocManager[T]) ApplyDelta(id string, delta list.MoveDelta) error {
	if m == nil {
		return ErrNilManager
	}
	document, ok := m.Document(id)
	if !ok {
		return ErrDocumentNotFound
	}
	return document.ApplyDelta(delta)
}

// Insert routes a local insertion to an existing document.
func (m *DocManager[T]) Insert(id string, offset int, values []T) (list.MoveDelta, error) {
	document, err := m.requireDocument(id)
	if err != nil {
		return list.MoveDelta{}, err
	}
	return document.Insert(offset, values)
}

// Delete routes a local deletion to an existing document.
func (m *DocManager[T]) Delete(id string, offset, count int) (list.MoveDelta, error) {
	document, err := m.requireDocument(id)
	if err != nil {
		return list.MoveDelta{}, err
	}
	return document.Delete(offset, count)
}

// Move routes a local identity-preserving range move to an existing document.
// The destination offset is interpreted after the selected range is removed.
func (m *DocManager[T]) Move(id string, from, count, to int) (list.MoveDelta, error) {
	document, err := m.requireDocument(id)
	if err != nil {
		return list.MoveDelta{}, err
	}
	return document.Move(from, count, to)
}

// DocumentIDs returns a sorted copy so callers can enumerate a stable view
// without gaining access to the manager's map.
func (m *DocManager[T]) DocumentIDs() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	ids := make([]string, 0, len(m.docs))
	for id := range m.docs {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	sort.Strings(ids)
	return ids
}

// Len reports the number of locally opened documents.
func (m *DocManager[T]) Len() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.docs)
}

func (m *DocManager[T]) requireDocument(id string) (*list.MoveRGA[T], error) {
	if m == nil {
		return nil, ErrNilManager
	}
	document, ok := m.Document(id)
	if !ok {
		return nil, ErrDocumentNotFound
	}
	return document, nil
}

func (m *DocManager[T]) validDocumentID(id string) bool {
	if !utf8.ValidString(id) || len(id) == 0 || len(id) > m.options.MaxDocumentIDBytes || strings.TrimSpace(id) != id {
		return false
	}
	for _, value := range id {
		if unicode.IsControl(value) {
			return false
		}
	}
	return true
}
