package awareness

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"sync"
	"testing"
	"time"
)

func TestStoreCanonicalizesStatesAndReturnsCopies(t *testing.T) {
	store := mustStore(t, DefaultOptions())
	now := time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC)
	update, err := store.Set("alice", []byte(`{ "name":"Alice", "cursor":{"offset":3} }`), now)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(update.State), `{"cursor":{"offset":3},"name":"Alice"}`; got != want {
		t.Fatalf("canonical state = %s, want %s", got, want)
	}
	update.State[0] = '!'
	active := store.ActiveAt(now)
	if len(active) != 1 || string(active[0].State) != `{"cursor":{"offset":3},"name":"Alice"}` {
		t.Fatalf("active = %#v", active)
	}
	active[0].State[0] = '!'
	if got := string(store.ActiveAt(now)[0].State); got != `{"cursor":{"offset":3},"name":"Alice"}` {
		t.Fatalf("returned state mutated store: %s", got)
	}
}

func TestStoreRejectsEqualClockConflictsAndRetainsRemovalTombstones(t *testing.T) {
	store := mustStore(t, DefaultOptions())
	now := time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC)
	if changed, err := store.Apply(Update{Actor: "alice", Clock: 4, State: []byte(`{"cursor":1}`)}, now); err != nil || !changed {
		t.Fatalf("apply online = %t, %v", changed, err)
	}
	if changed, err := store.Apply(Update{Actor: "alice", Clock: 4, State: []byte(`{"cursor":1}`)}, now); err != nil || changed {
		t.Fatalf("apply duplicate = %t, %v", changed, err)
	}
	if _, err := store.Apply(Update{Actor: "alice", Clock: 4, State: []byte(`{"cursor":2}`)}, now); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("equal clock conflict = %v, want %v", err, ErrStateConflict)
	}
	if changed, err := store.Apply(Update{Actor: "alice", Clock: 5}, now); err != nil || !changed {
		t.Fatalf("apply removal = %t, %v", changed, err)
	}
	if active := store.ActiveAt(now); len(active) != 0 {
		t.Fatalf("active after removal = %#v", active)
	}
	if changed, err := store.Apply(Update{Actor: "alice", Clock: 4, State: []byte(`{"cursor":1}`)}, now); err != nil || changed {
		t.Fatalf("stale revival = %t, %v", changed, err)
	}
	if update, err := store.Set("alice", []byte(`{"cursor":6}`), now); err != nil || update.Clock != 6 {
		t.Fatalf("set after removal = %#v, %v", update, err)
	}
}

func TestStoreExpiryNeedsNewerHeartbeat(t *testing.T) {
	options := DefaultOptions()
	options.Timeout = 5 * time.Second
	store := mustStore(t, options)
	now := time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC)
	if _, err := store.Set("alice", []byte(`{"cursor":1}`), now); err != nil {
		t.Fatal(err)
	}
	if active := store.ActiveAt(now.Add(5 * time.Second)); len(active) != 1 {
		t.Fatalf("active at exact timeout = %#v", active)
	}
	if active := store.ActiveAt(now.Add(5*time.Second + time.Nanosecond)); len(active) != 0 {
		t.Fatalf("expired state = %#v", active)
	}
	if _, err := store.Set("alice", []byte(`{"cursor":1}`), now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	if active := store.ActiveAt(now.Add(10 * time.Second)); len(active) != 1 || active[0].Clock != 2 {
		t.Fatalf("heartbeat active = %#v", active)
	}
}

