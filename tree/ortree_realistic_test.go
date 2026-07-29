package tree

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

// TestORTreeThreeEditorDeliveryAndRecovery exercises the receive path used by
// collaborative clients: independently produced edits are encoded, duplicated,
// shuffled, decoded, checkpointed, and replayed after recovery.
func TestORTreeThreeEditorDeliveryAndRecovery(t *testing.T) {
	alice, err := New("alice")
	if err != nil {
		t.Fatal(err)
	}
	root, rootDelta, err := alice.Add(NodeID{}, []byte("document"))
	if err != nil {
		t.Fatal(err)
	}
	bob, err := New("bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := bob.ApplyDelta(rootDelta); err != nil {
		t.Fatal(err)
	}
	bobNode, bobDelta, err := bob.Add(root, []byte("bob paragraph"))
	if err != nil {
		t.Fatal(err)
	}
	carol, err := New("carol")
	if err != nil {
		t.Fatal(err)
	}
	if err := carol.ApplyDelta(rootDelta); err != nil {
		t.Fatal(err)
	}
	_, carolDelta, err := carol.Add(root, []byte("carol paragraph"))
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.ApplyDelta(bobDelta); err != nil {
		t.Fatal(err)
	}
	if err := alice.ApplyDelta(carolDelta); err != nil {
		t.Fatal(err)
	}
	removeBob, err := alice.Remove(bobNode)
	if err != nil {
		t.Fatal(err)
	}

	changes := []Delta{rootDelta, bobDelta, carolDelta, removeBob}
	wantNodes, wantTombstones := alice.Nodes(), alice.TombstoneTags()
	for _, seed := range []int64{20260729, 20260801, 20260815, 20260901, 20261001} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			target, err := New(fmt.Sprintf("receiver-%d", seed))
			if err != nil {
				t.Fatal(err)
			}
			deliverTreeDeltas(t, target, changes, seed)
			assertORTreeState(t, target, wantNodes, wantTombstones)

			checkpoint, err := target.SnapshotCurrentState()
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := NewFromSnapshot(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			deliverTreeDeltas(t, recovered, changes, seed+1)
			assertORTreeState(t, recovered, wantNodes, wantTombstones)
		})
	}
}

func deliverTreeDeltas(t testing.TB, target *ORTree, changes []Delta, seed int64) {
	t.Helper()
	frames := make([][]byte, 0, len(changes)*2)
	for _, change := range changes {
		encoded, err := change.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, encoded, encoded)
	}
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(frames), func(left, right int) { frames[left], frames[right] = frames[right], frames[left] })
	for _, encoded := range frames {
		decoded, err := UnmarshalDelta(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if err := target.ApplyDelta(decoded); err != nil {
			t.Fatal(err)
		}
	}
}

func assertORTreeState(t testing.TB, target *ORTree, wantNodes []Node, wantTombstones []NodeID) {
	t.Helper()
	if nodes := target.Nodes(); !reflect.DeepEqual(nodes, wantNodes) {
		t.Fatalf("visible nodes = %#v, want %#v", nodes, wantNodes)
	}
	if tombstones := target.TombstoneTags(); !reflect.DeepEqual(tombstones, wantTombstones) {
		t.Fatalf("tombstones = %#v, want %#v", tombstones, wantTombstones)
	}
}
