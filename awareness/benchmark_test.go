package awareness

import (
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