func TestStoreHeartbeatReusesOnlineStateAndRejectsOfflineActors(t *testing.T) {
	options := DefaultOptions()
	options.Timeout = time.Second
	store := mustStore(t, options)
	now := time.Date(2026, time.July, 31, 11, 0, 0, 0, time.UTC)
	if _, err := store.Heartbeat("alice", now); !errors.Is(err, ErrOfflineActor) {
		t.Fatalf("unknown Heartbeat = %v, want %v", err, ErrOfflineActor)
	}
	first, err := store.Set("alice", []byte(`{ "name":"Alice", "cursor":4 }`), now)
	if err != nil {
		t.Fatal(err)
	}
	if first.Clock != 1 || string(first.State) != `{"cursor":4,"name":"Alice"}` {
		t.Fatalf("initial state = %#v", first)
	}
	second, err := store.Heartbeat("alice", now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if second.Clock != 2 || string(second.State) != string(first.State) {
		t.Fatalf("heartbeat = %#v, want next canonical state", second)
	}
	second.State[0] = '!'
	active := store.ActiveAt(now.Add(2 * time.Second))
	if len(active) != 1 || active[0].Clock != 2 || string(active[0].State) != string(first.State) {
		t.Fatalf("heartbeat mutated retained state = %#v", active)
	}
	if removed, err := store.Remove("alice", now.Add(3*time.Second)); err != nil || removed.Clock != 3 {
		t.Fatalf("Remove = %#v, %v", removed, err)
	}
	if _, err := store.Heartbeat("alice", now.Add(4*time.Second)); !errors.Is(err, ErrOfflineActor) {
		t.Fatalf("removed Heartbeat = %v, want %v", err, ErrOfflineActor)
	}
	if _, err := store.Heartbeat(" ", now); !errors.Is(err, ErrInvalidActor) {
		t.Fatalf("invalid actor Heartbeat = %v, want %v", err, ErrInvalidActor)
	}
}

func TestUpdateWireRoundTripAndRejectsInvalidInput(t *testing.T) {
	options := DefaultOptions()
	input := Update{Actor: "alice", Clock: 7, State: []byte(` {"name":"Alice","cursor":4} `)}
	encoded, err := input.MarshalBinaryWithOptions(options)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalUpdate(encoded, options)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Actor != "alice" || decoded.Clock != 7 || string(decoded.State) != `{"cursor":4,"name":"Alice"}` {
		t.Fatalf("round trip = %#v", decoded)
	}
	for _, malformed := range [][]byte{
		nil,
		append(append([]byte(nil), encoded...), 0),
		{protocolVersion, 0x81, 0, 'a', 1, 0},
	} {
		if _, err := UnmarshalUpdate(malformed, options); !errors.Is(err, ErrInvalidUpdate) {
			t.Fatalf("UnmarshalUpdate(%x) = %v, want ErrInvalidUpdate", malformed, err)
		}
	}
	if _, err := UnmarshalUpdate([]byte{protocolVersion, 0, 1, 0}, options); !errors.Is(err, ErrInvalidActor) {
		t.Fatalf("empty actor = %v, want %v", err, ErrInvalidActor)
	}
	if _, err := Normalize(Update{Actor: "alice", Clock: 1, State: []byte(`[]`)}, options); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("array state = %v, want %v", err, ErrInvalidState)
	}
}

func TestThreeEditorSimulationConvergesWithDuplicatesAndReordering(t *testing.T) {
	now := time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC)
	source := mustStore(t, DefaultOptions())
	updates := make([]Update, 0, 4)
	for _, item := range []struct {
		actor string
		state string
	}{
		{"alice", `{"cursor":{"anchor":"a:9","association":"before"},"name":"Alice"}`},
		{"bob", `{"cursor":{"anchor":"b:4","association":"after"},"name":"Bob"}`},
		{"carol", `{"cursor":{"anchor":"c:2","association":"before"},"name":"Carol"}`},
		{"alice", `{"cursor":{"anchor":"a:10","association":"before"},"name":"Alice"}`},
	} {
		update, err := source.Set(item.actor, []byte(item.state), now)
		if err != nil {
			t.Fatal(err)
		}
		updates = append(updates, update)
	}
	for replicaIndex := 0; replicaIndex < 3; replicaIndex++ {
		replica := mustStore(t, DefaultOptions())
		delivery := append(append([]Update(nil), updates...), updates...)
		random := rand.New(rand.NewSource(int64(20260730 + replicaIndex)))
		random.Shuffle(len(delivery), func(left, right int) { delivery[left], delivery[right] = delivery[right], delivery[left] })
		for _, update := range delivery {
			if _, err := replica.Apply(update, now); err != nil {
				t.Fatal(err)
			}
		}
		active := replica.ActiveAt(now)
		if len(active) != 3 || active[0].Actor != "alice" || active[0].Clock != 2 || active[2].Actor != "carol" {
			t.Fatalf("replica %d active = %#v", replicaIndex, active)
		}
	}
}

