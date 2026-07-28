package crdt_test

import (
	"reflect"
	"testing"

	"github.com/darkinno/crdt/counter"
	"github.com/darkinno/crdt/delta"
	frame "github.com/darkinno/crdt/encoding"
	"github.com/darkinno/crdt/merkle"
	"github.com/darkinno/crdt/set"
	"github.com/darkinno/crdt/snapshot"
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
