package text

import "testing"

func TestSequenceIndexReserveInitialPairsKeepsRoot(t *testing.T) {
	index := newSequenceIndex()
	root := index.pair(Position{})
	index.reserveInitialPairs(64)
	if got := index.pair(Position{}); got != root {
		t.Fatal("reserveInitialPairs replaced root pair")
	}

	pair := newSequencePair(Position{ReplicaID: "writer", WallTime: 1}, true)
	index.insertPairAfter(&root.entry, pair)
	index.reserveInitialPairs(64)
	if got := index.pair(pair.position); got != pair {
		t.Fatal("reserveInitialPairs changed a non-empty index")
	}
}
