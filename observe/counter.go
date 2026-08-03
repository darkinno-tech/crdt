package observe

import (
	"errors"

	"github.com/DarkInno/crdt/counter"
)

// GCounterView is the immutable application projection of a G-Counter. An
// accepted distributed state can have more valid uint64 components than fit in
// one uint64 aggregate; in that case Overflow is true and Value is zero.
type GCounterView struct {
	Value    uint64
	Overflow bool
}

// PNCounterView is the immutable application projection of a PN-Counter. The
// decimal representation preserves the full signed range of its uint64
// components without exposing a mutable big.Int to callbacks.
type PNCounterView struct {
	Value string
}

// GCounterObserver owns one G-Counter mutation and observation boundary. Local
// increments return a delta for the authenticated application transport;
// ApplyDelta accepts an already-decoded remote delta. It does not establish a
// network connection, authenticate peers, or persist state.
type GCounterObserver struct {
	store *Store[*counter.GCounter, GCounterView]
}

// NewGCounterObserver creates a distributed G-Counter observation boundary.
func NewGCounterObserver(replicaID string) (*GCounterObserver, error) {
	return NewGCounterObserverWithOptions(replicaID, Options{})
}

// NewGCounterObserverWithOptions creates a G-Counter observer with callback
// panic diagnostics selected by options.
func NewGCounterObserverWithOptions(replicaID string, options Options) (*GCounterObserver, error) {
	value, err := counter.NewGCounter(replicaID)
	if err != nil {
		return nil, err
	}
	store, err := NewWithOptions(value, gCounterView, options)
	if err != nil {
		return nil, err
	}
	return &GCounterObserver{store: store}, nil
}

// Increment changes this replica's component and returns its joinable delta.
// The caller owns framing, authenticated transport, durable acknowledgement,
// and retry; it must not send Event.Version as a wire or causal value.
func (o *GCounterObserver) Increment(amount uint64) (counter.GCounterDelta, error) {
	if o == nil || o.store == nil {
		return counter.GCounterDelta{}, ErrNilStore
	}
	var delta counter.GCounterDelta
	_, err := o.store.MutateIf(Local, func(value *counter.GCounter) (bool, error) {
		var err error
		delta, err = value.Increment(amount)
		return err == nil, err
	})
	if err != nil {
		return counter.GCounterDelta{}, err
	}
	return delta, nil
}

// ApplyDelta joins one decoded remote G-Counter delta. It returns false for a
// duplicate or subsumed delta and deliberately publishes no Remote event then.
func (o *GCounterObserver) ApplyDelta(delta counter.GCounterDelta) (bool, error) {
	if o == nil || o.store == nil {
		return false, ErrNilStore
	}
	return o.store.MutateIf(Remote, func(value *counter.GCounter) (bool, error) {
		return value.ApplyDeltaChanged(delta)
	})
}

// Snapshot returns the current G-Counter projection and local UI revision.
func (o *GCounterObserver) Snapshot() (Event[GCounterView], error) {
	if o == nil || o.store == nil {
		return Event[GCounterView]{}, ErrNilStore
	}
	return o.store.Snapshot()
}

// Subscribe atomically queues the current G-Counter projection before later
// counter changes may publish.
func (o *GCounterObserver) Subscribe(callback Callback[GCounterView]) (*Subscription[GCounterView], error) {
	if o == nil || o.store == nil {
		return nil, ErrNilStore
	}
	return o.store.Subscribe(callback)
}

// SubscribeFromNow registers a G-Counter callback without an initial view.
func (o *GCounterObserver) SubscribeFromNow(callback Callback[GCounterView]) (*Subscription[GCounterView], error) {
	if o == nil || o.store == nil {
		return nil, ErrNilStore
	}
	return o.store.SubscribeFromNow(callback)
}

// Close stops this observer and its active subscriptions.
func (o *GCounterObserver) Close() {
	if o != nil && o.store != nil {
		o.store.Close()
	}
}

