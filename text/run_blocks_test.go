package text

import "testing"

func TestMakeRunBlocksFastPathAndOutOfOrderFallback(t *testing.T) {
	first := Position{ReplicaID: "writer", WallTime: 1}
	second := Position{ReplicaID: "writer", WallTime: 2}
	third := Position{ReplicaID: "writer", WallTime: 3}
	linear := map[Position]node{
		first:  {rune: 'a'},
		second: {parent: first, rune: 'b'},
		third:  {parent: second, rune: 'c'},
	}
	blocks := makeRunBlocks(linear)
	if len(blocks) != 1 || len(blocks[0]) != 3 {
		t.Fatalf("linear blocks = %#v", blocks)
	}

	// Valid deltas may carry a parent whose HLC tag sorts after its child. That
	// shape must not take the sorted-chain fast path or emit a non-canonical run.
	parent := Position{ReplicaID: "writer", WallTime: 9}
	child := Position{ReplicaID: "writer", WallTime: 4}
	outOfOrder := map[Position]node{
		parent: {rune: 'p'},
		child:  {parent: parent, rune: 'c'},
	}
	blocks = makeRunBlocks(outOfOrder)
	if len(blocks) != 2 || len(blocks[0]) != 1 || len(blocks[1]) != 1 {
		t.Fatalf("out-of-order blocks = %#v", blocks)
	}
}
