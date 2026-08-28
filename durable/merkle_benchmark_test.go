package durable

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/counter"
	"github.com/darkinno-tech/crdt/replica"
)

// BenchmarkMerkleInventoryReconcile measures the local, no-network part of a
// complete bounded inventory comparison. It intentionally covers equal and
// one-missing-leaf cases: a Merkle root avoids the inventory exchange for the
// former, while the latter exercises exact leaf selection without a state
// vector. It is a development baseline, not a production capacity claim.
func BenchmarkMerkleInventoryReconcile(b *testing.B) {
	for _, leafCount := range []int{256, 4096} {
		b.Run(fmt.Sprintf("leaves=%d/equal", leafCount), func(b *testing.B) {
			index, leaves := benchmarkMerkleIndex(b, leafCount, false)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				identities, err := index.Reconcile(leaves)
				if err != nil || len(identities) != 0 {
					b.Fatalf("equal reconcile identities=%d err=%v", len(identities), err)
				}
			}
		})
		b.Run(fmt.Sprintf("leaves=%d/missing-one", leafCount), func(b *testing.B) {
			index, leaves := benchmarkMerkleIndex(b, leafCount, true)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				identities, err := index.Reconcile(leaves)
				if err != nil || len(identities) != 1 {
					b.Fatalf("sparse reconcile identities=%d err=%v", len(identities), err)
				}
			}
		})
	}
}

// BenchmarkMerkleLoopbackSession uses the real WebSocket handler, bbolt log,
// client control frames, and bounded inventory/request exchange. Its callback
// updates an in-memory MerkleIndex only, so it isolates transport recovery
// overhead; BenchmarkDurableSameHostFanout remains the decoder/install
// end-to-end capacity baseline.
func BenchmarkMerkleLoopbackSession(b *testing.B) {
	for _, missingOne := range []bool{false, true} {
		name := "equal-root"
		if missingOne {
			name = "sparse-repair"
		}
		b.Run(name, func(b *testing.B) {
			manifest := durableBenchmarkManifest(b)
			store, events := benchmarkMerkleStore(b, manifest, 64)
			defer func() { _ = store.Close() }()
			handler, _ := durableBenchmarkHandler(b, store, manifest)
			server := httptest.NewServer(handler)
			defer server.Close()
			endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				index := NewMerkleIndex()
				last := len(events)
				if missingOne {
					last--
				}
				for _, event := range events[:last] {
					if err := index.Put(event); err != nil {
						b.Fatal(err)
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				completed := make(chan error, 1)
				client, err := NewReconnectClient(endpoint, manifest, ClientConfig{
					Header:          http.Header{"Authorization": []string{"Bearer benchmark"}},
					MerkleRoot:      index.Root,
					ReconcileMerkle: index.Reconcile,
					OnEvent: func(event Event) error {
						return index.Put(event)
					},
					OnMerkleCatchUp: func(MerkleBoundary) error {
						cancel()
						return nil
					},
				})
				if err != nil {
					cancel()
					b.Fatal(err)
				}
				go func() { completed <- client.Run(ctx) }()
				if err := <-completed; err != nil {
					cancel()
					b.Fatal(err)
				}
				cancel()
			}
		})
	}
}

func benchmarkMerkleIndex(b *testing.B, leafCount int, missingOne bool) (*MerkleIndex, []MerkleLeaf) {
	b.Helper()
	manifest := durableBenchmarkManifest(b)
	index := NewMerkleIndex()
	leaves := make([]MerkleLeaf, 0, leafCount)
	writer, err := counter.NewGCounter("writer")
	if err != nil {
		b.Fatal(err)
	}
	for ordinal := 1; ordinal <= leafCount; ordinal++ {
		delta, err := writer.Increment(1)
		if err != nil {
			b.Fatal(err)
		}
		change, err := durableCounterChange(manifest, "writer", uint64(ordinal), delta)
		if err != nil {
			b.Fatal(err)
		}
		event := Event{
			Sequence: uint64(ordinal),
			HLC:      crdt.Tag{ReplicaID: "benchmark-relay", WallTime: uint64(ordinal)},
			Change:   change,
		}
		leaf, err := merkleLeafForEvent(event)
		if err != nil {
			b.Fatal(err)
		}
		leaves = append(leaves, leaf)
		if !missingOne || ordinal != leafCount {
			if err := index.Put(event); err != nil {
				b.Fatal(err)
			}
		}
	}
	return index, leaves
}

func benchmarkMerkleStore(b *testing.B, manifest replica.Manifest, eventCount int) (*Store, []Event) {
	b.Helper()
	store, err := OpenStore(b.TempDir()+"/relay.db", StoreConfig{MaxEvents: uint64(eventCount) + 1, MaxBytes: uint64(eventCount+1) * 4096, HLCReplicaID: "benchmark-relay"})
	if err != nil {
		b.Fatal(err)
	}
	events := make([]Event, 0, eventCount)
	writer, err := counter.NewGCounter("writer")
	if err != nil {
		_ = store.Close()
		b.Fatal(err)
	}
	for index := 0; index < eventCount; index++ {
		delta, err := writer.Increment(1)
		if err != nil {
			_ = store.Close()
			b.Fatal(err)
		}
		change, err := durableCounterChange(manifest, "writer", uint64(index+1), delta)
		if err != nil {
			_ = store.Close()
			b.Fatal(err)
		}
		result, err := store.Append(manifest.GroupID, change)
		if err != nil {
			_ = store.Close()
			b.Fatal(err)
		}
		events = append(events, result.Event)
	}
	return store, events
}
