package membership

import (
	"math/rand"
	"strconv"
	"testing"
)

// TestSimulatedReceiptReorderingNeverCompactsEarly explores duplicated,
// shuffled receipt delivery across many deterministic schedules. It exercises
// the actual OR-Set/Coordinator path rather than a mock acknowledgement map.
func TestSimulatedReceiptReorderingNeverCompactsEarly(t *testing.T) {
	for seed := int64(1); seed <= 48; seed++ {
		t.Run("seed_"+strconv.FormatInt(seed, 10), func(t *testing.T) {
			setup := newFixture(t, 1, "api", "mobile", "warehouse")
			manager, err := NewManager[string](setup.view, setup.authorityPublic, &MemoryStore{})
			if err != nil {
				t.Fatal(err)
			}
			bridge, err := NewGCBridge(manager)
			if err != nil {
				t.Fatal(err)
			}
			target := mustORSet(t, "api")
			for index := 0; index < 12; index++ {
				value := "order-" + strconv.Itoa(index)
				if _, err := target.Add(value); err != nil {
					t.Fatal(err)
				}
				if _, err := target.Remove(value); err != nil {
					t.Fatal(err)
				}
			}
			tags, err := SortedTags(target.TombstoneTags())
			if err != nil {
				t.Fatal(err)
			}
			if len(tags) != 12 {
				t.Fatalf("tag count = %d", len(tags))
			}
			memberIDs := []string{"api", "mobile", "warehouse"}
			random := rand.New(rand.NewSource(seed))
			random.Shuffle(len(memberIDs), func(left, right int) { memberIDs[left], memberIDs[right] = memberIDs[right], memberIDs[left] })
			for index, memberID := range memberIDs {
				receipt := signedReceipt(t, setup, setup.view, memberID, uint64(index+1), tags)
				removed, err := bridge.Apply(receipt, target)
				if err != nil {
					t.Fatal(err)
				}
				if index < len(memberIDs)-1 {
					if removed != 0 || len(target.TombstoneTags()) != len(tags) {
						t.Fatalf("early compaction after %d receipts: removed=%d remaining=%d", index+1, removed, len(target.TombstoneTags()))
					}
					continue
				}
				if removed != len(tags) || len(target.TombstoneTags()) != 0 {
					t.Fatalf("final compaction removed=%d remaining=%d", removed, len(target.TombstoneTags()))
				}
			}
		})
	}
}
