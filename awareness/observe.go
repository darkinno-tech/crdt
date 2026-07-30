package awareness

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrNilCallback reports a nil local awareness callback.
	ErrNilCallback = errors.New("awareness: nil callback")
	// ErrSubscriptionLimit reports that local application observation exceeded
	// the Store's configured resource bound.
	ErrSubscriptionLimit = errors.New("awareness: subscription limit exceeded")
	// ErrNilContext reports an expiry loop without an explicit owner lifecycle.
	ErrNilContext = errors.New("awareness: nil context")
	// ErrInvalidExpiryInterval reports a non-positive expiry scheduling interval.
	ErrInvalidExpiryInterval = errors.New("awareness: invalid expiry interval")
)

// Origin identifies why a local presence observer received a new snapshot.
// It is process-local UI metadata and must never be transmitted or persisted.
type Origin uint8

const (
	// Initial is the state atomically captured when Subscribe is registered.
	Initial Origin = iota
	// Local is an update created through Set or Remove.
	Local
	// Remote is a newer update successfully accepted through Apply.
	Remote
	// Expired is a liveness transition made explicit by Expire.
	Expired
)

// Event is one immutable UI snapshot of ephemeral presence. Update is empty
// for Initial and Expired events; Active is sorted by actor. Every subscriber
// for one revision receives the same immutable snapshot, avoiding an
// observer-count multiplier for large presence lists. A slow subscription may
// skip superseded versions, in which case Coalesced reports how many pending
// events were replaced.
type Event struct {
	Version   uint64
	Origin    Origin
	Update    Update
	Active    []Update
	Coalesced uint64
}

// Callback receives one Event after Store releases its internal lock. It may
// call Store methods, but must treat Event and every nested byte slice as
// immutable. A callback panic stops only that subscription.
type Callback func(Event)

// Subscription is one bounded latest-state mailbox. Unsubscribe is idempotent;
// Done closes after an in-flight callback returns.
type Subscription struct{ subscriber *subscriber }

// Unsubscribe stops this callback without waiting for one already in progress.
func (subscription *Subscription) Unsubscribe() {
	if subscription != nil && subscription.subscriber != nil {
		subscription.subscriber.unsubscribe()
	}
}

// Done closes when callback delivery has stopped.
func (subscription *Subscription) Done() <-chan struct{} {
	if subscription == nil || subscription.subscriber == nil {
		return closedDone
	}
	return subscription.subscriber.done
}

var closedDone = func() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}()

// Subscribe atomically queues the current active snapshot and then later
// updates. It uses the current wall clock; deterministic simulations should use
// SubscribeAt with their simulated time.
func (store *Store) Subscribe(callback Callback) (*Subscription, error) {
	return store.SubscribeAt(time.Now(), callback)
}

// SubscribeAt is Subscribe with an explicit liveness time. It prevents a UI
// from missing a presence update between its initial read and registration.
func (store *Store) SubscribeAt(now time.Time, callback Callback) (*Subscription, error) {
	if store == nil {
		return nil, ErrInvalidOptions
	}
	if callback == nil {
		return nil, ErrNilCallback
	}
	now = normalizeTime(now)
	store.mu.Lock()
	defer store.mu.Unlock()
	subscription, err := store.hub.subscribe(callback)
	if err != nil {
		return nil, err
	}
	subscription.subscriber.enqueue(Event{
		Version: store.version,
		Origin:  Initial,
		Active:  store.activeAtLocked(now),
	})
	return subscription, nil
}

// Expire records liveness transitions which have passed Timeout and publishes
// one latest presence snapshot when any state becomes inactive. It deliberately
// does not create a wire update or erase the actor's clock tombstone. A later,
// strictly newer heartbeat can make the actor active again. Applications call
// it from their own scheduler so the Store never owns an unbounded timer or
// goroutine lifetime.
func (store *Store) Expire(now time.Time) bool {
	if store == nil {
		return false
	}
	now = normalizeTime(now)
	store.mu.Lock()
	changed := false
	for actor, current := range store.records {
		if current.update.Online() && !current.expired && now.Sub(current.lastSeen) > store.options.Timeout {
			current.expired = true
			store.records[actor] = current
			changed = true
		}
	}
	if changed {
		store.publishLocked(Expired, Update{}, now)
	}
	store.mu.Unlock()
	return changed
}

// ExpiryLoop is one caller-owned expiry scheduler. Done closes after its
// context is cancelled; cancelling a parent application context is the only
// shutdown mechanism, so a Store never retains an unbounded background task.
type ExpiryLoop struct{ done <-chan struct{} }

