package history

import (
	"errors"
	"testing"

	"github.com/im10furry/crdt/text"
)

func TestRepositoryBranchesAndCRDTMergeSimulation(t *testing.T) {
	repository, err := NewRepository(DefaultRepositoryOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateBranch("main", ID{}); err != nil {
		t.Fatal(err)
	}
	base := mustText(t, "base")
	seed, err := base.Insert(0, "A")
	if err != nil {
		t.Fatal(err)
	}
	baseID, err := repository.Commit("main", stateForText(t, base))
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Fork("experiment", "main"); err != nil {
		t.Fatal(err)
	}

	alice := mustText(t, "alice")
	bob := mustText(t, "bob")
	observer := mustText(t, "observer")
	for _, replica := range []*text.RGA{alice, bob, observer} {
		if err := replica.ApplyDelta(seed); err != nil {
			t.Fatal(err)
		}
	}
	aliceChange, err := alice.Insert(1, "L")
	if err != nil {
		t.Fatal(err)
	}
	bobChange, err := bob.Insert(1, "R")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Commit("main", stateForText(t, alice)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Commit("experiment", stateForText(t, bob)); err != nil {
		t.Fatal(err)
	}
	// Deliver divergent operations in different order and with one duplicate.
	for _, delta := range []text.Delta{bobChange, aliceChange, bobChange} {
		if err := alice.ApplyDelta(delta); err != nil {
			t.Fatal(err)
		}
	}
	for _, delta := range []text.Delta{aliceChange, bobChange, aliceChange} {
		if err := bob.ApplyDelta(delta); err != nil {
			t.Fatal(err)
		}
	}
	for _, delta := range []text.Delta{aliceChange, bobChange, bobChange, aliceChange} {
		if err := observer.ApplyDelta(delta); err != nil {
			t.Fatal(err)
		}
	}
	if alice.String() != bob.String() || alice.String() != observer.String() || alice.String() == "" {
		t.Fatalf("replicas did not converge: alice=%q bob=%q observer=%q", alice.String(), bob.String(), observer.String())
	}
	mergedID, err := repository.Merge("main", "experiment", stateForText(t, alice))
	if err != nil {
		t.Fatal(err)
	}
	version, ok := repository.Version(mergedID)
	if !ok || len(version.Parents) != 2 {
		t.Fatalf("merge version = %#v, %t", version, ok)
	}
	if history := repository.History("main"); len(history) != 4 || history[0].ID != mergedID || !containsVersion(history, baseID) {
		t.Fatalf("history = %#v", history)
	}
	if got, want := repository.Branches(), []string{"experiment", "main"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("branches = %q", got)
	}

	persisted, err := repository.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewRepositoryFromBinary(DefaultRepositoryOptions(), persisted)
	if err != nil {
		t.Fatal(err)
	}
	head, ok := restored.Head("main")
	if !ok || head != mergedID {
		t.Fatalf("restored main head = %v, %t", head, ok)
	}
	restoredVersion, ok := restored.Version(head)
	if !ok {
		t.Fatal("restored version missing")
	}
	materialized, err := text.NewFromSnapshot(restoredVersion.State.Snapshots[0].Value)
	if err != nil || materialized.String() != alice.String() {
		t.Fatalf("materialized version = %q, %v", materialized.String(), err)
	}
}

func TestRepositoryRejectsInvalidBranchesAndResourceExhaustion(t *testing.T) {
	options := DefaultRepositoryOptions()
	options.MaxVersions = 1
	repository, err := NewRepository(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateBranch("main", ID{}); err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateBranch("bad branch", ID{}); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("invalid branch = %v", err)
	}
	if _, err := repository.Commit("missing", State{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("missing branch = %v", err)
	}
	value := mustText(t, "writer")
	if _, err := value.Insert(0, "A"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Commit("main", stateForText(t, value)); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Insert(1, "B"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Commit("main", stateForText(t, value)); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("second version = %v", err)
	}
	if _, err := NewRepositoryFromBinary(DefaultRepositoryOptions(), []byte("bad")); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("bad persistence = %v", err)
	}
}

func FuzzRepositoryUnmarshal(f *testing.F) {
	repository, err := NewRepository(DefaultRepositoryOptions())
	if err != nil {
		f.Fatal(err)
	}
	if err := repository.CreateBranch("main", ID{}); err != nil {
		f.Fatal(err)
	}
	value := mustText(f, "writer")
	if _, err := value.Insert(0, "seed"); err != nil {
		f.Fatal(err)
	}
	if _, err := repository.Commit("main", stateForText(f, value)); err != nil {
		f.Fatal(err)
	}
	seed, err := repository.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte("bad"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = NewRepositoryFromBinary(DefaultRepositoryOptions(), data)
	})
}

func BenchmarkRepositoryCommitSnapshot(b *testing.B) {
	options := DefaultRepositoryOptions()
	options.MaxVersions = b.N + 1
	options.MaxEncodedBytes = 1 << 30
	repository, err := NewRepository(options)
	if err != nil {
		b.Fatal(err)
	}
	if err := repository.CreateBranch("main", ID{}); err != nil {
		b.Fatal(err)
	}
	value := mustText(b, "writer")
	if _, err := value.Insert(0, "benchmark"); err != nil {
		b.Fatal(err)
	}
	state := stateForText(b, value)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := repository.Commit("main", state); err != nil {
			b.Fatal(err)
		}
	}
}

func mustText(t testing.TB, replicaID string) *text.RGA {
	t.Helper()
	value, err := text.New(replicaID)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func stateForText(t testing.TB, value *text.RGA) State {
	t.Helper()
	saved, err := value.SnapshotRunCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	return State{Snapshots: []Snapshot{{Scope: "text/body", Value: saved}}}
}

func containsVersion(versions []Version, id ID) bool {
	for _, version := range versions {
		if version.ID == id {
			return true
		}
	}
	return false
}
