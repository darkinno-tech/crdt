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
