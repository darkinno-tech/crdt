package observe_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/im10furry/crdt/counter"
	"github.com/im10furry/crdt/observe"
)

func TestGCounterObserverPublishesOnlyStateExtendingRemoteDeltas(t *testing.T) {
	alice := mustGCounterObserver(t, "alice")
	bob := mustGCounterObserver(t, "bob")
	events := make(chan observe.Event[observe.GCounterView], 4)
	subscription, err := bob.Subscribe(func(event observe.Event[observe.GCounterView]) { events <- event })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		subscription.Unsubscribe()
		awaitCounterSubscription(t, subscription.Done())
	})

	initial := nextGCounterEvent(t, events)
	if initial.Origin != observe.Initial || initial.Version != 0 || initial.Value != (observe.GCounterView{}) {
		t.Fatalf("initial event = %+v", initial)
	}

	local, err := alice.Increment(7)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := local.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	received, err := counter.UnmarshalGCounterDelta(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := bob.ApplyDelta(received); err != nil || !changed {
		t.Fatalf("ApplyDelta(first) = %t, %v, want changed", changed, err)
	}
	remote := nextGCounterEvent(t, events)
	if remote.Origin != observe.Remote || remote.Version != 1 || remote.Value != (observe.GCounterView{Value: 7}) {
		t.Fatalf("remote event = %+v", remote)
	}
	if changed, err := bob.ApplyDelta(received); err != nil || changed {
		t.Fatalf("ApplyDelta(duplicate) = %t, %v, want unchanged", changed, err)
	}
	select {
	case event := <-events:
		t.Fatalf("duplicate published event: %+v", event)
	case <-time.After(25 * time.Millisecond):
	}
	snapshot, err := bob.Snapshot()
	if err != nil || snapshot.Version != 1 || snapshot.Value != (observe.GCounterView{Value: 7}) {
		t.Fatalf("Snapshot() = %+v, %v", snapshot, err)
	}

	bob.Close()
	if _, err := bob.ApplyDelta(received); !errors.Is(err, observe.ErrClosed) {
		t.Fatalf("ApplyDelta() after Close = %v, want %v", err, observe.ErrClosed)
	}
}

func TestPNCounterObserversConvergeAfterReorderedDuplicateDeltas(t *testing.T) {
	alice := mustPNCounterObserver(t, "alice")
	bob := mustPNCounterObserver(t, "bob")
	carol := mustPNCounterObserver(t, "carol")
	events := make(chan observe.Event[observe.PNCounterView], 8)
	subscription, err := carol.Subscribe(func(event observe.Event[observe.PNCounterView]) { events <- event })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		subscription.Unsubscribe()
		awaitCounterSubscription(t, subscription.Done())
	})
	if event := nextPNCounterEvent(t, events); event.Origin != observe.Initial || event.Value.Value != "0" {
		t.Fatalf("initial event = %+v", event)
	}

	changes := make([]counter.PNCounterDelta, 0, 4)
	for _, change := range []struct {
		observer *observe.PNCounterObserver
		amount   uint64
		positive bool
	}{
		{alice, 9, true},
		{alice, 2, false},
		{bob, 4, true},
		{bob, 5, false},
	} {
		var delta counter.PNCounterDelta
		if change.positive {
			delta, err = change.observer.Increment(change.amount)
		} else {
			delta, err = change.observer.Decrement(change.amount)
		}
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := delta.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := counter.UnmarshalPNCounterDelta(encoded)
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, decoded)
	}

	for _, target := range []*observe.PNCounterObserver{alice, bob, carol} {
		for index := len(changes) - 1; index >= 0; index-- {
			if _, err := target.ApplyDelta(changes[index]); err != nil {
				t.Fatal(err)
			}
		}
		for _, delta := range changes {
			if changed, err := target.ApplyDelta(delta); err != nil || changed {
				t.Fatalf("duplicate ApplyDelta = %t, %v, want unchanged", changed, err)
			}
		}
		snapshot, err := target.Snapshot()
		if err != nil || snapshot.Value.Value != "6" {
			t.Fatalf("Snapshot() = %+v, %v, want 6", snapshot, err)
		}
	}
}

