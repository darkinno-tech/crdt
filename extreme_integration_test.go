package crdt_test

import (
	"fmt"
	"testing"

	"github.com/DarkInno/crdt/counter"
	"github.com/DarkInno/crdt/delta"
	"github.com/DarkInno/crdt/merkle"
	"github.com/DarkInno/crdt/set"
)

const (
	extremeReplicaCount       = 3
	extremeElementsPerReplica = 2048
	extremeCounterComponents  = 256
)

func TestHighCardinalityThreeReplicaRecoveryAndConvergence(t *testing.T) {
	codec := integrationStringCodec{}
	replicas := make([]*set.ORSet[string], extremeReplicaCount)
	for replica := range replicas {
		replicas[replica] = mustSet(t, fmt.Sprintf("stress-%d", replica), codec)
		for element := 0; element < extremeElementsPerReplica; element++ {
			if _, err := replicas[replica].Add(fmt.Sprintf("%d/%04d", replica, element)); err != nil {
				t.Fatal(err)
			}
		}
	}

	for target, receiver := range replicas {
		for source, sender := range replicas {
			if source == target {
				continue
			}
			if err := receiver.Merge(sender); err != nil {
				t.Fatal(err)
			}
		}
		if got, want := len(receiver.Elements()), extremeReplicaCount*extremeElementsPerReplica; got != want {
			t.Fatalf("replica %d element count = %d, want %d", target, got, want)
		}
	}

	state, err := replicas[0].MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(state) >= 1<<20 {
		t.Fatalf("high-cardinality state size = %d, expected to remain inside 1 MiB transport profile", len(state))
	}
	saved, err := replicas[0].SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := set.NewORSetFromSnapshot(saved, codec)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(recovered.Elements()), extremeReplicaCount*extremeElementsPerReplica; got != want {
		t.Fatalf("recovered element count = %d, want %d", got, want)
	}

	continued, err := recovered.Add("continued-after-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if err := replicas[1].ApplyDelta(continued); err != nil {
		t.Fatal(err)
	}
	if err := replicas[1].ApplyDelta(continued); err != nil {
		t.Fatal(err)
	}
	if !replicas[1].Contains("continued-after-recovery") {
		t.Fatal("continued delta did not survive duplicate delivery")
	}

	leftTree, rightTree := merkle.NewTree(), merkle.NewTree()
	leftState, err := replicas[1].MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	rightState, err := recovered.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := leftTree.InsertState("large-orset", leftState); err != nil {
		t.Fatal(err)
	}
	if err := rightTree.InsertState("large-orset", rightState); err != nil {
		t.Fatal(err)
	}
	if leftTree.Root() != rightTree.Root() {
		t.Fatal("large replica state diverged after duplicate delivery")
	}

	counterDeltas := make([][]byte, 0, extremeCounterComponents)
	for component := 0; component < extremeCounterComponents; component++ {
		value := mustCounter(t, fmt.Sprintf("counter-%03d", component))
		deltaValue := mustIncrement(t, value, uint64(component+1))
		counterDeltas = append(counterDeltas, mustMarshalCounterDelta(t, deltaValue))
	}
	batch, err := delta.NewBatch(counterDeltas, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	encodedBatch, err := batch.MarshalBinary(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	decodedBatch, err := delta.UnmarshalBatch(encodedBatch, extremeCounterComponents, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	aggregate := mustCounter(t, "aggregate")
	for _, encoded := range decodedBatch.Items() {
		decoded, err := counter.UnmarshalGCounterDelta(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if err := aggregate.ApplyDelta(decoded); err != nil {
			t.Fatal(err)
		}
	}
	want := uint64(extremeCounterComponents * (extremeCounterComponents + 1) / 2)
	if got, err := aggregate.Value(); err != nil || got != want {
		t.Fatalf("aggregate counter = %d, %v; want %d, nil", got, err, want)
	}
	t.Logf("high-cardinality scenario: elements=%d state_bytes=%d counter_components=%d batch_bytes=%d", extremeReplicaCount*extremeElementsPerReplica, len(state), extremeCounterComponents, len(encodedBatch))
}