func TestStoreConcurrentUpdates(t *testing.T) {
	store := mustStore(t, DefaultOptions())
	now := time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC)
	const actors = 64
	var group sync.WaitGroup
	group.Add(actors)
	for index := 0; index < actors; index++ {
		go func(index int) {
			defer group.Done()
			actor := string(rune('a'+index%26)) + string(rune('A'+index/26))
			for clock := 0; clock < 16; clock++ {
				if _, err := store.Set(actor, []byte(`{"cursor":1}`), now); err != nil {
					t.Errorf("Set(%s) = %v", actor, err)
					return
				}
			}
		}(index)
	}
	group.Wait()
	if active := store.ActiveAt(now); len(active) != actors {
		t.Fatalf("active actors = %d, want %d", len(active), actors)
	}
}

func TestStoreAndUpdateBoundaries(t *testing.T) {
	if _, err := NewStore(Options{}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("NewStore zero options error = %v", err)
	}
	options := DefaultOptions()
	options.MaxActors = 1
	store := mustStore(t, options)
	if got := store.Options(); got != options {
		t.Fatalf("Options = %#v, want %#v", got, options)
	}
	var nilStore *Store
	if _, err := nilStore.Set("alice", []byte(`{}`), time.Time{}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("nil Set error = %v", err)
	}
	if _, err := nilStore.Heartbeat("alice", time.Time{}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("nil Heartbeat error = %v", err)
	}
	if active := nilStore.ActiveAt(time.Time{}); active != nil {
		t.Fatalf("nil ActiveAt = %#v", active)
	}
	if got := nilStore.Options(); got != (Options{}) {
		t.Fatalf("nil Options = %#v", got)
	}
	if _, err := store.Set("alice", []byte(`{}`), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if removed, err := store.Remove("alice", time.Time{}); err != nil || removed.Online() || removed.Clock != 2 {
		t.Fatalf("Remove = %#v, %v", removed, err)
	}
	if _, err := store.Apply(Update{Actor: "bob", Clock: 1, State: []byte(`{}`)}, time.Time{}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("actor limit error = %v", err)
	}
	store.records["alice"] = record{update: Update{Actor: "alice", Clock: math.MaxUint64}}
	if _, err := store.Set("alice", []byte(`{}`), time.Time{}); !errors.Is(err, ErrClockExhausted) {
		t.Fatalf("clock exhaustion error = %v", err)
	}

	for _, update := range []Update{
		{Actor: "alice", Clock: 0},
		{Actor: "\xff", Clock: 1},
		{Actor: "alice", Clock: 1, State: []byte(`[]`)},
		{Actor: "alice", Clock: 1, State: []byte(`{} trailing`)},
		{Actor: "alice", Clock: 1, State: make([]byte, options.MaxStateBytes+1)},
	} {
		if _, err := Normalize(update, options); err == nil {
			t.Fatalf("Normalize(%#v) succeeded", update)
		}
	}
	if _, err := UnmarshalUpdate([]byte{protocolVersion, 1, 'a', 1, 2}, options); !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("invalid wire status error = %v", err)
	}
	if _, err := UnmarshalUpdate([]byte{protocolVersion, 1, 'a', 1, 0}, Options{}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("invalid options decode error = %v", err)
	}
}

