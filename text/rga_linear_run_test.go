package text

import (
	"strings"
	"testing"
)

func TestRGAResolvedLinearRunPreservesTombstonesAndPendingReplay(t *testing.T) {
	const count = resolvedRunFastPathMinNodes * 2
	delta, ids := linearRunDeltaForTest(count)
	// A tombstone in the same delta must take effect after integration, just as
	// it does through the generic planner.
	delta.tombstones[ids[count/2]] = struct{}{}

	target := mustRGA(t, "target")
	pendingID := Position{ReplicaID: "later", WallTime: 1}
	if err := target.ApplyDelta(Delta{
		nodes:      map[Position]node{pendingID: {parent: ids[len(ids)-1], rune: 'z'}},
		tombstones: map[Position]struct{}{},
	}); err != nil {
		t.Fatal(err)
	}
	if got := target.PendingCount(); got != 1 {
		t.Fatalf("pending before linear run = %d, want 1", got)
	}

	target.mu.Lock()
	resolvedIDs, ok := target.resolvedLinearRunLocked(delta)
	target.mu.Unlock()
	if !ok || len(resolvedIDs) != count {
		t.Fatalf("resolvedLinearRunLocked() = %d IDs, %t; want %d, true", len(resolvedIDs), ok, count)
	}
	if err := target.ApplyDelta(delta); err != nil {
		t.Fatal(err)
	}
	if got := target.PendingCount(); got != 0 {
		t.Fatalf("pending after linear run = %d, want 0", got)
	}
	want := strings.Repeat("a", count/2) + strings.Repeat("a", count-count/2-1) + "z"
	if got := target.String(); got != want {
		t.Fatalf("linear run text = %q, want %q", got, want)
	}
	if state := target.State(); state.ElementCount != count || state.TombstoneCount != 1 {
		t.Fatalf("linear run state = %#v", state)
	}
}

func TestRGAResolvedLinearRunRejectsBranchingAndEnforcesLimitsAtomically(t *testing.T) {
	const count = resolvedRunFastPathMinNodes
	branching := Delta{nodes: make(map[Position]node, count), tombstones: map[Position]struct{}{}}
	for index := 0; index < count; index++ {
		id := Position{ReplicaID: "branch", WallTime: uint64(index + 1)}
		branching.nodes[id] = node{rune: 'a'}
	}
	target := mustRGA(t, "target")
	target.mu.Lock()
	_, ok := target.resolvedLinearRunLocked(branching)
	target.mu.Unlock()
	if ok {
		t.Fatal("branching delta unexpectedly selected resolved linear-run path")
	}

	limitedOptions := DefaultOptions()
	limitedOptions.MaxNodes = count - 1
	limited := mustRGAWithOptions(t, "limited", limitedOptions)
	linear, _ := linearRunDeltaForTest(count)
	if err := limited.ApplyDelta(linear); err != ErrResourceLimit {
		t.Fatalf("ApplyDelta(linear over limit) = %v, want %v", err, ErrResourceLimit)
	}
	if got := limited.String(); got != "" || limited.PendingCount() != 0 || limited.State().ElementCount != 0 {
		t.Fatalf("resource-limit rejection mutated state: text=%q pending=%d state=%#v", got, limited.PendingCount(), limited.State())
	}
}

func TestRGAResolvedLinearRunBatchIndexPreservesSiblingOrder(t *testing.T) {
	parent := Position{ReplicaID: "parent", WallTime: 1}
	existingLeft := Position{ReplicaID: "existing-left", WallTime: 1}
	existingRight := Position{ReplicaID: "existing-right", WallTime: 1}
	base := Delta{nodes: map[Position]node{
		parent:        {rune: 'p'},
		existingLeft:  {parent: parent, rune: 'l'},
		existingRight: {parent: parent, rune: 'r'},
	}, tombstones: map[Position]struct{}{}}
	target := mustRGA(t, "target")
	if err := target.ApplyDelta(base); err != nil {
		t.Fatal(err)
	}

	linear, ids := linearRunDeltaFromParentForTest(resolvedRunFastPathMinNodes, parent)
	linear.tombstones[ids[3]] = struct{}{}
	if err := target.ApplyDelta(linear); err != nil {
		t.Fatal(err)
	}
	allNodes := cloneNodes(base.nodes)
	for id, item := range linear.nodes {
		allNodes[id] = item
	}
	allTombstones := cloneTombstones(linear.tombstones)
	assertRGASequenceMatchesBuild(t, target, allNodes, allTombstones)

	// A later concurrent sibling verifies the child index was updated for the
	// batch just as it is for one-node generic integration.
	later := Position{ReplicaID: "later", WallTime: 1}
	laterDelta := Delta{nodes: map[Position]node{later: {parent: parent, rune: 'x'}}, tombstones: map[Position]struct{}{}}
	if err := target.ApplyDelta(laterDelta); err != nil {
		t.Fatal(err)
	}
	allNodes[later] = laterDelta.nodes[later]
	assertRGASequenceMatchesBuild(t, target, allNodes, allTombstones)
}

func linearRunDeltaForTest(count int) (Delta, []Position) {
	return linearRunDeltaFromParentForTest(count, Position{})
}

func linearRunDeltaFromParentForTest(count int, parent Position) (Delta, []Position) {
	nodes := make(map[Position]node, count)
	ids := make([]Position, 0, count)
	for index := 0; index < count; index++ {
		id := Position{ReplicaID: "linear", WallTime: uint64(index + 1)}
		nodes[id] = node{parent: parent, rune: 'a'}
		ids = append(ids, id)
		parent = id
	}
	return Delta{nodes: nodes, tombstones: make(map[Position]struct{})}, ids
}

func assertRGASequenceMatchesBuild(t *testing.T, value *RGA, nodes map[Position]node, tombstones map[Position]struct{}) {
	t.Helper()
	want, _, err := buildSequence(nodes, tombstones)
	if err != nil {
		t.Fatalf("build expected sequence: %v", err)
	}
	gotPositions, wantPositions := value.sequence.visiblePositions(), want.visiblePositions()
	if len(gotPositions) != len(wantPositions) {
		t.Fatalf("visible positions = %d, want %d", len(gotPositions), len(wantPositions))
	}
	for index := range wantPositions {
		if gotPositions[index] != wantPositions[index] {
			t.Fatalf("visible position %d = %#v, want %#v", index, gotPositions[index], wantPositions[index])
		}
	}
	if markerCount(value.sequence.root) != markerCount(want.root) || visibleCount(value.sequence.root) != visibleCount(want.root) {
		t.Fatalf("sequence counts = markers:%d visible:%d, want markers:%d visible:%d", markerCount(value.sequence.root), visibleCount(value.sequence.root), markerCount(want.root), visibleCount(want.root))
	}
}

func mustRGAWithOptions(t testing.TB, replicaID string, options Options) *RGA {
	t.Helper()
	value, err := NewWithOptions(replicaID, options)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
