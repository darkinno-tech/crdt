package awareness

import (
	"fmt"
	"testing"
	"time"
)

func BenchmarkStoreApplyHeartbeat(b *testing.B) {
	store := mustStore(b, DefaultOptions())
	now := time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		_, err := store.Apply(Update{Actor: "alice", Clock: uint64(index + 1), State: []byte(`{"cursor":{"anchor":"actor:2048","association":"before"},"name":"Alice"}`)}, now)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStoreHeartbeat(b *testing.B) {
	store := mustStore(b, DefaultOptions())
	now := time.Date(2026, time.July, 31, 11, 0, 0, 0, time.UTC)
	if _, err := store.Set("alice", []byte(`{"cursor":{"anchor":"actor:2048","association":"before"},"name":"Alice"}`), now); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := store.Heartbeat("alice", now); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStoreSetUnchangedState(b *testing.B) {
	store := mustStore(b, DefaultOptions())
	now := time.Date(2026, time.July, 31, 11, 0, 0, 0, time.UTC)
	state := []byte(`{"cursor":{"anchor":"actor:2048","association":"before"},"name":"Alice"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := store.Set("alice", state, now); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStoreExpireNoTransition(b *testing.B) {
	for _, actorCount := range []int{1, 64, 1024} {
		b.Run(fmt.Sprintf("actors_%d", actorCount), func(b *testing.B) {
			store := mustStore(b, DefaultOptions())
			now := time.Date(2026, time.July, 31, 11, 0, 0, 0, time.UTC)
			for index := 0; index < actorCount; index++ {
				if _, err := store.Set(fmt.Sprintf("actor-%d", index), []byte(`{"cursor":1}`), now); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if store.Expire(now) {
					b.Fatal("fresh state expired")
				}
			}
		})
	}
}

func BenchmarkUpdateDecodeAndApply(b *testing.B) {
	encoded, err := (Update{Actor: "alice", Clock: 1, State: []byte(`{"cursor":{"anchor":"actor:2048","association":"before"},"name":"Alice"}`)}).MarshalBinary()
	if err != nil {
		b.Fatal(err)
	}
	store := mustStore(b, DefaultOptions())
	now := time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		decoded, err := UnmarshalUpdate(encoded, DefaultOptions())
		if err != nil {
			b.Fatal(err)
		}
		decoded.Clock = uint64(index + 1)
		if _, err := store.Apply(decoded, now); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStoreApplyHeartbeatWithObservers(b *testing.B) {
	options := DefaultOptions()
	options.MaxSubscribers = 64
	store := mustStore(b, options)
	subscriptions := make([]*Subscription, 0, options.MaxSubscribers)
	for index := 0; index < options.MaxSubscribers; index++ {
		subscription, err := store.Subscribe(func(Event) {})
		if err != nil {
			b.Fatal(err)
		}
		subscriptions = append(subscriptions, subscription)
	}
	b.Cleanup(func() {
		for _, subscription := range subscriptions {
			subscription.Unsubscribe()
		}
		for _, subscription := range subscriptions {
			<-subscription.Done()
		}
	})
	now := time.Date(2026, time.July, 31, 10, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		_, err := store.Apply(Update{Actor: "alice", Clock: uint64(index + 1), State: []byte(`{"cursor":{"anchor":"actor:2048","association":"before"},"name":"Alice"}`)}, now)
		if err != nil {
			b.Fatal(err)
		}
	}
}