func TestCounterObserverLifecycleAndAggregateOverflow(t *testing.T) {
	if _, err := observe.NewGCounterObserver(""); !errors.Is(err, counter.ErrInvalidReplicaID) {
		t.Fatalf("NewGCounterObserver(invalid) = %v", err)
	}
	if _, err := observe.NewPNCounterObserver(""); !errors.Is(err, counter.ErrInvalidReplicaID) {
		t.Fatalf("NewPNCounterObserver(invalid) = %v", err)
	}
	var nilG *observe.GCounterObserver
	if _, err := nilG.Increment(1); !errors.Is(err, observe.ErrNilStore) {
		t.Fatalf("nil G Increment = %v", err)
	}
	if _, err := nilG.ApplyDelta(counter.GCounterDelta{}); !errors.Is(err, observe.ErrNilStore) {
		t.Fatalf("nil G ApplyDelta = %v", err)
	}
	if _, err := nilG.Snapshot(); !errors.Is(err, observe.ErrNilStore) {
		t.Fatalf("nil G Snapshot = %v", err)
	}
	if _, err := nilG.Subscribe(nil); !errors.Is(err, observe.ErrNilStore) {
		t.Fatalf("nil G Subscribe = %v", err)
	}
	if _, err := nilG.SubscribeFromNow(nil); !errors.Is(err, observe.ErrNilStore) {
		t.Fatalf("nil G SubscribeFromNow = %v", err)
	}
	nilG.Close()

	target := mustGCounterObserver(t, "target")
	first := mustGCounterObserver(t, "first")
	second := mustGCounterObserver(t, "second")
	firstDelta, err := first.Increment(math.MaxUint64)
	if err != nil {
		t.Fatal(err)
	}
	secondDelta, err := second.Increment(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []counter.GCounterDelta{firstDelta, secondDelta} {
		if changed, err := target.ApplyDelta(delta); err != nil || !changed {
			t.Fatalf("overflow setup ApplyDelta = %t, %v", changed, err)
		}
	}
	snapshot, err := target.Snapshot()
	if err != nil || !snapshot.Value.Overflow || snapshot.Value.Value != 0 {
		t.Fatalf("overflow Snapshot() = %+v, %v", snapshot, err)
	}

	pn := mustPNCounterObserver(t, "pn")
	subscription, err := pn.SubscribeFromNow(func(observe.Event[observe.PNCounterView]) {})
	if err != nil {
		t.Fatal(err)
	}
	subscription.Unsubscribe()
	awaitCounterSubscription(t, subscription.Done())
	pn.Close()
	if _, err := pn.Decrement(1); !errors.Is(err, observe.ErrClosed) {
		t.Fatalf("Decrement after Close = %v, want %v", err, observe.ErrClosed)
	}
	var nilPN *observe.PNCounterObserver
	if _, err := nilPN.Increment(1); !errors.Is(err, observe.ErrNilStore) {
		t.Fatalf("nil PN Increment = %v", err)
	}
	if _, err := nilPN.ApplyDelta(counter.PNCounterDelta{}); !errors.Is(err, observe.ErrNilStore) {
		t.Fatalf("nil PN ApplyDelta = %v", err)
	}
	if _, err := nilPN.Snapshot(); !errors.Is(err, observe.ErrNilStore) {
		t.Fatalf("nil PN Snapshot = %v", err)
	}
	if _, err := nilPN.Subscribe(nil); !errors.Is(err, observe.ErrNilStore) {
		t.Fatalf("nil PN Subscribe = %v", err)
	}
	if _, err := nilPN.SubscribeFromNow(nil); !errors.Is(err, observe.ErrNilStore) {
		t.Fatalf("nil PN SubscribeFromNow = %v", err)
	}
	nilPN.Close()
}

func mustGCounterObserver(t testing.TB, replicaID string) *observe.GCounterObserver {
	t.Helper()
	value, err := observe.NewGCounterObserver(replicaID)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustPNCounterObserver(t testing.TB, replicaID string) *observe.PNCounterObserver {
	t.Helper()
	value, err := observe.NewPNCounterObserver(replicaID)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func nextGCounterEvent(t testing.TB, events <-chan observe.Event[observe.GCounterView]) observe.Event[observe.GCounterView] {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for G-Counter event")
		return observe.Event[observe.GCounterView]{}
	}
}

func nextPNCounterEvent(t testing.TB, events <-chan observe.Event[observe.PNCounterView]) observe.Event[observe.PNCounterView] {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for PN-Counter event")
		return observe.Event[observe.PNCounterView]{}
	}
}

func awaitCounterSubscription(t testing.TB, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for counter subscription shutdown")
	}
}