// PNCounterObserver owns one PN-Counter mutation and observation boundary.
// Local changes return deltas for an application-owned authenticated transport;
// remote deltas are observed only when they extend either component map.
type PNCounterObserver struct {
	store *Store[*counter.PNCounter, PNCounterView]
}

// NewPNCounterObserver creates a distributed PN-Counter observation boundary.
func NewPNCounterObserver(replicaID string) (*PNCounterObserver, error) {
	return NewPNCounterObserverWithOptions(replicaID, Options{})
}

// NewPNCounterObserverWithOptions creates a PN-Counter observer with callback
// panic diagnostics selected by options.
func NewPNCounterObserverWithOptions(replicaID string, options Options) (*PNCounterObserver, error) {
	value, err := counter.NewPNCounter(replicaID)
	if err != nil {
		return nil, err
	}
	store, err := NewWithOptions(value, pnCounterView, options)
	if err != nil {
		return nil, err
	}
	return &PNCounterObserver{store: store}, nil
}

// Increment changes this replica's positive component and returns its delta.
func (o *PNCounterObserver) Increment(amount uint64) (counter.PNCounterDelta, error) {
	return o.change(amount, true)
}

// Decrement changes this replica's negative component and returns its delta.
func (o *PNCounterObserver) Decrement(amount uint64) (counter.PNCounterDelta, error) {
	return o.change(amount, false)
}

func (o *PNCounterObserver) change(amount uint64, positive bool) (counter.PNCounterDelta, error) {
	if o == nil || o.store == nil {
		return counter.PNCounterDelta{}, ErrNilStore
	}
	var delta counter.PNCounterDelta
	_, err := o.store.MutateIf(Local, func(value *counter.PNCounter) (bool, error) {
		var err error
		if positive {
			delta, err = value.Increment(amount)
		} else {
			delta, err = value.Decrement(amount)
		}
		return err == nil, err
	})
	if err != nil {
		return counter.PNCounterDelta{}, err
	}
	return delta, nil
}

// ApplyDelta joins one decoded remote PN-Counter delta. Duplicate or subsumed
// delivery returns changed == false and produces no Remote event.
func (o *PNCounterObserver) ApplyDelta(delta counter.PNCounterDelta) (bool, error) {
	if o == nil || o.store == nil {
		return false, ErrNilStore
	}
	return o.store.MutateIf(Remote, func(value *counter.PNCounter) (bool, error) {
		return value.ApplyDeltaChanged(delta)
	})
}

// Snapshot returns the current PN-Counter projection and local UI revision.
func (o *PNCounterObserver) Snapshot() (Event[PNCounterView], error) {
	if o == nil || o.store == nil {
		return Event[PNCounterView]{}, ErrNilStore
	}
	return o.store.Snapshot()
}

// Subscribe atomically queues the current PN-Counter projection before later
// counter changes may publish.
func (o *PNCounterObserver) Subscribe(callback Callback[PNCounterView]) (*Subscription[PNCounterView], error) {
	if o == nil || o.store == nil {
		return nil, ErrNilStore
	}
	return o.store.Subscribe(callback)
}

// SubscribeFromNow registers a PN-Counter callback without an initial view.
func (o *PNCounterObserver) SubscribeFromNow(callback Callback[PNCounterView]) (*Subscription[PNCounterView], error) {
	if o == nil || o.store == nil {
		return nil, ErrNilStore
	}
	return o.store.SubscribeFromNow(callback)
}

// Close stops this observer and its active subscriptions.
func (o *PNCounterObserver) Close() {
	if o != nil && o.store != nil {
		o.store.Close()
	}
}

func gCounterView(value *counter.GCounter) GCounterView {
	total, err := value.Value()
	if err == nil {
		return GCounterView{Value: total}
	}
	if errors.Is(err, counter.ErrCounterOverflow) {
		return GCounterView{Overflow: true}
	}
	panic(err)
}

func pnCounterView(value *counter.PNCounter) PNCounterView {
	total, err := value.Value()
	if err != nil {
		panic(err)
	}
	return PNCounterView{Value: total.String()}
}
