package text

import (
	"math/rand"
	"testing"
)

// TestRGAThreeReplicaEditingOverUnreliableNetwork exercises a document-editing
// session rather than only a single CRDT method: concurrent inserts, a delete
// authored after merge, duplicate frames, arbitrary delivery order, and
// snapshot recovery all have to converge.
func TestRGAThreeReplicaEditingOverUnreliableNetwork(t *testing.T) {
	alice := mustRGA(t, "alice")
	bob := mustRGA(t, "bob")
	carol := mustRGA(t, "carol")

	base := mustInsertRGA(t, alice, 0, "Draft")
	for _, replica := range []*RGA{bob, carol} {
		mustApplyRGA(t, replica, base)
	}
	bobEdit := mustInsertRGA(t, bob, 5, " for review")
	carolEdit := mustInsertRGA(t, carol, 5, " collaboratively")

	// Alice receives both concurrent edits before making a selection deletion.
	mustApplyRGA(t, alice, carolEdit)
	mustApplyRGA(t, alice, bobEdit)
	deleteEdit, err := alice.Delete(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	changes := []Delta{base, bobEdit, carolEdit, deleteEdit}

	for index, replica := range []*RGA{alice, bob, carol} {
		deliverRGAChanges(t, replica, changes, int64(20260729+index))
	}
	want := alice.String()
	for _, replica := range []*RGA{bob, carol} {
		if got := replica.String(); got != want {
			t.Fatalf("replica text = %q, want %q", got, want)
		}
		if replica.PendingCount() != 0 {
			t.Fatalf("replica retained %d unresolved dependencies", replica.PendingCount())
		}
	}

	saved, err := bob.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	if got := recovered.String(); got != want {
		t.Fatalf("recovered text = %q, want %q", got, want)
	}
}

// TestRGARunV2ThreeReplicaEditingOverUnreliableNetwork exercises the
// production run-v2 transport boundary across the same edit pattern: each
// delivery is decoded from its negotiated wire format before merge, including
// retries and parent-before-child reordering.
func TestRGARunV2ThreeReplicaEditingOverUnreliableNetwork(t *testing.T) {
	alice := mustRGA(t, "run-alice")
	bob := mustRGA(t, "run-bob")
	carol := mustRGA(t, "run-carol")

	base := mustInsertRGA(t, alice, 0, "Draft")
	for _, replica := range []*RGA{bob, carol} {
		mustApplyRGARunDelta(t, replica, base)
	}
	bobEdit := mustInsertRGA(t, bob, 5, " for review")
	carolEdit := mustInsertRGA(t, carol, 5, " collaboratively")

	// Alice sees both concurrent run-v2 frames before cutting selected text.
	mustApplyRGARunDelta(t, alice, carolEdit)
	mustApplyRGARunDelta(t, alice, bobEdit)
	deleteEdit, err := alice.Delete(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	changes := []Delta{base, bobEdit, carolEdit, deleteEdit}

	for index, replica := range []*RGA{alice, bob, carol} {
		deliverRGARunChanges(t, replica, changes, int64(20260730+index))
	}
	want := alice.String()
	for _, replica := range []*RGA{bob, carol} {
		if got := replica.String(); got != want {
			t.Fatalf("replica text = %q, want %q", got, want)
		}
		if replica.PendingCount() != 0 {
			t.Fatalf("replica retained %d unresolved dependencies", replica.PendingCount())
		}
	}

	saved, err := bob.SnapshotRunCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	if got := recovered.String(); got != want {
		t.Fatalf("recovered text = %q, want %q", got, want)
	}
}

func deliverRGAChanges(t testing.TB, target *RGA, changes []Delta, seed int64) {
	t.Helper()
	frames := make([][]byte, 0, len(changes)*2)
	for _, change := range changes {
		encoded, err := change.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, encoded, encoded)
	}
	random := rand.New(rand.NewSource(seed))
	random.Shuffle(len(frames), func(left, right int) { frames[left], frames[right] = frames[right], frames[left] })
	for _, encoded := range frames {
		change, err := UnmarshalRGADelta(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if err := target.ApplyDelta(change); err != nil {
			t.Fatal(err)
		}
	}
}

func deliverRGARunChanges(t testing.TB, target *RGA, changes []Delta, seed int64) {
	t.Helper()
	frames := make([][]byte, 0, len(changes)*2)
	for _, change := range changes {
		encoded, err := change.MarshalRunBinary()
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, encoded, encoded)
	}
	random := rand.New(rand.NewSource(seed))
	random.Shuffle(len(frames), func(left, right int) { frames[left], frames[right] = frames[right], frames[left] })
	for _, encoded := range frames {
		change, err := UnmarshalRGARunDelta(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if err := target.ApplyDelta(change); err != nil {
			t.Fatal(err)
		}
	}
}

func mustRGA(t testing.TB, replicaID string) *RGA {
	t.Helper()
	value, err := New(replicaID)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustInsertRGA(t testing.TB, value *RGA, offset int, input string) Delta {
	t.Helper()
	delta, err := value.Insert(offset, input)
	if err != nil {
		t.Fatal(err)
	}
	return delta
}

func mustApplyRGA(t testing.TB, value *RGA, change Delta) {
	t.Helper()
	if err := value.ApplyDelta(change); err != nil {
		t.Fatal(err)
	}
}

func mustApplyRGARunDelta(t testing.TB, value *RGA, change Delta) {
	t.Helper()
	encoded, err := change.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalRGARunDelta(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := value.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
}
