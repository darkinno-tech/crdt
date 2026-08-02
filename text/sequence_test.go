package text

import (
	"math/rand"
	"strconv"
	"testing"
)

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

func TestMarkerTreeForLinearPairsMatchesReferenceTree(t *testing.T) {
	positions := []Position{
		{ReplicaID: "z", WallTime: 3, Logical: 1},
		{ReplicaID: "a", WallTime: 1, Logical: 2},
		{ReplicaID: "middle", WallTime: 9, Logical: 0},
		{ReplicaID: "a", WallTime: 4, Logical: 5},
		{ReplicaID: "last", WallTime: 2, Logical: 8},
	}
	random := rand.New(rand.NewSource(20260802))
	for index := 0; index < 128; index++ {
		positions = append(positions, Position{
			ReplicaID: "replica-" + strconv.Itoa(random.Intn(8)),
			WallTime:  uint64(random.Int63()),
			Logical:   uint64(random.Int63()),
		})
	}
	gotPairs := make([]sequencePair, len(positions))
	wantPairs := make([]sequencePair, len(positions))
	for index, position := range positions {
		visible := index%2 == 0
		initializeSequencePair(&gotPairs[index], position, visible)
		initializeSequencePair(&wantPairs[index], position, visible)
	}

	wantMarkers := make([]*sequenceMarker, 0, len(wantPairs)*2)
	for index := range wantPairs {
		wantMarkers = append(wantMarkers, &wantPairs[index].entry)
	}
	for index := len(wantPairs) - 1; index >= 0; index-- {
		wantMarkers = append(wantMarkers, &wantPairs[index].exit)
	}
	assertMatchingMarkerTrees(t, markerTreeForLinearPairs(gotPairs), markerTree(wantMarkers), nil, nil)
}

func assertMatchingMarkerTrees(t testing.TB, got, want, gotParent, wantParent *sequenceMarker) {
	t.Helper()
	if got == nil || want == nil {
		if got != nil || want != nil {
			t.Fatalf("marker presence = %t, want %t", got != nil, want != nil)
		}
		return
	}
	if got.parent != gotParent || want.parent != wantParent {
		t.Fatal("marker tree parent link is inconsistent")
	}
	if got.pair.position != want.pair.position || got.visible != want.visible || got.priority != want.priority || got.markers != want.markers || got.visibleN != want.visibleN {
		t.Fatalf("marker = %#v, want %#v", got, want)
	}
	assertMatchingMarkerTrees(t, got.left, want.left, got, want)
	assertMatchingMarkerTrees(t, got.right, want.right, got, want)
}
