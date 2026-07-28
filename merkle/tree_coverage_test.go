package merkle

import "testing"

func TestNilAndEmptyTreesHaveSafeDeterministicBehavior(t *testing.T) {
	var nilTree *Tree
	nilTree.Insert("ignored", []byte("value"))
	nilTree.Delete("ignored")
	if nilTree.Root() != emptyRoot() {
		t.Fatal("nil tree root differs from empty root")
	}
	empty := NewTree()
	if empty.Root() != nilTree.Root() {
		t.Fatal("empty and nil tree roots differ")
	}
	empty.Insert("remote", []byte("value"))
	leftOnly, rightOnly, different := Diff(nil, empty)
	if len(leftOnly) != 0 || len(rightOnly) != 1 || rightOnly[0] != "remote" || len(different) != 0 {
		t.Fatalf("Diff(nil, tree) = %#v %#v %#v", leftOnly, rightOnly, different)
	}
}
