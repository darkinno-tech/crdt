package text

import (
	"bytes"
	"math/rand"
	"strconv"
	"testing"
)

// TestSimulatedRGAEligibleCompactionMatchesLeafByLeaf compares the batched
// structural planner with the conservative public fallback over varied local
// edit histories. The fallback removes one currently safe leaf at a time, so
// matching final state proves that batching did not skip an anchor or remove a
// node whose unselected descendant still needs it.
func TestSimulatedRGAEligibleCompactionMatchesLeafByLeaf(t *testing.T) {
	for seed := int64(1); seed <= 48; seed++ {
		t.Run("seed_"+strconv.FormatInt(seed, 10), func(t *testing.T) {
			value, err := New("writer")
			if err != nil {
				t.Fatal(err)
			}
			random := rand.New(rand.NewSource(seed))
			for step := 0; step < 64; step++ {
				visible := value.State().ElementCount
				if visible == 0 || random.Intn(3) != 0 {
					offset := random.Intn(visible + 1)
					if _, err := value.Insert(offset, string(rune('a'+random.Intn(26)))); err != nil {
						t.Fatal(err)
					}
					continue
				}
				count := 1 + random.Intn(minimum(3, visible))
				offset := random.Intn(visible - count + 1)
				if _, err := value.Delete(offset, count); err != nil {
					t.Fatal(err)
				}
			}

			state, err := value.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			fallback, err := New("fallback")
			if err != nil {
				t.Fatal(err)
			}
			if err := fallback.UnmarshalBinary(state); err != nil {
				t.Fatal(err)
			}
			if _, err := value.CompactEligibleTombstones(value.TombstoneTags()); err != nil {
				t.Fatal(err)
			}

			for {
				progressed := false
				for _, tag := range fallback.TombstoneTags() {
					removed, err := fallback.CompactTombstones([]Position{tag})
					if err == nil && removed == 1 {
						progressed = true
						continue
					}
					if err != nil && err != ErrUnsafeCompaction {
						t.Fatalf("CompactTombstones(%#v) = %d, %v", tag, removed, err)
					}
				}
				if !progressed {
					break
				}
			}

			batched, err := value.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			oneAtATime, err := fallback.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(batched, oneAtATime) {
				t.Fatalf("batched compaction diverged from leaf-by-leaf fallback")
			}
		})
	}
}

func minimum(left, right int) int {
	if left < right {
		return left
	}
	return right
}
