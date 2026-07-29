package observe

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DarkInno/crdt"
)

var errRejectedMutation = errors.New("rejected mutation")

type testModel struct {
	value int
}

func (m *testModel) add(amount int) error {
	if amount < 0 {
		return errRejectedMutation
	}
	m.value += amount
	return nil
}

func (m *testModel) State() crdt.StateSnapshot {
	return crdt.StateSnapshot{Type: "test", ReplicaID: "test", ElementCount: m.value}
}

func newTestStore(t *testing.T) *Store[*testModel, int] {
	t.Helper()
	store, err := New(&testModel{}, func(model *testModel) int { return model.value })
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return store
}

func awaitEvent(t *testing.T, events <-chan Event[int]) Event[int] {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return Event[int]{}
	}
}

func awaitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for subscription shutdown")
	}
}

func TestStoreDeliversInitialMutationAndSnapshot(t *testing.T) {
	store := newTestStore(t)
	events := make(chan Event[int], 2)
	subscription, err := store.Subscribe(func(event Event[int]) { events <- event })
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	t.Cleanup(func() {
		subscription.Unsubscribe()
		awaitDone(t, subscription.Done())
	})

	initial := awaitEvent(t, events)
	if initial.Version != 0 || initial.Origin != Initial || initial.Value != 0 || initial.Coalesced != 0 {
		t.Fatalf("initial event = %+v", initial)
	}
	if err := store.Mutate(Local, func(model *testModel) error { return model.add(2) }); err != nil {
		t.Fatalf("Mutate() error = %v", err)
	}
	change := awaitEvent(t, events)
	if change.Version != 1 || change.Origin != Local || change.Value != 2 || change.Coalesced != 0 {
		t.Fatalf("mutation event = %+v", change)
	}
	if got, want := change.State.ElementCount, 2; got != want {
		t.Fatalf("state element count = %d, want %d", got, want)
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.Version != 1 || snapshot.Origin != Initial || snapshot.Value != 2 {
		t.Fatalf("Snapshot() = %+v", snapshot)
	}
}

func TestStoreFailedMutationDoesNotPublish(t *testing.T) {
	store := newTestStore(t)
	events := make(chan Event[int], 2)
	subscription, err := store.Subscribe(func(event Event[int]) { events <- event })
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	t.Cleanup(func() {
		subscription.Unsubscribe()
		awaitDone(t, subscription.Done())
	})
	_ = awaitEvent(t, events)

	if err := store.Mutate(Local, func(model *testModel) error { return model.add(-1) }); !errors.Is(err, errRejectedMutation) {
		t.Fatalf("Mutate() error = %v, want %v", err, errRejectedMutation)
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.Version != 0 || snapshot.Value != 0 {
		t.Fatalf("failed mutation snapshot = %+v", snapshot)
	}
	select {
	case event := <-events:
		t.Fatalf("failed mutation published %+v", event)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestStoreCoalescesSlowSubscriber(t *testing.T) {
	store := newTestStore(t)
	entered := make(chan Event[int], 1)
	release := make(chan struct{})
	delivered := make(chan Event[int], 1)
	released := false
	subscription, err := store.Subscribe(func(event Event[int]) {
		if event.Origin == Initial {
			entered <- event
			<-release
			return
		}
		delivered <- event
	})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		subscription.Unsubscribe()
		awaitDone(t, subscription.Done())
	})
	_ = awaitEvent(t, entered)

	for index := 0; index < 100; index++ {
		if err := store.Mutate(Local, func(model *testModel) error { return model.add(1) }); err != nil {
			t.Fatalf("Mutate(%d) error = %v", index, err)
		}
	}
	close(release)
	released = true
	event := awaitEvent(t, delivered)
	if event.Version != 100 || event.Value != 100 || event.Coalesced != 99 {
		t.Fatalf("coalesced event = %+v, want version/value/coalesced 100/100/99", event)
	}
}

func TestStoreCallbackCanMutateWithoutReordering(t *testing.T) {
	store := newTestStore(t)
	events := make(chan Event[int], 2)
	var once sync.Once
	subscription, err := store.Subscribe(func(event Event[int]) {
		if event.Origin == Initial {
			return
		}
		events <- event
		if event.Version == 1 {
			once.Do(func() {
				if err := store.Mutate(Local, func(model *testModel) error { return model.add(1) }); err != nil {
					t.Errorf("reentrant Mutate() error = %v", err)
				}
			})
		}
	})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	t.Cleanup(func() {
		subscription.Unsubscribe()
		awaitDone(t, subscription.Done())
	})

	if err := store.Mutate(Local, func(model *testModel) error { return model.add(1) }); err != nil {
		t.Fatalf("Mutate() error = %v", err)
	}
	first := awaitEvent(t, events)
	second := awaitEvent(t, events)
	if first.Version != 1 || first.Value != 1 || second.Version != 2 || second.Value != 2 {
		t.Fatalf("reentrant event order = %+v then %+v", first, second)
	}
}

func TestStoreContainsCallbackPanic(t *testing.T) {
	panicEvents := make(chan Panic, 1)
	store, err := NewWithOptions(&testModel{}, func(model *testModel) int { return model.value }, Options{
		OnPanic: func(info Panic) { panicEvents <- info },
	})
	if err != nil {
		t.Fatalf("NewWithOptions() error = %v", err)
	}
	bad, err := store.Subscribe(func(Event[int]) { panic("bad observer") })
	if err != nil {
		t.Fatalf("Subscribe(bad) error = %v", err)
	}
	goodEvents := make(chan Event[int], 2)
	good, err := store.Subscribe(func(event Event[int]) { goodEvents <- event })
	if err != nil {
		t.Fatalf("Subscribe(good) error = %v", err)
	}
	t.Cleanup(func() {
		good.Unsubscribe()
		awaitDone(t, good.Done())
	})
	_ = awaitEvent(t, goodEvents)
	awaitDone(t, bad.Done())
	select {
	case info := <-panicEvents:
		if info.EventVersion != 0 || info.Origin != Initial || info.Value != "bad observer" {
			t.Fatalf("panic info = %+v", info)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for panic hook")
	}
	if info, ok := bad.Panic(); !ok || info.Value != "bad observer" {
		t.Fatalf("Subscription.Panic() = %+v, %v", info, ok)
	}

	if err := store.Mutate(Local, func(model *testModel) error { return model.add(1) }); err != nil {
		t.Fatalf("Mutate() error = %v", err)
	}
	change := awaitEvent(t, goodEvents)
	if change.Version != 1 || change.Value != 1 {
		t.Fatalf("healthy observer event = %+v", change)
	}
}

func TestStoreSubscribeFromNowAndClose(t *testing.T) {
	store := newTestStore(t)
	events := make(chan Event[int], 1)
	subscription, err := store.SubscribeFromNow(func(event Event[int]) { events <- event })
	if err != nil {
		t.Fatalf("SubscribeFromNow() error = %v", err)
	}
	select {
	case event := <-events:
		t.Fatalf("SubscribeFromNow() delivered initial %+v", event)
	case <-time.After(25 * time.Millisecond):
	}
	if err := store.Mutate(Remote, func(model *testModel) error { return model.add(4) }); err != nil {
		t.Fatalf("Mutate() error = %v", err)
	}
	change := awaitEvent(t, events)
	if change.Origin != Remote || change.Version != 1 || change.Value != 4 {
		t.Fatalf("remote event = %+v", change)
	}

	store.Close()
	awaitDone(t, subscription.Done())
	if err := store.Mutate(Local, func(model *testModel) error { return model.add(1) }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Mutate() after Close = %v, want %v", err, ErrClosed)
	}
	if _, err := store.Subscribe(func(Event[int]) {}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Subscribe() after Close = %v, want %v", err, ErrClosed)
	}
}

func TestStoreConcurrentMutationsKeepVersionsMonotonic(t *testing.T) {
	store := newTestStore(t)
	const mutations = 256
	latest := make(chan struct{}, 1)
	var observedMu sync.Mutex
	lastVersion := uint64(0)
	subscription, err := store.Subscribe(func(event Event[int]) {
		if event.Origin == Initial {
			return
		}
		observedMu.Lock()
		if event.Version <= lastVersion {
			t.Errorf("event versions not monotonic: previous=%d current=%d", lastVersion, event.Version)
		}
		lastVersion = event.Version
		observedMu.Unlock()
		if event.Version == mutations {
			latest <- struct{}{}
		}
	})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	t.Cleanup(func() {
		subscription.Unsubscribe()
		awaitDone(t, subscription.Done())
	})

	var workers sync.WaitGroup
	for index := 0; index < mutations; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := store.Mutate(Local, func(model *testModel) error { return model.add(1) }); err != nil {
				t.Errorf("Mutate() error = %v", err)
			}
		}()
	}
	workers.Wait()
	select {
	case <-latest:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for final version")
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.Version != mutations || snapshot.Value != mutations {
		t.Fatalf("final snapshot = %+v", snapshot)
	}
}

func TestStoreValidationAndLifecycleEdges(t *testing.T) {
	for origin, want := range map[Origin]string{
		Initial:     "initial",
		Local:       "local",
		Remote:      "remote",
		Merge:       "merge",
		Restore:     "restore",
		Maintenance: "maintenance",
		Origin(99):  "invalid",
	} {
		if got := origin.String(); got != want {
			t.Errorf("Origin(%d).String() = %q, want %q", origin, got, want)
		}
	}
	if _, err := New[*testModel, int](&testModel{}, nil); !errors.Is(err, ErrNilView) {
		t.Fatalf("New(nil view) error = %v, want %v", err, ErrNilView)
	}

	var nilStore *Store[*testModel, int]
	if err := nilStore.Mutate(Local, func(*testModel) error { return nil }); !errors.Is(err, ErrNilStore) {
		t.Fatalf("nil Store Mutate() error = %v, want %v", err, ErrNilStore)
	}
	if _, err := nilStore.Snapshot(); !errors.Is(err, ErrNilStore) {
		t.Fatalf("nil Store Snapshot() error = %v, want %v", err, ErrNilStore)
	}
	if _, err := nilStore.Subscribe(func(Event[int]) {}); !errors.Is(err, ErrNilStore) {
		t.Fatalf("nil Store Subscribe() error = %v, want %v", err, ErrNilStore)
	}
	var nilSubscription *Subscription[int]
	nilSubscription.Unsubscribe()
	awaitDone(t, nilSubscription.Done())
	if info, ok := nilSubscription.Panic(); ok || info != (Panic{}) {
		t.Fatalf("nil Subscription Panic() = %+v, %v", info, ok)
	}

	store := newTestStore(t)
	if err := store.Mutate(Initial, func(*testModel) error { return nil }); !errors.Is(err, ErrInvalidOrigin) {
		t.Fatalf("Mutate(Initial) error = %v, want %v", err, ErrInvalidOrigin)
	}
	if err := store.Mutate(Origin(99), func(*testModel) error { return nil }); !errors.Is(err, ErrInvalidOrigin) {
		t.Fatalf("Mutate(invalid) error = %v, want %v", err, ErrInvalidOrigin)
	}
	if err := store.Mutate(Local, nil); !errors.Is(err, ErrNilMutation) {
		t.Fatalf("Mutate(nil) error = %v, want %v", err, ErrNilMutation)
	}
	if _, err := store.Subscribe(nil); !errors.Is(err, ErrNilCallback) {
		t.Fatalf("Subscribe(nil) error = %v, want %v", err, ErrNilCallback)
	}

	subscription, err := store.Subscribe(func(Event[int]) {})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if info, ok := subscription.Panic(); ok || info != (Panic{}) {
		t.Fatalf("healthy Subscription Panic() = %+v, %v", info, ok)
	}
	subscription.Unsubscribe()
	subscription.Unsubscribe()
	// A publication racing with cancellation is ignored after cancellation;
	// exercise the same stopped-mailbox path directly.
	subscription.subscriber.enqueue(Event[int]{Version: 1, Origin: Local, Value: 1})
	awaitDone(t, subscription.Done())

	store.Close()
	store.Close()
}

func TestStoreContainsPanicHandlerPanic(t *testing.T) {
	store, err := NewWithOptions(&testModel{}, func(model *testModel) int { return model.value }, Options{
		OnPanic: func(Panic) { panic("diagnostic handler panic") },
	})
	if err != nil {
		t.Fatalf("NewWithOptions() error = %v", err)
	}
	subscription, err := store.Subscribe(func(Event[int]) { panic("observer panic") })
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	awaitDone(t, subscription.Done())
	if info, ok := subscription.Panic(); !ok || info.Value != "observer panic" {
		t.Fatalf("Subscription.Panic() = %+v, %v", info, ok)
	}
}
