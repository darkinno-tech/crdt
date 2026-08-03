package observe

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/DarkInno/crdt"
)

var (
	// ErrNilStore reports an operation on a nil Store.
	ErrNilStore = errors.New("observe: nil store")
	// ErrNilView reports a Store constructed without an immutable view function.
	ErrNilView = errors.New("observe: nil view function")
	// ErrNilMutation reports a nil mutation function.
	ErrNilMutation = errors.New("observe: nil mutation function")
	// ErrNilCallback reports a nil subscriber callback.
	ErrNilCallback = errors.New("observe: nil callback")
	// ErrInvalidOrigin reports an origin that cannot describe a mutation.
	ErrInvalidOrigin = errors.New("observe: invalid mutation origin")
	// ErrClosed reports a mutation or subscription after Store.Close.
	ErrClosed = errors.New("observe: store is closed")
)

// Origin identifies why an application-visible state update was committed.
// It is local process metadata, not a CRDT conflict-resolution input and not
// a wire-protocol field.
type Origin uint8

const (
	// Initial describes the current state delivered immediately after Subscribe.
	Initial Origin = iota
	// Local describes a successful local user or application mutation.
	Local
	// Remote describes a successfully applied remote delta or state update.
	Remote
	// Merge describes a successful CRDT state merge.
	Merge
	// Restore describes successful installation of recovered local state.
	Restore
	// Maintenance describes a successful local maintenance operation, such as
	// an authority-approved tombstone compaction.
	Maintenance
)

// String returns a stable diagnostic name for o.
func (o Origin) String() string {
	switch o {
	case Initial:
		return "initial"
	case Local:
		return "local"
	case Remote:
		return "remote"
	case Merge:
		return "merge"
	case Restore:
		return "restore"
	case Maintenance:
		return "maintenance"
	default:
		return "invalid"
	}
}

func (o Origin) mutation() bool {
	return o == Local || o == Remote || o == Merge || o == Restore || o == Maintenance
}

// Event is one application-visible Store revision. Value is produced by the
// application-supplied view function while the Store serializes a mutation.
// It must be treated as immutable by every callback.
//
// Coalesced reports how many older, not-yet-delivered events this event
// replaced for this particular subscriber. Event versions are monotonic per
// Store, but a subscriber may observe gaps when it is slower than mutations.
type Event[V any] struct {
	Version   uint64
	Origin    Origin
	Value     V
	State     crdt.StateSnapshot
	Coalesced uint64
}

// Callback receives one Event on a subscriber-owned goroutine. It must not
// retain mutable aliases from Event.Value or block indefinitely. A panic is
// contained, recorded on the Subscription, and unsubscribes that callback.
type Callback[V any] func(Event[V])

// Panic describes a callback panic captured by the observer dispatcher.
// Value is the recovered panic value and must be treated as diagnostic data.
type Panic struct {
	Value        any
	EventVersion uint64
	Origin       Origin
}

// Options controls diagnostic handling for a Store. OnPanic runs after a
// callback panic, on the failing callback's goroutine. It must return quickly;
// its own panic is contained. Callback delivery never depends on this hook.
type Options struct {
	OnPanic func(Panic)
}

// Store owns one application-facing mutation path around a CRDT or another
// StateReporter. T should not be mutated outside Mutate while observers are
// active, otherwise Store cannot preserve version/event ordering.
//
// V is an application view such as a counter value, immutable document text,
// or copied list of set elements. Store does not use V for CRDT merge or wire
// semantics.
type Store[T crdt.StateReporter, V any] struct {
	mu      sync.Mutex
	value   T
	view    func(T) V
	version uint64
	closed  bool
	hub     hub[V]
}

// New creates a Store with an application-owned view function. The view runs
// only to serve a subscription or publish to at least one subscriber, so a
// Store without subscribers does not pay projection/copy costs on mutations.
func New[T crdt.StateReporter, V any](value T, view func(T) V) (*Store[T, V], error) {
	return NewWithOptions(value, view, Options{})
}

// NewWithOptions creates a Store with diagnostic callback-panic handling.
func NewWithOptions[T crdt.StateReporter, V any](value T, view func(T) V, options Options) (*Store[T, V], error) {
	if view == nil {
		return nil, ErrNilView
	}
	store := &Store[T, V]{value: value, view: view}
	store.hub.onPanic = options.OnPanic
	store.hub.subscribers = make(map[uint64]*subscriber[V])
	return store, nil
}

