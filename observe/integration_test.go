package observe_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/im10furry/crdt/counter"
	"github.com/im10furry/crdt/observe"
)

func TestGCounterReplicaSimulationDrivesLatestUIState(t *testing.T) {
	left, leftStore, leftView := newObservedCounter(t, "left")
	right, rightStore, rightView := newObservedCounter(t, "right")
	mobile, mobileStore, mobileView := newObservedCounter(t, "mobile")

	leftDelta := incrementObserved(t, leftStore, 2)
	rightDelta := incrementObserved(t, rightStore, 3)
	mobileDelta := incrementObserved(t, mobileStore, 5)

	// Model a short partition. Each replica receives the other replicas' delta
	// in a different order, and left receives a duplicate. Applying a remote
	// CRDT delta remains idempotent while the Store gives the UI a local event.
	applyRemote(t, mobileStore, leftDelta)
	applyRemote(t, leftStore, mobileDelta)
	applyRemote(t, rightStore, leftDelta)
	applyRemote(t, mobileStore, rightDelta)
	applyRemote(t, leftStore, rightDelta)
	applyRemote(t, rightStore, mobileDelta)
	applyRemote(t, leftStore, rightDelta)

	for name, replica := range map[string]*counter.GCounter{
		"left":   left,
		"right":  right,
		"mobile": mobile,
	} {
		value, err := replica.Value()
		if err != nil {
			t.Fatalf("%s Value() error = %v", name, err)
		}
		if value != 10 {
			t.Fatalf("%s converged value = %d, want 10", name, value)
		}
	}
	awaitRenderedValue(t, leftView, 10)
	awaitRenderedValue(t, rightView, 10)
	awaitRenderedValue(t, mobileView, 10)
}

func newObservedCounter(t *testing.T, replicaID string) (*counter.GCounter, *observe.Store[*counter.GCounter, uint64], *atomic.Uint64) {
	t.Helper()
	value, err := counter.NewGCounter(replicaID)
	if err != nil {
		t.Fatalf("NewGCounter(%q) error = %v", replicaID, err)
	}
	store, err := observe.New(value, func(current *counter.GCounter) uint64 {
		total, err := current.Value()
		if err != nil {
			panic(err)
		}
		return total
	})
	if err != nil {
		t.Fatalf("observe.New() error = %v", err)
	}
	var rendered atomic.Uint64
	subscription, err := store.Subscribe(func(event observe.Event[uint64]) {
		// A UI framework would schedule this assignment on its own render loop.
		// The event carries an immutable scalar value and never requires reading
		// CRDT internals from the callback.
		rendered.Store(event.Value)
	})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	t.Cleanup(func() {
		subscription.Unsubscribe()
		select {
		case <-subscription.Done():
		case <-time.After(time.Second):
			t.Fatal("timed out stopping UI subscription")
		}
	})
	return value, store, &rendered
}

func incrementObserved(t *testing.T, store *observe.Store[*counter.GCounter, uint64], amount uint64) counter.GCounterDelta {
	t.Helper()
	var delta counter.GCounterDelta
	if err := store.Mutate(observe.Local, func(value *counter.GCounter) error {
		var err error
		delta, err = value.Increment(amount)
		return err
	}); err != nil {
		t.Fatalf("local increment error = %v", err)
	}
	return delta
}

func applyRemote(t *testing.T, store *observe.Store[*counter.GCounter, uint64], delta counter.GCounterDelta) {
	t.Helper()
	if err := store.Mutate(observe.Remote, func(value *counter.GCounter) error {
		return value.ApplyDelta(delta)
	}); err != nil {
		t.Fatalf("remote ApplyDelta() error = %v", err)
	}
}

func awaitRenderedValue(t *testing.T, rendered *atomic.Uint64, want uint64) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if got := rendered.Load(); got == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("rendered value = %d, want %d", rendered.Load(), want)
		case <-ticker.C:
		}
	}
}
