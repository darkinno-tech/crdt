package redis

import (
	"testing"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/replica"
)

// BenchmarkStoreAppendLoopback measures the provider's canonical envelope,
// Lua transaction, and loopback Redis client. It is a controlled lower-latency
// baseline, not an AOF/TLS/cluster or production durability claim.
func BenchmarkStoreAppendLoopback(b *testing.B) {
	store, cleanup := testStore(b, uint64(b.N)+1, uint64(b.N+1)*1024)
	defer cleanup()
	manifest := benchmarkManifest(b)
	changes := make([]replica.Change, b.N)
	for index := range changes {
		changes[index] = testChange(b, manifest, "writer", uint64(index+1), uint64(index+1))
	}
	b.ResetTimer()
	for index := range changes {
		if _, err := store.Append(manifest.GroupID, changes[index]); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStoreReplayLoopback(b *testing.B) {
	store, cleanup := testStore(b, 256, 1<<20)
	defer cleanup()
	manifest := benchmarkManifest(b)
	for index := 0; index < 256; index++ {
		if _, err := store.Append(manifest.GroupID, testChange(b, manifest, "writer", uint64(index+1), uint64(index+1))); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, _, err := store.Replay(manifest.GroupID, 0, 256, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkManifest(b testing.TB) replica.Manifest {
	b.Helper()
	return testManifest(b)
}