// Mutate runs mutation under Store's serialization gate. A nil or failed
// mutation does not advance Version or notify subscribers. Mutate assumes a
// successful mutation changed the application-visible state; callers that can
// determine otherwise should use MutateIf. The wrapped CRDT operation must
// retain its documented all-or-nothing-on-error behavior.
//
// mutation is intentionally not a notification callback: it may change T and
// must not recursively call methods on the same Store. Subscribers run only
// after this method releases the Store lock, so they may safely call Mutate.
func (s *Store[T, V]) Mutate(origin Origin, mutation func(T) error) error {
	if s == nil {
		return ErrNilStore
	}
	if !origin.mutation() {
		return ErrInvalidOrigin
	}
	if mutation == nil {
		return ErrNilMutation
	}
	_, err := s.MutateIf(origin, func(value T) (bool, error) {
		return true, mutation(value)
	})
	return err
}

// MutateIf runs mutation under Store's serialization gate and publishes a
// revision only when mutation reports changed. A nil, failed, or unchanged
// mutation does not advance Version or notify subscribers. It is suitable for
// idempotent remote CRDT joins whose caller can determine whether a duplicate
// delivery extended retained state.
//
// Like Mutate, mutation is not a notification callback and must not recursively
// call methods on the same Store. Subscribers run only after this method
// releases the Store lock, so they may safely start a later mutation.
func (s *Store[T, V]) MutateIf(origin Origin, mutation func(T) (changed bool, err error)) (bool, error) {
	if s == nil {
		return false, ErrNilStore
	}
	if !origin.mutation() {
		return false, ErrInvalidOrigin
	}
	if mutation == nil {
		return false, ErrNilMutation
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, ErrClosed
	}
	changed, err := mutation(s.value)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	s.version++
	if !s.hub.active() {
		return true, nil
	}
	s.hub.publish(Event[V]{
		Version: s.version,
		Origin:  origin,
		Value:   s.view(s.value),
		State:   s.value.State(),
	})
	return true, nil
}

// Snapshot returns the current reactive view. Its Origin is Initial because
// it is a point-in-time read rather than a mutation notification.
func (s *Store[T, V]) Snapshot() (Event[V], error) {
	if s == nil {
		return Event[V]{}, ErrNilStore
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Event[V]{
		Version: s.version,
		Origin:  Initial,
		Value:   s.view(s.value),
		State:   s.value.State(),
	}, nil
}

// Subscribe atomically registers callback and queues the Store's current view
// before later updates may publish. This prevents a UI from missing a change
// between reading state and starting observation. If the callback is slow,
// that initial event may be coalesced into a newer event before delivery.
func (s *Store[T, V]) Subscribe(callback Callback[V]) (*Subscription[V], error) {
	return s.subscribe(callback, true)
}

// SubscribeFromNow registers callback without an initial state event. It is
// for consumers that have already obtained a coherent Snapshot themselves.
func (s *Store[T, V]) SubscribeFromNow(callback Callback[V]) (*Subscription[V], error) {
	return s.subscribe(callback, false)
}

func (s *Store[T, V]) subscribe(callback Callback[V], initial bool) (*Subscription[V], error) {
	if s == nil {
		return nil, ErrNilStore
	}
	if callback == nil {
		return nil, ErrNilCallback
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	subscription := s.hub.subscribe(callback)
	if initial {
		subscription.subscriber.enqueue(Event[V]{
			Version: s.version,
			Origin:  Initial,
			Value:   s.view(s.value),
			State:   s.value.State(),
		})
	}
	return subscription, nil
}

// Close prevents future mutations and subscriptions, and cancels all current
// subscriptions. It does not wait for a callback already in progress; use a
// Subscription's Done channel when a caller needs to wait for quiescence.
func (s *Store[T, V]) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.hub.closeAll()
	}
	s.mu.Unlock()
}

// Subscription owns one callback registration. Unsubscribe is idempotent.
// Done closes after the subscription goroutine has stopped, including any
// callback that was already executing at the time of Unsubscribe.
type Subscription[V any] struct {
	subscriber *subscriber[V]
}

