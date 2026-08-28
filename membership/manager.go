package membership

import (
	"crypto/ed25519"
	"errors"
	"sync"

	"github.com/darkinno-tech/crdt/tombstonegc"
)

// ViewStore persists a signed view before it is made active in memory. A real
// implementation must make Save durable before returning nil; a lost view
// could otherwise allow a restarted process to accept an old epoch.
type ViewStore interface {
	LoadView() (View, bool, error)
	SaveView(View) error
}

// MemoryStore is a concurrency-safe store for tests, examples, and embedding
// applications that already provide their own durable control-plane store.
// It intentionally makes no durability claim.
type MemoryStore struct {
	mu   sync.RWMutex
	view View
	ok   bool
}

func (s *MemoryStore) LoadView() (View, bool, error) {
	if s == nil {
		return View{}, false, errors.New("membership: nil view store")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneView(s.view), s.ok, nil
}

func (s *MemoryStore) SaveView(view View) error {
	if s == nil {
		return errors.New("membership: nil view store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.view = cloneView(view)
	s.ok = true
	return nil
}

// Manager is the bridge from a signed authoritative View to a Coordinator.
// It persists a valid next view before fencing the local data plane at that
// epoch. The caller must use the same epoch in replica.Manifest during its
// authenticated handshake.
type Manager[T comparable] struct {
	mu           sync.RWMutex
	authorityKey ed25519.PublicKey
	store        ViewStore
	view         View
	coordinator  *tombstonegc.Coordinator[T]
}

// NewManager verifies and durably installs initial before exposing a
// coordinator. It is suitable for first bootstrap and for a process that has
// obtained the currently authoritative view from its control plane.
func NewManager[T comparable](initial View, authorityKey ed25519.PublicKey, store ViewStore) (*Manager[T], error) {
	if store == nil || VerifyView(initial, authorityKey) != nil {
		return nil, ErrInvalidView
	}
	if err := store.SaveView(initial); err != nil {
		return nil, err
	}
	coordinator, err := tombstonegc.NewCoordinatorAtMembership[T](toCoordinatorMembership(initial))
	if err != nil {
		return nil, err
	}
	return &Manager[T]{
		authorityKey: append(ed25519.PublicKey(nil), authorityKey...),
		store:        store,
		view:         cloneView(initial),
		coordinator:  coordinator,
	}, nil
}

// OpenManager restores a previously persisted signed view. Receipt state is
// intentionally not restored: losing it delays GC, while restoring an
// untrusted or stale receipt could compact too early.
func OpenManager[T comparable](authorityKey ed25519.PublicKey, store ViewStore) (*Manager[T], error) {
	if store == nil {
		return nil, ErrMissingView
	}
	view, ok, err := store.LoadView()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrMissingView
	}
	if VerifyView(view, authorityKey) != nil {
		return nil, ErrInvalidView
	}
	coordinator, err := tombstonegc.NewCoordinatorAtMembership[T](toCoordinatorMembership(view))
	if err != nil {
		return nil, err
	}
	return &Manager[T]{
		authorityKey: append(ed25519.PublicKey(nil), authorityKey...),
		store:        store,
		view:         cloneView(view),
		coordinator:  coordinator,
	}, nil
}

// Install verifies a direct successor, persists it, and then advances the
// coordinator epoch. A process crash after persistence is safe because
// OpenManager restores the newer fence; a crash before persistence leaves the
// older in-memory state and therefore only delays collection.
func (m *Manager[T]) Install(next View) error {
	if m == nil || VerifyView(next, m.authorityKey) != nil {
		return ErrInvalidView
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if next.GroupID != m.view.GroupID {
		return ErrGroupMismatch
	}
	if next.Epoch <= m.view.Epoch {
		if next.Epoch == m.view.Epoch && next.Hash() == m.view.Hash() {
			return nil
		}
		return ErrViewRollback
	}
	if next.Epoch != m.view.Epoch+1 {
		return ErrViewFork
	}
	if next.PreviousHash != m.view.Hash() {
		return ErrViewFork
	}
	if err := m.store.SaveView(next); err != nil {
		return err
	}
	if err := m.coordinator.InstallMembership(toCoordinatorMembership(next)); err != nil {
		return err
	}
	m.view = cloneView(next)
	return nil
}

// View returns an immutable copy of the active view.
func (m *Manager[T]) View() View {
	if m == nil {
		return View{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneView(m.view)
}

// Coordinator exposes the local exact-acknowledgement coordinator. All
// membership changes must continue through Install, never through direct
// ReplaceMembership calls.
func (m *Manager[T]) Coordinator() *tombstonegc.Coordinator[T] {
	if m == nil {
		return nil
	}
	return m.coordinator
}

func toCoordinatorMembership(view View) tombstonegc.Membership {
	members := make([]string, len(view.Members))
	for index, member := range view.Members {
		members[index] = member.ID
	}
	return tombstonegc.Membership{GroupID: view.GroupID, Epoch: view.Epoch, Members: members}
}
