package text

import (
	"math/rand"
	"testing"
)

// TestRGARunOuterFrameV2ThreeReplicaEditingOverUnreliableNetwork keeps the
// existing run-v2 RGA semantics but sends every delta through the separately
// negotiated compression-aware outer frame v2. Retries and shuffled delivery
// must converge exactly as they do for a v1 envelope.
func TestRGARunOuterFrameV2ThreeReplicaEditingOverUnreliableNetwork(t *testing.T) {
	alice := mustRGA(t, "frame-v2-alice")
	bob := mustRGA(t, "frame-v2-bob")
	carol := mustRGA(t, "frame-v2-carol")

	base := mustInsertRGA(t, alice, 0, "Draft")
	mustApplyRGARunFrameV2Delta(t, bob, base)
	mustApplyRGARunFrameV2Delta(t, carol, base)
	bobEdit := mustInsertRGA(t, bob, 5, " for review")
	carolEdit := mustInsertRGA(t, carol, 5, " collaboratively")
	mustApplyRGARunFrameV2Delta(t, alice, carolEdit)
	mustApplyRGARunFrameV2Delta(t, alice, bobEdit)
	deleteEdit, err := alice.Delete(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	changes := []Delta{base, bobEdit, carolEdit, deleteEdit}

	for index, replica := range []*RGA{alice, bob, carol} {
		deliverRGARunFrameV2Changes(t, replica, changes, int64(20260730+index))
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

	state, err := alice.MarshalRunFrameV2()
	if err != nil {
		t.Fatal(err)
	}
	recovered := mustRGA(t, "frame-v2-recovered")
	if err := recovered.UnmarshalRunBinary(state); err != nil {
		t.Fatal(err)
	}
	if got := recovered.String(); got != want {
		t.Fatalf("recovered text = %q, want %q", got, want)
	}
}

func mustApplyRGARunFrameV2Delta(t testing.TB, target *RGA, delta Delta) {
	t.Helper()
	compressed, err := delta.MarshalRunFrameV2()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalRGARunDelta(compressed)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
}

func deliverRGARunFrameV2Changes(t testing.TB, target *RGA, changes []Delta, seed int64) {
	t.Helper()
	frames := make([][]byte, 0, len(changes)*2)
	for _, change := range changes {
		compressed, err := change.MarshalRunFrameV2()
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, compressed, compressed)
	}
	random := rand.New(rand.NewSource(seed))
	random.Shuffle(len(frames), func(left, right int) { frames[left], frames[right] = frames[right], frames[left] })
	for _, encoded := range frames {
		decoded, err := UnmarshalRGARunDelta(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if err := target.ApplyDelta(decoded); err != nil {
			t.Fatal(err)
		}
	}
}
