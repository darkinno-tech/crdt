package tombstonegc

import (
	"fmt"
	"testing"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/set"
)

func BenchmarkCoordinatorStableTombstones(b *testing.B) {
	for _, workload := range []struct {
		name       string
		members    int
		tombstones int
	}{
		{name: "members_2_tombstones_128", members: 2, tombstones: 128},
		{name: "members_2_tombstones_1024", members: 2, tombstones: 1024},
		{name: "members_8_tombstones_1024", members: 8, tombstones: 1024},
	} {
		b.Run(workload.name, func(b *testing.B) {
			coordinator, tags := benchmarkCoordinator(b, workload.members, workload.tombstones)
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				coordinator.membershipMu.RLock()
				coordinator.acknowledgementMu.Lock()
				stable := coordinator.stableTombstonesLocked(tags)
				coordinator.acknowledgementMu.Unlock()
				coordinator.membershipMu.RUnlock()
				if len(stable) != len(tags) {
					b.Fatalf("stable tombstones = %d, want %d", len(stable), len(tags))
				}
			}
		})
	}
}

// BenchmarkCoordinatorAcknowledgeAndCompact measures one complete three-replica
// durable-outbox cycle: create and cancel orders, collect exact acknowledgements,
// then compact the local target. Including setup keeps the benchmark bounded
// when run through make benchmark and captures end-to-end allocation pressure.
func BenchmarkCoordinatorAcknowledgeAndCompact(b *testing.B) {
	for _, tombstoneCount := range []int{32, 256} {
		b.Run(fmt.Sprintf("members_3_tombstones_%d", tombstoneCount), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				target, tags := benchmarkTombstoneTarget(b, tombstoneCount)
				coordinator, err := NewCoordinator[string]("benchmark/orders/v1", []string{"api", "mobile", "warehouse"})
				if err != nil {
					b.Fatal(err)
				}
				membership := coordinator.Membership()
				for _, member := range []string{"api", "warehouse"} {
					if err := coordinator.Acknowledge(membership.GroupID, member, membership.Epoch, tags); err != nil {
						b.Fatal(err)
					}
				}
				removed, err := coordinator.AcknowledgeAndCompact(membership.GroupID, "mobile", membership.Epoch, tags, target)
				if err != nil || removed != len(tags) {
					b.Fatalf("AcknowledgeAndCompact() = %d, %v; want %d, nil", removed, err, len(tags))
				}
			}
		})
	}
}

// BenchmarkSimpleCollectorCollect measures one local-only cleanup cycle. It
// intentionally includes target setup, just like
// BenchmarkCoordinatorAcknowledgeAndCompact, so results describe a bounded
// cleanup workload rather than a transport or durability claim.
func BenchmarkSimpleCollectorCollect(b *testing.B) {
	for _, tombstoneCount := range []int{32, 256} {
		b.Run(fmt.Sprintf("tombstones_%d", tombstoneCount), func(b *testing.B) {
			collector, err := NewSimpleCollector(SimplePolicy{MaxBatch: tombstoneCount})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				target, _ := benchmarkTombstoneTarget(b, tombstoneCount)
				removed, err := collector.Collect(target)
				if err != nil || removed != tombstoneCount {
					b.Fatalf("Collect() = %d, %v; want %d, nil", removed, err, tombstoneCount)
				}
			}
		})
	}
}

// BenchmarkCoordinatorPruneAcknowledgements measures the normal checkpoint
// path where one durable compaction drains every receipt tracked by a
// coordinator. Setup is stopped so ns/op covers only the prune operation.
func BenchmarkCoordinatorPruneAcknowledgements(b *testing.B) {
	for _, workload := range []struct {
		name       string
		members    int
		tombstones int
	}{
		{name: "members_3_tombstones_256", members: 3, tombstones: 256},
		{name: "members_8_tombstones_1024", members: 8, tombstones: 1024},
	} {
		b.Run(workload.name, func(b *testing.B) {
			b.ReportAllocs()
			for index := 0; index < b.N; index++ {
				b.StopTimer()
				coordinator, tags := benchmarkCoordinator(b, workload.members, workload.tombstones)
				membership := coordinator.Membership()
				b.StartTimer()
				removed, err := coordinator.PruneAcknowledgements(membership.GroupID, membership.Epoch, tags)
				if err != nil || removed != workload.members*workload.tombstones {
					b.Fatalf("PruneAcknowledgements() = %d, %v; want %d, nil", removed, err, workload.members*workload.tombstones)
				}
				b.StopTimer()
			}
		})
	}
}

func benchmarkCoordinator(b *testing.B, memberCount, tombstoneCount int) (*Coordinator[string], []crdt.Tag) {
	b.Helper()
	members := make([]string, memberCount)
	for index := range members {
		members[index] = fmt.Sprintf("member-%d", index)
	}
	coordinator, err := NewCoordinator[string]("benchmark/orders/v1", members)
	if err != nil {
		b.Fatal(err)
	}
	tags := make([]crdt.Tag, tombstoneCount)
	for index := range tags {
		tags[index] = crdt.Tag{ReplicaID: fmt.Sprintf("replica-%d", index%memberCount), WallTime: uint64(index + 1)}
	}
	membership := coordinator.Membership()
	for _, member := range members {
		if err := coordinator.Acknowledge(membership.GroupID, member, membership.Epoch, tags); err != nil {
			b.Fatal(err)
		}
	}
	return coordinator, tags
}

func benchmarkTombstoneTarget(b *testing.B, tombstoneCount int) (*set.ORSet[string], []crdt.Tag) {
	b.Helper()
	target := mustSet(b, "api", stringCodec{})
	for index := 0; index < tombstoneCount; index++ {
		value := fmt.Sprintf("order-%03d", index)
		if _, err := target.Add(value); err != nil {
			b.Fatal(err)
		}
		if _, err := target.Remove(value); err != nil {
			b.Fatal(err)
		}
	}
	return target, target.TombstoneTags()
}
