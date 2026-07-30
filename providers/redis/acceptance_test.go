package redis

import (
	"os"
	"testing"

	"github.com/DarkInno/crdt"
	redisclient "github.com/redis/go-redis/v9"
)

// TestRedisStoreAcceptance exercises an externally running Redis only when a
// caller explicitly supplies CRDT_REDIS_ADDR. CI still covers the Lua contract
// with miniredis; this check verifies the selected real server separately.
func TestRedisStoreAcceptance(t *testing.T) {
	address := os.Getenv("CRDT_REDIS_ADDR")
	if address == "" {
		t.Skip("set CRDT_REDIS_ADDR to run Redis acceptance")
	}
	client := redisclient.NewClient(&redisclient.Options{Addr: address})
	defer func() { _ = client.Close() }()
	store, err := New(client, Config{Prefix: "crdt-acceptance", MaxEvents: 8, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	manifest := testManifest(t)
	change := testChange(t, manifest, "acceptance-redis", 1, 1)
	if _, err := store.Append(manifest.GroupID, change); err != nil {
		t.Fatal(err)
	}
	events, highWater, err := store.Replay(manifest.GroupID, 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
	if err != nil || highWater == 0 || len(events) == 0 {
		t.Fatalf("real Redis replay = high=%d events=%d err=%v", highWater, len(events), err)
	}
}
