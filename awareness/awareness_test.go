package awareness

import (
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

func mustStore(t testing.TB, options Options) *Store {
	t.Helper()
	store, err := NewStore(options)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