// Done closes once the expiry scheduler has stopped. It is already closed for
// a nil loop, matching Subscription's lifecycle behavior.
func (loop *ExpiryLoop) Done() <-chan struct{} {
	if loop == nil || loop.done == nil {
		return closedDone
	}
	return loop.done
}

// StartExpiry runs Expire immediately and then at interval until ctx is
// cancelled. It only makes local liveness transitions observable: it never
// sends a removal, mutates a CRDT, or deletes retained actor clocks. Use an
// interval no greater than Options.Timeout when the UI must reflect expiry
// promptly.
func (store *Store) StartExpiry(ctx context.Context, interval time.Duration) (*ExpiryLoop, error) {
	if store == nil {
		return nil, ErrInvalidOptions
	}
	if ctx == nil {
		return nil, ErrNilContext
	}
	if interval <= 0 {
		return nil, ErrInvalidExpiryInterval
	}
	done := make(chan struct{})
	loop := &ExpiryLoop{done: done}
	go func() {
		defer close(done)
		if ctx.Err() != nil {
			return
		}
		store.Expire(time.Now())
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				store.Expire(now)
			}
		}
	}()
	return loop, nil
}

func (store *Store) publishLocked(origin Origin, update Update, now time.Time) {
	store.version++
	if !store.hub.active() {
		return
	}
	store.hub.publish(Event{
		Version: store.version,
		Origin:  origin,
		Update:  cloneUpdate(update),
		Active:  store.activeAtLocked(now),
	})
}

type observationHub struct {
	mu          sync.Mutex
	nextID      uint64
	max         int
	subscribers map[uint64]*subscriber
	count       atomic.Uint64
}

func (hub *observationHub) active() bool { return hub.count.Load() != 0 }

func (hub *observationHub) subscribe(callback Callback) (*Subscription, error) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.subscribers) >= hub.max {
		return nil, ErrSubscriptionLimit
	}
	hub.nextID++
	subscriber := &subscriber{
		hub:      hub,
		id:       hub.nextID,
		callback: callback,
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	hub.subscribers[subscriber.id] = subscriber
	hub.count.Add(1)
	go subscriber.run()
	return &Subscription{subscriber: subscriber}, nil
}

func (hub *observationHub) publish(event Event) {
	hub.mu.Lock()
	for _, subscriber := range hub.subscribers {
		subscriber.enqueue(event)
	}
	hub.mu.Unlock()
}

func (hub *observationHub) remove(subscriber *subscriber) {
	hub.mu.Lock()
	if current, ok := hub.subscribers[subscriber.id]; ok && current == subscriber {
		delete(hub.subscribers, subscriber.id)
		hub.count.Add(^uint64(0))
	}
	hub.mu.Unlock()
}

type subscriber struct {
	hub      *observationHub
	id       uint64
	callback Callback

	mu       sync.Mutex
	pending  *Event
	stopped  bool
	wake     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func (subscriber *subscriber) enqueue(event Event) {
	subscriber.mu.Lock()
	if subscriber.stopped {
		subscriber.mu.Unlock()
		return
	}
	if subscriber.pending != nil {
		event.Coalesced += subscriber.pending.Coalesced + 1
	}
	subscriber.pending = &event
	subscriber.mu.Unlock()
	select {
	case subscriber.wake <- struct{}{}:
	default:
	}
}

func (subscriber *subscriber) unsubscribe() {
	subscriber.hub.remove(subscriber)
	subscriber.stopOnce.Do(func() {
		subscriber.mu.Lock()
		subscriber.stopped = true
		subscriber.pending = nil
		subscriber.mu.Unlock()
		close(subscriber.stop)
	})
}

func (subscriber *subscriber) run() {
	defer close(subscriber.done)
	for {
		select {
		case <-subscriber.stop:
			return
		case <-subscriber.wake:
			subscriber.mu.Lock()
			if subscriber.stopped {
				subscriber.mu.Unlock()
				return
			}
			event := subscriber.pending
			subscriber.pending = nil
			subscriber.mu.Unlock()
			if event == nil {
				continue
			}
			if !subscriber.call(*event) {
				return
			}
		}
	}
}

func (subscriber *subscriber) call(event Event) (completed bool) {
	completed = true
	defer func() {
		if recover() != nil {
			subscriber.unsubscribe()
			completed = false
		}
	}()
	subscriber.callback(event)
	return completed
}
