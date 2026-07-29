package text

import "testing"

func TestChildIndexPreservesDescendingSiblingOrderAcrossRemoval(t *testing.T) {
	index := newChildIndex()
	parent := newSequencePair(Position{ReplicaID: "parent", WallTime: 1}, false)
	oldest := newSequencePair(Position{ReplicaID: "child", WallTime: 1}, false)
	middle := newSequencePair(Position{ReplicaID: "child", WallTime: 2}, false)
	newest := newSequencePair(Position{ReplicaID: "child", WallTime: 3}, false)

	if previous, exists := index.insert(parent, middle); exists || previous != nil {
		t.Fatalf("first child predecessor = %#v, %t; want none", previous, exists)
	}
	if previous, exists := index.insert(parent, newest); exists || previous != nil {
		t.Fatalf("new highest child predecessor = %#v, %t; want none", previous, exists)
	}
	if previous, exists := index.insert(parent, oldest); !exists || previous != middle {
		t.Fatalf("new lowest child predecessor = %#v, %t; want %p, true", previous, exists, middle)
	}
	assertChildIndexSiblings(t, index, parent, []*sequencePair{newest, middle, oldest})

	if !index.remove(parent, middle) {
		t.Fatal("remove middle sibling = false")
	}
	assertChildIndexSiblings(t, index, parent, []*sequencePair{newest, oldest})
	if !index.remove(parent, newest) {
		t.Fatal("remove newest sibling = false")
	}
	assertChildIndexSiblings(t, index, parent, []*sequencePair{oldest})
	if !index.remove(parent, oldest) {
		t.Fatal("remove final sibling = false")
	}
	assertChildIndexSiblings(t, index, parent, nil)
}

func TestChildIndexRemoveSelectedCollapsesBranchToSingleton(t *testing.T) {
	index := newChildIndex()
	parent := newSequencePair(Position{ReplicaID: "parent", WallTime: 1}, false)
	oldest := newSequencePair(Position{ReplicaID: "child", WallTime: 1}, false)
	middle := newSequencePair(Position{ReplicaID: "child", WallTime: 2}, false)
	newest := newSequencePair(Position{ReplicaID: "child", WallTime: 3}, false)
	for _, child := range []*sequencePair{oldest, middle, newest} {
		index.insert(parent, child)
	}
	index.removeSelected(parent, map[Position]int{
		oldest.position: 0,
		middle.position: 1,
		newest.position: 2,
	}, []bool{true, false, true})
	assertChildIndexSiblings(t, index, parent, []*sequencePair{middle})
}

func TestRGACompactionClearsInlineSingleChild(t *testing.T) {
	value, err := New("writer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Insert(0, "ab"); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Delete(0, 2); err != nil {
		t.Fatal(err)
	}
	if removed, err := value.CompactEligibleTombstones(value.TombstoneTags()); err != nil || removed != 2 {
		t.Fatalf("CompactEligibleTombstones() = %d, %v; want 2, nil", removed, err)
	}
	value.mu.RLock()
	defer value.mu.RUnlock()
	root := value.sequence.pair(Position{})
	childCount := value.children.count(root)
	if root == nil || root.singleChild != nil || childCount != 0 || len(value.children.branches) != 0 {
		t.Fatalf("compaction retained inline child indexes: root=%t single=%t count=%d branches=%d", root != nil, root != nil && root.singleChild != nil, childCount, len(value.children.branches))
	}
}

func assertChildIndexSiblings(t *testing.T, index childIndex, parent *sequencePair, want []*sequencePair) {
	t.Helper()
	if got := index.count(parent); got != len(want) {
		t.Fatalf("child count = %d, want %d", got, len(want))
	}
	if len(want) == 0 {
		if parent.singleChild != nil {
			t.Fatalf("single child retained after removal: %#v", parent.singleChild.position)
		}
		if siblings := index.branches[parent.position]; len(siblings) != 0 {
			t.Fatalf("branch children retained after removal: %#v", siblings)
		}
		return
	}
	if len(want) == 1 {
		if got := parent.singleChild; got != want[0] {
			t.Fatalf("singleton child = %#v, want %#v", pairPosition(got), pairPosition(want[0]))
		}
		return
	}
	got := index.branches[parent.position]
	if len(got) != len(want) {
		t.Fatalf("branch count = %d, want %d", len(got), len(want))
	}
	for position, wantChild := range want {
		if got[position] != wantChild {
			t.Fatalf("branch child[%d] = %#v, want %#v", position, pairPosition(got[position]), pairPosition(wantChild))
		}
	}
}

func pairPosition(pair *sequencePair) Position {
	if pair == nil {
		return Position{}
	}
	return pair.position
}
