package lww_test

import (
	"bytes"
	"testing"

	"github.com/darkinno-tech/crdt"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/lww"
	"github.com/darkinno-tech/crdt/replica"
)

type replicatedStringCodec struct{}

func (replicatedStringCodec) ID() string                            { return "example.com/lww-set-replication/v1" }
func (replicatedStringCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (replicatedStringCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }

func TestLWWSetThreeReplicaOutOfOrderDuplicateDelivery(t *testing.T) {
	codec := replicatedStringCodec{}
	policy := crdt.ProtocolPolicy{}
	manifest, err := replica.NewManifest("shared-labels", "example.com/labels/v1", 1, replica.Protocol{
		StateID:          crdt.TypeIDLWWSetState,
		DeltaID:          crdt.TypeIDLWWSetDelta,
		CodecID:          codec.ID(),
		SemanticsVersion: 1,
	}, policy)
	if err != nil {
		t.Fatal(err)
	}

	writerA, err := lww.NewSet[string]("writer-a")
	if err != nil {
		t.Fatal(err)
	}
	writerB, err := lww.NewSet[string]("writer-b")
	if err != nil {
		t.Fatal(err)
	}
	writerC, err := lww.NewSet[string]("writer-c")
	if err != nil {
		t.Fatal(err)
	}
	firstA, err := writerA.AddWithDelta("alpha")
	if err != nil {
		t.Fatal(err)
	}
	firstB, err := writerB.AddWithDelta("beta")
	if err != nil {
		t.Fatal(err)
	}
	firstC, err := writerC.AddWithDelta("gamma")
	if err != nil {
		t.Fatal(err)
	}
	removeA, err := writerA.RemoveWithDelta("alpha")
	if err != nil {
		t.Fatal(err)
	}

	changes := make([]replica.Change, 0, 4)
	for _, candidate := range []struct {
		delta   lww.SetDelta[string]
		actor   string
		counter uint64
	}{
		{firstA, "writer-a", 1},
		{firstB, "writer-b", 1},
		{firstC, "writer-c", 1},
		{removeA, "writer-a", 2},
	} {
		encoded, err := candidate.delta.MarshalBinary(codec)
		if err != nil {
			t.Fatal(err)
		}
		change, err := replica.NewChangeWithPolicy(manifest, replica.Dot{Actor: candidate.actor, Counter: candidate.counter}, encoded, policy)
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, change)
	}

	orders := [][]int{
		{3, 2, 0, 1, 0}, // writer-a counter 2 arrives before counter 1; duplicate counter 1.
		{1, 0, 3, 2, 3}, // reverse writer-a sequence and duplicate counter 2.
		{2, 3, 1, 0, 1},
	}
	states := make([][]byte, 0, len(orders))
	for index, order := range orders {
		target, err := lww.NewSet[string]("replica-" + string(rune('a'+index)))
		if err != nil {
			t.Fatal(err)
		}
		frontier, err := replica.NewFrontier(nil)
		if err != nil {
			t.Fatal(err)
		}
		inbox, err := replica.NewInboxWithPolicy(manifest, frontier, 8, frame.DefaultLimits().MaxFrameBytes, func(encoded []byte) error {
			delta, err := lww.UnmarshalSetDelta(encoded, codec)
			if err != nil {
				return err
			}
			return target.ApplyDelta(delta)
		}, policy)
		if err != nil {
			t.Fatal(err)
		}
		for _, changeIndex := range order {
			if _, err := inbox.Receive(changes[changeIndex]); err != nil {
				t.Fatalf("replica %d Receive(change %d) = %v", index, changeIndex, err)
			}
		}
		if pending, _ := inbox.Pending(); pending != 0 {
			t.Fatalf("replica %d pending = %d", index, pending)
		}
		if target.Contains("alpha") || !target.Contains("beta") || !target.Contains("gamma") {
			t.Fatalf("replica %d visible set diverged", index)
		}
		state, err := target.MarshalBinary(codec)
		if err != nil {
			t.Fatal(err)
		}
		states = append(states, state)
	}
	for index := 1; index < len(states); index++ {
		if !bytes.Equal(states[0], states[index]) {
			t.Fatalf("replicas 0 and %d produced different canonical states", index)
		}
	}
}
