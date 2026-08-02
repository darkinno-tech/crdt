package merkle

import (
	"fmt"
	"testing"

	"github.com/DarkInno/crdt/counter"
)

func TestTreeRootCachesUntilStateChanges(t *testing.T) {
	t.Parallel()
	tree := NewTree()
	tree.Insert("a", []byte("1"))
	first := tree.Root()

	tree.mu.RLock()
	firstGeneration := tree.generation
	if !tree.hasCachedRoot || tree.cachedRootFor != firstGeneration || tree.cachedRoot != first {
		tree.mu.RUnlock()
		t.Fatal("Root() did not cache the current generation")
	}
	tree.mu.RUnlock()

	tree.Insert("a", []byte("1"))
	tree.Delete("missing")
	tree.mu.RLock()
	if tree.generation != firstGeneration || !tree.hasCachedRoot {
		tree.mu.RUnlock()
		t.Fatal("unchanged state invalidated the root cache")
	}
	tree.mu.RUnlock()

	tree.Insert("a", []byte("2"))
	tree.mu.RLock()
	if tree.generation != firstGeneration+1 || tree.hasCachedRoot {
		tree.mu.RUnlock()
		t.Fatal("state change did not invalidate the root cache")
	}
	tree.mu.RUnlock()
	if second := tree.Root(); second == first {
		t.Fatal("Root() did not change after a different value was inserted")
	}
}

func TestTreeRootAndDiffAreDeterministic(t *testing.T) {
	left, right := NewTree(), NewTree()
	left.Insert("a", []byte("1"))
	left.Insert("b", []byte("2"))
	right.Insert("b", []byte("2"))
	right.Insert("a", []byte("1"))
	if got, want := fmt.Sprintf("%x", left.Root()), "834dfb5453f4cefaa6a513e994379ff0e1d56e5fbc28be7112b0fa7c76b6e816"; got != want {
		t.Fatalf("canonical root = %s, want %s", got, want)
	}
	if left.Root() != right.Root() {
		t.Fatal("equal entries have different roots")
	}
	right.Insert("b", []byte("3"))
	right.Insert("c", []byte("4"))
	leftOnly, rightOnly, different := Diff(left, right)
	if len(leftOnly) != 0 || len(rightOnly) != 1 || rightOnly[0] != "c" || len(different) != 1 || different[0] != "b" {
		t.Fatalf("Diff() = %#v %#v %#v", leftOnly, rightOnly, different)
	}
}

func TestDeleteRemovesKeyFromRootAndDiff(t *testing.T) {
	t.Parallel()
	withKey, withoutKey := NewTree(), NewTree()
	withKey.Insert("a", []byte("1"))
	withKey.Insert("b", []byte("2"))
	withoutKey.Insert("a", []byte("1"))
	withKey.Delete("b")
	withKey.Delete("missing")
	if withKey.Root() != withoutKey.Root() {
		t.Fatal("Delete() did not remove key from root")
	}
	leftOnly, rightOnly, different := Diff(withKey, withoutKey)
	if len(leftOnly) != 0 || len(rightOnly) != 0 || len(different) != 0 {
		t.Fatalf("Diff() after Delete = %#v %#v %#v", leftOnly, rightOnly, different)
	}
}

func TestInsertStateValidatesCRDTFrame(t *testing.T) {
	t.Parallel()
	counter, err := counter.NewGCounter("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := counter.Increment(1); err != nil {
		t.Fatal(err)
	}
	state, err := counter.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	tree := NewTree()
	if err := tree.InsertState("a", state); err != nil {
		t.Fatal(err)
	}
	if err := tree.InsertState("bad", []byte("not a frame")); err == nil {
		t.Fatal("invalid state was accepted")
	}
}

func TestTreeDiffFindsSparseDifferencesInLargeReplicaSet(t *testing.T) {
	t.Parallel()
	left, right := NewTree(), NewTree()
	for index := 0; index < 2048; index++ {
		key := fmt.Sprintf("document/%04d", index)
		state := []byte(fmt.Sprintf("state/%04d", index))
		left.Insert(key, state)
		right.Insert(key, state)
	}
	left.Insert("document/0100", []byte("new-left-state"))
	left.Insert("document/left-only", []byte("left"))
	right.Insert("document/right-only", []byte("right"))

	leftOnly, rightOnly, different := Diff(left, right)
	if got, want := leftOnly, []string{"document/left-only"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("left-only = %#v, want %#v", got, want)
	}
	if got, want := rightOnly, []string{"document/right-only"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("right-only = %#v, want %#v", got, want)
	}
	if got, want := different, []string{"document/0100"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("different = %#v, want %#v", got, want)
	}
}