func TestStoreSubscriptionObservesSnapshotsAndExpiry(t *testing.T) {
	options := DefaultOptions()
	options.Timeout = time.Second
	store := mustStore(t, options)
	now := time.Date(2026, time.July, 31, 10, 0, 0, 0, time.UTC)
	events := make(chan Event, 4)
	subscription, err := store.SubscribeAt(now, func(event Event) {
		events <- event
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		subscription.Unsubscribe()
		<-subscription.Done()
	}()

	initial := awaitEvent(t, events)
	if initial.Origin != Initial || initial.Version != 0 || len(initial.Active) != 0 {
		t.Fatalf("initial event = %#v", initial)
	}
	if _, err := store.Set("alice", []byte(`{"cursor":1}`), now); err != nil {
		t.Fatal(err)
	}
	local := awaitEvent(t, events)
	if local.Origin != Local || local.Version != 1 || local.Update.Actor != "alice" || len(local.Active) != 1 {
		t.Fatalf("local event = %#v", local)
	}
	local.Active[0].State[0] = '!'
	if active := store.ActiveAt(now); string(active[0].State) != `{"cursor":1}` {
		t.Fatalf("event mutated store state: %#v", active)
	}
	if changed, err := store.Apply(Update{Actor: "bob", Clock: 1, State: []byte(`{"cursor":2}`)}, now); err != nil || !changed {
		t.Fatalf("apply bob = %t, %v", changed, err)
	}
	remote := awaitEvent(t, events)
	if remote.Origin != Remote || remote.Version != 2 || len(remote.Active) != 2 || remote.Active[1].Actor != "bob" {
		t.Fatalf("remote event = %#v", remote)
	}
	if !store.Expire(now.Add(time.Second + time.Nanosecond)) {
		t.Fatal("Expire did not report liveness transition")
	}
	expired := awaitEvent(t, events)
	if expired.Origin != Expired || expired.Version != 3 || len(expired.Active) != 0 {
		t.Fatalf("expired event = %#v", expired)
	}
	if store.Expire(now.Add(2 * time.Second)) {
		t.Fatal("second expiry reported a duplicate transition")
	}
	if changed, err := store.Apply(Update{Actor: "alice", Clock: 2, State: []byte(`{"cursor":3}`)}, now.Add(2*time.Second)); err != nil || !changed {
		t.Fatalf("newer heartbeat = %t, %v", changed, err)
	}
	revived := awaitEvent(t, events)
	if revived.Origin != Remote || revived.Version != 4 || len(revived.Active) != 1 || revived.Active[0].Actor != "alice" {
		t.Fatalf("revived event = %#v", revived)
	}
}

func TestStoreSubscriptionCoalescesAndBoundsListeners(t *testing.T) {
	options := DefaultOptions()
	options.MaxSubscribers = 1
	store := mustStore(t, options)
	now := time.Date(2026, time.July, 31, 10, 0, 0, 0, time.UTC)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	events := make(chan Event, 2)
	subscription, err := store.SubscribeAt(now, func(event Event) {
		entered <- struct{}{}
		<-release
		events <- event
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		subscription.Unsubscribe()
		select {
		case <-release:
		default:
			close(release)
		}
		<-subscription.Done()
	}()
	if _, err := store.SubscribeAt(now, func(Event) {}); !errors.Is(err, ErrSubscriptionLimit) {
		t.Fatalf("second subscription error = %v", err)
	}
	<-entered
	if _, err := store.Set("alice", []byte(`{}`), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Set("bob", []byte(`{}`), now); err != nil {
		t.Fatal(err)
	}
	close(release)
	first := awaitEvent(t, events)
	second := awaitEvent(t, events)
	if first.Origin != Initial || second.Origin != Local || second.Version != 2 || second.Coalesced != 1 || len(second.Active) != 2 {
		t.Fatalf("coalesced events = %#v, %#v", first, second)
	}
}

func TestStoreSubscriptionLifecycleEdges(t *testing.T) {
	var nilStore *Store
	if subscription, err := nilStore.Subscribe(func(Event) {}); subscription != nil || !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("nil Subscribe = %#v, %v", subscription, err)
	}
	if nilStore.Expire(time.Time{}) {
		t.Fatal("nil Expire reported a transition")
	}
	var nilSubscription *Subscription
	select {
	case <-nilSubscription.Done():
	default:
		t.Fatal("nil subscription Done did not close")
	}
	nilSubscription.Unsubscribe()

	store := mustStore(t, DefaultOptions())
	if subscription, err := store.SubscribeAt(time.Now(), nil); subscription != nil || !errors.Is(err, ErrNilCallback) {
		t.Fatalf("nil callback = %#v, %v", subscription, err)
	}
	initial := make(chan Event, 1)
	subscription, err := store.Subscribe(func(event Event) { initial <- event })
	if err != nil {
		t.Fatal(err)
	}
	if event := awaitEvent(t, initial); event.Origin != Initial {
		t.Fatalf("Subscribe initial event = %#v", event)
	}
	subscription.Unsubscribe()
	<-subscription.Done()

	panicked := make(chan struct{})
	broken, err := store.SubscribeAt(time.Now(), func(Event) {
		close(panicked)
		panic("render failed")
	})
	if err != nil {
		t.Fatal(err)
	}
	<-panicked
	select {
	case <-broken.Done():
	case <-time.After(time.Second):
		t.Fatal("panicking callback did not stop")
	}
}

func TestStoreStartExpiryPublishesAndStopsWithContext(t *testing.T) {
	options := DefaultOptions()
	options.Timeout = time.Millisecond
	store := mustStore(t, options)
	staleAt := time.Now().Add(-time.Second)
	if _, err := store.Set("alice", []byte(`{"cursor":1}`), staleAt); err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 2)
	subscription, err := store.Subscribe(func(event Event) { events <- event })
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		subscription.Unsubscribe()
		<-subscription.Done()
	}()
	if event := awaitEvent(t, events); event.Origin != Initial || len(event.Active) != 0 {
		t.Fatalf("initial stale event = %#v", event)
	}
	ctx, cancel := context.WithCancel(context.Background())
	loop, err := store.StartExpiry(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if event := awaitEvent(t, events); event.Origin != Expired || event.Version != 2 || len(event.Active) != 0 {
		t.Fatalf("scheduled expiry event = %#v", event)
	}
	cancel()
	select {
	case <-loop.Done():
	case <-time.After(time.Second):
		t.Fatal("expiry loop did not stop after context cancellation")
	}
	if store.Expire(time.Now()) {
		t.Fatal("expiry scheduler left an unmarked stale record")
	}
}

func TestStoreStartExpiryRejectsInvalidLifecycle(t *testing.T) {
	store := mustStore(t, DefaultOptions())
	var nilContext context.Context
	if loop, err := store.StartExpiry(nilContext, time.Second); loop != nil || !errors.Is(err, ErrNilContext) {
		t.Fatalf("nil context = %#v, %v", loop, err)
	}
	if loop, err := store.StartExpiry(context.Background(), 0); loop != nil || !errors.Is(err, ErrInvalidExpiryInterval) {
		t.Fatalf("zero interval = %#v, %v", loop, err)
	}
	var nilStore *Store
	if loop, err := nilStore.StartExpiry(context.Background(), time.Second); loop != nil || !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("nil store = %#v, %v", loop, err)
	}
	var nilLoop *ExpiryLoop
	select {
	case <-nilLoop.Done():
	default:
		t.Fatal("nil expiry loop Done did not close")
	}
}

func awaitEvent(t testing.TB, events <-chan Event) Event {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for awareness event")
		return Event{}
	}
}

func mustStore(t testing.TB, options Options) *Store {
	t.Helper()
	store, err := NewStore(options)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
