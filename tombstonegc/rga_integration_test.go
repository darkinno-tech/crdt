package tombstonegc

import (
	"math/rand"
	"strconv"
	"testing"

	"github.com/darkinno-tech/crdt/text"
)

func TestCoordinatorCompactsDeletedRGAChainAfterExactAcknowledgements(t *testing.T) {
	source, err := text.New("api")
	if err != nil {
		t.Fatal(err)
	}
	insert, err := source.Insert(0, "abc")
	if err != nil {
		t.Fatal(err)
	}
	remove, err := source.Delete(0, 3)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := text.New("mobile")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyDelta(insert); err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyDelta(remove); err != nil {
		t.Fatal(err)
	}

	coordinator, err := NewCoordinator[struct{}]("documents/rga/v1", []string{"api", "mobile"})
	if err != nil {
		t.Fatal(err)
	}
	membership := coordinator.Membership()
	if removed, err := coordinator.AcknowledgeAndCompactTarget(membership.GroupID, "api", membership.Epoch, source.TombstoneTags(), source); err != nil || removed != 0 {
		t.Fatalf("api acknowledgement = %d, %v; want 0, nil", removed, err)
	}
	if removed, err := coordinator.AcknowledgeAndCompactTarget(membership.GroupID, "mobile", membership.Epoch, remote.TombstoneTags(), source); err != nil || removed != 3 {
		t.Fatalf("mobile acknowledgement = %d, %v; want 3, nil", removed, err)
	}
	if state := source.State(); state.ElementCount != 0 || state.TombstoneCount != 0 {
		t.Fatalf("compacted RGA state = %#v", state)
	}
}

// TestSimulatedCoordinatorCompactsRGADeletedBlock exercises the real RGA and
// coordinator paths under duplicate and reordered delta/receipt delivery.
func TestSimulatedCoordinatorCompactsRGADeletedBlock(t *testing.T) {
	for seed := int64(1); seed <= 32; seed++ {
		t.Run("seed_"+strconv.FormatInt(seed, 10), func(t *testing.T) {
			source, err := text.New("api")
			if err != nil {
				t.Fatal(err)
			}
			insert, err := source.Insert(0, "synchronized document")
			if err != nil {
				t.Fatal(err)
			}
			remove, err := source.Delete(0, len([]rune("synchronized document")))
			if err != nil {
				t.Fatal(err)
			}
			replicas := map[string]*text.RGA{"api": source}
			for replicaID, replicaSeed := range map[string]int64{"mobile": seed, "warehouse": seed + 1000} {
				replica, err := text.New(replicaID)
				if err != nil {
					t.Fatal(err)
				}
				changes := []text.Delta{insert, insert, remove, remove}
				random := rand.New(rand.NewSource(replicaSeed))
				random.Shuffle(len(changes), func(left, right int) { changes[left], changes[right] = changes[right], changes[left] })
				for _, change := range changes {
					if err := replica.ApplyDelta(change); err != nil {
						t.Fatal(err)
					}
				}
				if replica.PendingCount() != 0 || replica.String() != "" {
					t.Fatalf("replica %s did not converge after reordering: pending=%d text=%q", replicaID, replica.PendingCount(), replica.String())
				}
				replicas[replicaID] = replica
			}

			coordinator, err := NewCoordinator[struct{}]("documents/rga/v1", []string{"api", "mobile", "warehouse"})
			if err != nil {
				t.Fatal(err)
			}
			membership := coordinator.Membership()
			members := append([]string(nil), membership.Members...)
			random := rand.New(rand.NewSource(seed + 2000))
			random.Shuffle(len(members), func(left, right int) { members[left], members[right] = members[right], members[left] })
			for index, member := range members {
				removed, err := coordinator.AcknowledgeAndCompactTarget(membership.GroupID, member, membership.Epoch, replicas[member].TombstoneTags(), source)
				if err != nil {
					t.Fatal(err)
				}
				if index < len(members)-1 && removed != 0 {
					t.Fatalf("early compaction after %d acknowledgements: %d", index+1, removed)
				}
				if index == len(members)-1 && removed != len([]rune("synchronized document")) {
					t.Fatalf("final compaction = %d, want %d", removed, len([]rune("synchronized document")))
				}
			}
			if state := source.State(); state.ElementCount != 0 || state.TombstoneCount != 0 {
				t.Fatalf("source state after simulated compaction = %#v", state)
			}
		})
	}
}
