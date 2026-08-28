package crdt_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/im10furry/crdt/counter"
	"github.com/im10furry/crdt/delta"
	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/merkle"
	"github.com/im10furry/crdt/set"
	"github.com/im10furry/crdt/snapshot"
	"github.com/im10furry/crdt/tombstonegc"
)

type integrationStringCodec struct{}

func (integrationStringCodec) ID() string                            { return "crdt/integration-string/v1" }
func (integrationStringCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (integrationStringCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }

func TestThreeReplicaDeltaDeliveryRecoveryAndAntiEntropy(t *testing.T) {
	leftCounter := mustCounter(t, "left")
	middleCounter := mustCounter(t, "middle")
	rightCounter := mustCounter(t, "right")

	leftDelta := mustIncrement(t, leftCounter, 2)
	middleDelta := mustIncrement(t, middleCounter, 3)
	rightDelta := mustIncrement(t, rightCounter, 5)
	encodedDeltas := [][]byte{mustMarshalCounterDelta(t, leftDelta), mustMarshalCounterDelta(t, middleDelta), mustMarshalCounterDelta(t, rightDelta)}

	batch, err := delta.NewBatch([][]byte{encodedDeltas[2], encodedDeltas[0], encodedDeltas[0], encodedDeltas[1]}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	batchBytes, err := batch.MarshalBinary(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	delivered, err := delta.UnmarshalBatch(batchBytes, 8, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, replica := range []*counter.GCounter{leftCounter, middleCounter, rightCounter} {
		for _, encoded := range delivered.Items() {
			decoded, err := counter.UnmarshalGCounterDelta(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if err := replica.ApplyDelta(decoded); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := replica.Value(); err != nil || got != 10 {
			t.Fatalf("replica value = %d, %v; want 10, nil", got, err)
		}
	}

	codec := integrationStringCodec{}
	leftSet := mustSet(t, "left", codec)
	middleSet := mustSet(t, "middle", codec)
	rightSet := mustSet(t, "right", codec)
	leftAdd := mustAdd(t, leftSet, "left-only")
	middleAdd := mustAdd(t, middleSet, "middle-only")
	rightAdd := mustAdd(t, rightSet, "right-only")
	for _, target := range []*set.ORSet[string]{leftSet, middleSet, rightSet} {
		for _, change := range []set.ORSetDelta[string]{rightAdd, leftAdd, middleAdd, leftAdd} {
			if err := target.ApplyDelta(change); err != nil {
				t.Fatal(err)
			}
		}
	}
	remove := mustRemove(t, middleSet, "left-only")
	if err := leftSet.ApplyDelta(remove); err != nil {
		t.Fatal(err)
	}
	if err := rightSet.ApplyDelta(remove); err != nil {
		t.Fatal(err)
	}
	for _, target := range []*set.ORSet[string]{leftSet, middleSet, rightSet} {
		if target.Contains("left-only") || !target.Contains("middle-only") || !target.Contains("right-only") {
			t.Fatalf("unexpected OR-Set membership: %#v", target.Elements())
		}
	}

	saved, err := rightSet.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := snapshot.NewRecoveryPlan(saved, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := set.NewORSetFromSnapshot(plan.Snapshot, codec)
	if err != nil {
		t.Fatal(err)
	}
	postRestartAdd := mustAdd(t, recovered, "after-restart")
	if err := leftSet.ApplyDelta(postRestartAdd); err != nil {
		t.Fatal(err)
	}
	if !leftSet.Contains("after-restart") {
		t.Fatal("post-recovery OR-Set delta was not applied")
	}
	if _, err := leftSet.Add("needs-sync"); err != nil {
		t.Fatal(err)
	}

	leftState, err := leftSet.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	recoveredState, err := recovered.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := frame.UnmarshalFrame(leftState, frame.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	leftTree, recoveredTree := merkle.NewTree(), merkle.NewTree()
	if err := leftTree.InsertState("orset", leftState); err != nil {
		t.Fatal(err)
	}
	if err := recoveredTree.InsertState("orset", recoveredState); err != nil {
		t.Fatal(err)
	}
	_, _, different := merkle.Diff(leftTree, recoveredTree)
	if !reflect.DeepEqual(different, []string{"orset"}) {
		t.Fatalf("anti-entropy diff = %#v, want [orset]", different)
	}
	if err := recovered.Merge(leftSet); err != nil {
		t.Fatal(err)
	}
	if !recovered.Contains("after-restart") || !recovered.Contains("middle-only") || !recovered.Contains("needs-sync") || !recovered.Contains("right-only") {
		t.Fatalf("recovered set did not converge: %#v", recovered.Elements())
	}
}

// TestThreeReplicaOrderCancellationTombstoneLifecycle models a durable outbox
// workflow: all replicas receive order creates, one mobile replica misses
// cancellation deltas while receiving a later update, retries eventually
// deliver the cancellations, then every replica compacts and persists a
// post-compaction snapshot before acknowledgement records are pruned.
func TestThreeReplicaOrderCancellationTombstoneLifecycle(t *testing.T) {
	const (
		groupID       = "orders/production-like/v1"
		orderCount    = 96
		cancelEvery   = 3
		cancelledWant = orderCount / cancelEvery
	)
	codec := integrationStringCodec{}
	api := mustSet(t, "api", codec)
	mobile := mustSet(t, "mobile", codec)
	warehouse := mustSet(t, "warehouse", codec)

	for order := 0; order < orderCount; order++ {
		id := fmt.Sprintf("order-%03d", order)
		created := mustAdd(t, api, id)
		for _, replica := range []*set.ORSet[string]{mobile, warehouse} {
			if err := replica.ApplyDelta(created); err != nil {
				t.Fatal(err)
			}
		}
	}

	cancellations := make([]set.ORSetDelta[string], 0, cancelledWant)
	for order := 0; order < orderCount; order += cancelEvery {
		cancellations = append(cancellations, mustRemove(t, api, fmt.Sprintf("order-%03d", order)))
	}
	// The warehouse receives every cancellation. The mobile replica misses all
	// of them but still receives a later order, so its Frontier cannot be used
	// as evidence that it observed the missing tombstones.
	for index := len(cancellations) - 1; index >= 0; index-- {
		if err := warehouse.ApplyDelta(cancellations[index]); err != nil {
			t.Fatal(err)
		}
	}
	later := mustAdd(t, api, "order-late")
	if err := mobile.ApplyDelta(later); err != nil {
		t.Fatal(err)
	}
	if err := mobile.ApplyDelta(later); err != nil {
		t.Fatal(err)
	}
	if !mobile.Contains("order-000") {
		t.Fatal("mobile unexpectedly observed a cancellation before its outbox retry")
	}

	coordinator, err := tombstonegc.NewCoordinator[string](groupID, []string{"api", "mobile", "warehouse"})
	if err != nil {
		t.Fatal(err)
	}
	membership := coordinator.Membership()
	acknowledged := api.TombstoneTags()
	if len(acknowledged) != cancelledWant {
		t.Fatalf("api tombstones = %d, want %d", len(acknowledged), cancelledWant)
	}
	if removed, err := coordinator.AcknowledgeAndCompact(groupID, "api", membership.Epoch, acknowledged, api); err != nil || removed != 0 {
		t.Fatalf("api acknowledgement = %d, %v; want 0, nil", removed, err)
	}
	if removed, err := coordinator.AcknowledgeAndCompact(groupID, "warehouse", membership.Epoch, warehouse.TombstoneTags(), api); err != nil || removed != 0 {
		t.Fatalf("warehouse acknowledgement = %d, %v; want 0, nil", removed, err)
	}
	if got := api.State().TombstoneCount; got != cancelledWant {
		t.Fatalf("premature compaction tombstones = %d, want %d", got, cancelledWant)
	}

	for index := len(cancellations) - 1; index >= 0; index-- {
		if err := mobile.ApplyDelta(cancellations[index]); err != nil {
			t.Fatal(err)
		}
		if index%5 == 0 {
			if err := mobile.ApplyDelta(cancellations[index]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if removed, err := coordinator.AcknowledgeAndCompact(groupID, "mobile", membership.Epoch, mobile.TombstoneTags(), api); err != nil || removed != cancelledWant {
		t.Fatalf("mobile acknowledgement = %d, %v; want %d, nil", removed, err, cancelledWant)
	}
	for _, target := range []*set.ORSet[string]{mobile, warehouse} {
		if removed, err := coordinator.AcknowledgeAndCompact(groupID, "api", membership.Epoch, api.TombstoneTags(), target); err != nil || removed != cancelledWant {
			t.Fatalf("shared coordinator compaction = %d, %v; want %d, nil", removed, err, cancelledWant)
		}
	}
	for _, replica := range []*set.ORSet[string]{api, mobile, warehouse} {
		if got := replica.State().TombstoneCount; got != 0 {
			t.Fatalf("replica retained %d tombstones after all acknowledgements", got)
		}
		if replica.Contains("order-000") {
			t.Fatalf("cancelled order resurrected: %#v", replica.Elements())
		}
	}

	// Model the durable persistence boundary before acknowledgement records are
	// freed. A same-ID recovery must retain its HLC state and can continue to
	// accept new orders without reusing an old mutation tag.
	saved, err := api.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := set.NewORSetFromSnapshot(saved, codec)
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := coordinator.PruneAcknowledgements(groupID, membership.Epoch, acknowledged); err != nil || removed != 3*cancelledWant {
		t.Fatalf("PruneAcknowledgements() = %d, %v; want %d, nil", removed, err, 3*cancelledWant)
	}
	if stats := coordinator.AcknowledgementStats(); stats.Tags != 0 || stats.Entries != 0 {
		t.Fatalf("acknowledgement stats after snapshot and prune = %#v", stats)
	}
	continued := mustAdd(t, recovered, "order-after-recovery")
	for _, replica := range []*set.ORSet[string]{mobile, warehouse} {
		if err := replica.ApplyDelta(continued); err != nil {
			t.Fatal(err)
		}
		if !replica.Contains("order-after-recovery") || replica.Contains("order-000") {
			t.Fatalf("unexpected state after recovery delta: %#v", replica.Elements())
		}
	}
}

func mustCounter(t testing.TB, replicaID string) *counter.GCounter {
	t.Helper()
	value, err := counter.NewGCounter(replicaID)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustIncrement(t testing.TB, value *counter.GCounter, amount uint64) counter.GCounterDelta {
	t.Helper()
	delta, err := value.Increment(amount)
	if err != nil {
		t.Fatal(err)
	}
	return delta
}

func mustMarshalCounterDelta(t testing.TB, value counter.GCounterDelta) []byte {
	t.Helper()
	encoded, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func mustSet(t testing.TB, replicaID string, codec integrationStringCodec) *set.ORSet[string] {
	t.Helper()
	value, err := set.NewORSet(replicaID, codec)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustAdd(t testing.TB, value *set.ORSet[string], element string) set.ORSetDelta[string] {
	t.Helper()
	delta, err := value.Add(element)
	if err != nil {
		t.Fatal(err)
	}
	return delta
}

func mustRemove(t testing.TB, value *set.ORSet[string], element string) set.ORSetDelta[string] {
	t.Helper()
	delta, err := value.Remove(element)
	if err != nil {
		t.Fatal(err)
	}
	return delta
}