// Unsubscribe cancels delivery to this subscription without waiting for an
// in-progress callback. It is safe to call from the callback itself.
func (s *Subscription[V]) Unsubscribe() {
	if s == nil || s.subscriber == nil {
		return
	}
	s.subscriber.unsubscribe()
}

// Done is closed after all delivery work for this subscription stops.
func (s *Subscription[V]) Done() <-chan struct{} {
	if s == nil || s.subscriber == nil {
		return alreadyDone
	}
	return s.subscriber.done
}

// Panic returns the callback panic, if one caused this subscription to stop.
func (s *Subscription[V]) Panic() (Panic, bool) {
	if s == nil || s.subscriber == nil {
		return Panic{}, false
	}
	return s.subscriber.panicInfo()
}

var alreadyDone = func() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}()

type hub[V any] struct {
	mu          sync.Mutex
	nextID      uint64
	subscribers map[uint64]*subscriber[V]
	count       atomic.Uint64
	onPanic     func(Panic)
}

func (h *hub[V]) active() bool { return h.count.Load() != 0 }

func (h *hub[V]) subscribe(callback Callback[V]) *Subscription[V] {
	h.mu.Lock()
	h.nextID++
	sub := &subscriber[V]{
		hub:      h,
		id:       h.nextID,
		callback: callback,
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	h.subscribers[sub.id] = sub
	h.count.Add(1)
	h.mu.Unlock()
	go sub.run()
	return &Subscription[V]{subscriber: sub}
}

// publish never calls application code. h.mu protects membership while each
// subscriber's own lock protects its single coalescing mailbox. Unsubscribe
// removes itself from h before taking its mailbox lock, so this lock order
// cannot deadlock with publication.
func (h *hub[V]) publish(event Event[V]) {
	h.mu.Lock()
	for _, sub := range h.subscribers {
		sub.enqueue(event)
	}
	h.mu.Unlock()
}

func (h *hub[V]) remove(sub *subscriber[V]) {
	h.mu.Lock()
	if existing, ok := h.subscribers[sub.id]; ok && existing == sub {
		delete(h.subscribers, sub.id)
		h.count.Add(^uint64(0))
	}
	h.mu.Unlock()
}

func (h *hub[V]) closeAll() {
	h.mu.Lock()
	subscribers := h.subscribers
	h.subscribers = make(map[uint64]*subscriber[V])
	h.count.Store(0)
	h.mu.Unlock()
	for _, sub := range subscribers {
		sub.stopDelivery()
	}
}

func (h *hub[V]) reportPanic(info Panic) {
	if h.onPanic == nil {
		return
	}
	func() {
		defer func() { _ = recover() }()
		h.onPanic(info)
	}()
}

type subscriber[V any] struct {
	hub      *hub[V]
	id       uint64
	callback Callback[V]

	mu       sync.Mutex
	pending  *Event[V]
	stopped  bool
	panic    *Panic
	wake     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func (s *subscriber[V]) enqueue(event Event[V]) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	if s.pending != nil {
		event.Coalesced += s.pending.Coalesced + 1
	}
	s.pending = &event
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *subscriber[V]) unsubscribe() {
	s.hub.remove(s)
	s.stopDelivery()
}

func (s *subscriber[V]) stopDelivery() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.pending = nil
		s.mu.Unlock()
		close(s.stop)
	})
}

func (s *subscriber[V]) panicInfo() (Panic, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.panic == nil {
		return Panic{}, false
	}
	return *s.panic, true
}

func (s *subscriber[V]) run() {
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			return
		case <-s.wake:
			s.mu.Lock()
			if s.stopped {
				s.mu.Unlock()
				return
			}
			event := s.pending
			s.pending = nil
			s.mu.Unlock()
			if event == nil {
				continue
			}
			if !s.call(*event) {
				return
			}
		}
	}
}

func (s *subscriber[V]) call(event Event[V]) (completed bool) {
	completed = true
	defer func() {
		if recovered := recover(); recovered != nil {
			info := Panic{Value: recovered, EventVersion: event.Version, Origin: event.Origin}
			s.mu.Lock()
			s.panic = &info
			s.mu.Unlock()
			s.unsubscribe()
			s.hub.reportPanic(info)
			completed = false
		}
	}()
	s.callback(event)
	return completed
}
