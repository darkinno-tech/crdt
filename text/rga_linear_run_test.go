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

func linearRunDeltaForTest(count int) (Delta, []Position) {
	nodes := make(map[Position]node, count)
	ids := make([]Position, 0, count)
	parent := Position{}
	for index := 0; index < count; index++ {
		id := Position{ReplicaID: "linear", WallTime: uint64(index + 1)}
		nodes[id] = node{parent: parent, rune: 'a'}
		ids = append(ids, id)
		parent = id
	}
	return Delta{nodes: nodes, tombstones: make(map[Position]struct{})}, ids
}

func mustRGAWithOptions(t testing.TB, replicaID string, options Options) *RGA {
	t.Helper()
	value, err := NewWithOptions(replicaID, options)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
