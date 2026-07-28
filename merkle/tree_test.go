package merkle

import (
	"testing"

	"github.com/DarkInno/crdt/counter"
)

func TestTreeRootAndDiffAreDeterministic(t *testing.T) {
	left, right := NewTree(), NewTree()
	left.Insert("a", []byte("1"))
	left.Insert("b", []byte("2"))
	right.Insert("b", []byte("2"))
	right.Insert("a", []byte("1"))
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
