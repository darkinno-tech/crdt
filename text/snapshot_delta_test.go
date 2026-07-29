package text

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

func TestRGAMarshalDeltaSinceSnapshotRoundTrip(t *testing.T) {
	source := mustRGA(t, "source")
	mustInsertRGA(t, source, 0, "base")
	base, err := source.SnapshotRunCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	mustInsertRGA(t, source, 4, " update")
	if _, err := source.Delete(1, 1); err != nil {
		t.Fatal(err)
	}

	encoded, err := source.MarshalDeltaSince(base)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := frame.UnmarshalFrame(encoded, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if decoded.TypeID != crdt.TypeIDRGARunDelta {
		t.Fatalf("delta type = %d, want run-v2", decoded.TypeID)
	}
	delta, err := UnmarshalRGARunDelta(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(delta.nodes), len([]rune(" update")); got != want {
		t.Fatalf("delta node count = %d, want %d", got, want)
	}
	if got := len(delta.tombstones); got != 1 {
		t.Fatalf("delta tombstone count = %d, want 1", got)
	}
	cachedBase, err := NewSnapshotBase(base)
	if err != nil {
		t.Fatal(err)
	}
	cachedEncoded, err := source.MarshalDeltaSinceBase(cachedBase)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cachedEncoded, encoded) {
		t.Fatal("cached snapshot base changed the canonical delta")
	}

	target, err := NewFromSnapshot(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(delta); err != nil {
		t.Fatal(err)
	}
	if got, want := target.String(), source.String(); got != want {
		t.Fatalf("delta recovery text = %q, want %q", got, want)
	}
	wantState, err := source.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	gotState, err := target.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotState, wantState) {
		t.Fatal("delta recovery state differs from source")
	}
}

func TestRGAMarshalDeltaSincePreservesScalarSnapshotProtocol(t *testing.T) {
	source := mustRGA(t, "source")
	mustInsertRGA(t, source, 0, "base")
	base, err := source.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	mustInsertRGA(t, source, 4, " update")
	if _, err := source.Delete(1, 1); err != nil {
		t.Fatal(err)
	}

	encoded, err := source.MarshalDeltaSince(base)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := frame.UnmarshalFrame(encoded, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if decoded.TypeID != crdt.TypeIDRGADelta {
		t.Fatalf("delta type = %d, want scalar v1", decoded.TypeID)
	}
	delta, err := UnmarshalRGADelta(encoded)
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewFromSnapshot(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(delta); err != nil {
		t.Fatal(err)
	}
	if got, want := target.String(), source.String(); got != want {
		t.Fatalf("delta recovery text = %q, want %q", got, want)
	}
}

func TestRGADeltaSinceIncludesTombstonedStructuralAncestor(t *testing.T) {
	parent := Position{ReplicaID: "source", WallTime: 1}
	child := Position{ReplicaID: "source", WallTime: 2}
	source := mustRGA(t, "source")
	if err := source.ApplyDelta(Delta{nodes: map[Position]node{
		parent: {rune: 'a'},
		child:  {parent: parent, rune: 'b'},
	}, tombstones: map[Position]struct{}{parent: {}}}); err != nil {
		t.Fatal(err)
	}
	baseReplica := mustRGA(t, "base")
	if err := baseReplica.ApplyDelta(Delta{
		nodes:      map[Position]node{},
		tombstones: map[Position]struct{}{parent: {}},
	}); err != nil {
		t.Fatal(err)
	}
	base, err := baseReplica.SnapshotRunCurrentState()
	if err != nil {
		t.Fatal(err)
	}

	delta, err := source.DeltaSince(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := delta.nodes[parent]; !ok {
		t.Fatal("delta omitted parent needed to integrate child")
	}
	if _, ok := delta.nodes[child]; !ok {
		t.Fatal("delta omitted novel child")
	}
	target, err := NewFromSnapshot(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(delta); err != nil {
		t.Fatal(err)
	}
	if got := target.String(); got != "b" {
		t.Fatalf("target text = %q, want child visible after tombstoned parent", got)
	}
}

func TestRGADeltaSinceRejectsConflictingOrInvalidBase(t *testing.T) {
	id := Position{ReplicaID: "shared", WallTime: 1}
	baseReplica := mustRGA(t, "base")
	if err := baseReplica.ApplyDelta(Delta{nodes: map[Position]node{id: {rune: 'a'}}, tombstones: map[Position]struct{}{}}); err != nil {
		t.Fatal(err)
	}
	base, err := baseReplica.SnapshotRunCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	source := mustRGA(t, "source")
	if err := source.ApplyDelta(Delta{nodes: map[Position]node{id: {rune: 'b'}}, tombstones: map[Position]struct{}{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.DeltaSince(base); !errors.Is(err, ErrTagConflict) {
		t.Fatalf("conflicting base error = %v, want %v", err, ErrTagConflict)
	}
	if _, err := source.DeltaSince(snapshot.Snapshot{}); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("invalid base error = %v, want %v", err, ErrInvalidSnapshot)
	}
	if _, err := source.DeltaSinceBase(SnapshotBase{}); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("invalid decoded base error = %v, want %v", err, ErrInvalidSnapshot)
	}
	if got := source.String(); got != "b" {
		t.Fatalf("failed delta calculation changed source to %q", got)
	}
}

func TestRGADeltaSinceRejectsBaseInvalidatedByCompaction(t *testing.T) {
	source := mustRGA(t, "source")
	mustInsertRGA(t, source, 0, "x")
	base, err := source.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Delete(0, 1); err != nil {
		t.Fatal(err)
	}
	if removed, err := source.CompactTombstones(source.TombstoneTags()); err != nil || removed != 1 {
		t.Fatalf("CompactTombstones() = %d, %v", removed, err)
	}

	if _, err := source.DeltaSince(base); !errors.Is(err, ErrIncompatibleSnapshot) {
		t.Fatalf("compacted base error = %v, want %v", err, ErrIncompatibleSnapshot)
	}
	if got := source.State(); got.ElementCount != 0 || got.TombstoneCount != 0 {
		t.Fatalf("failed delta calculation changed source state to %#v", got)
	}
}

func TestRGADeltaSinceRejectsBaseWithUnknownTombstone(t *testing.T) {
	id := Position{ReplicaID: "shared", WallTime: 1}
	source := mustRGA(t, "source")
	if err := source.ApplyDelta(Delta{nodes: map[Position]node{id: {rune: 'x'}}, tombstones: map[Position]struct{}{}}); err != nil {
		t.Fatal(err)
	}
	baseReplica := mustRGA(t, "base")
	if err := baseReplica.ApplyDelta(Delta{nodes: map[Position]node{}, tombstones: map[Position]struct{}{id: {}}}); err != nil {
		t.Fatal(err)
	}
	base, err := baseReplica.SnapshotRunCurrentState()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := source.DeltaSince(base); !errors.Is(err, ErrIncompatibleSnapshot) {
		t.Fatalf("newer tombstone error = %v, want %v", err, ErrIncompatibleSnapshot)
	}
	if got := source.String(); got != "x" {
		t.Fatalf("failed delta calculation changed source text to %q", got)
	}
}

func TestNewSnapshotBaseWithLimitsRetainsDecoderLimitError(t *testing.T) {
	source := mustRGA(t, "source")
	mustInsertRGA(t, source, 0, "ab")
	base, err := source.SnapshotRunCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	limits := frame.DefaultLimits()
	limits.MaxElements = 1
	if _, err := NewSnapshotBaseWithLimits(base, limits); !errors.Is(err, ErrInvalidSnapshot) || !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("limited base error = %v, want invalid snapshot and invalid frame", err)
	}
}

func TestRGAMarshalDeltaSinceHonorsOutputLimits(t *testing.T) {
	source := mustRGA(t, "source")
	base, err := source.SnapshotRunCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	mustInsertRGA(t, source, 0, "ab")
	limits := frame.DefaultLimits()
	limits.MaxElements = 1
	if _, err := source.MarshalDeltaSinceWithLimits(base, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("over-limit delta error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if got := source.String(); got != "ab" {
		t.Fatalf("over-limit encode changed source to %q", got)
	}
}

func TestRGAThreeReplicaSnapshotDeltaAntiEntropy(t *testing.T) {
	seed := mustRGA(t, "seed")
	baseDelta := mustInsertRGA(t, seed, 0, "Draft")
	base, err := seed.SnapshotRunCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	alice, bob, carol := mustRGA(t, "alice"), mustRGA(t, "bob"), mustRGA(t, "carol")
	for _, replica := range []*RGA{alice, bob, carol} {
		if err := replica.ApplyDelta(baseDelta); err != nil {
			t.Fatal(err)
		}
	}
	mustInsertRGA(t, alice, alice.State().ElementCount, " A")
	if _, err := alice.Delete(1, 1); err != nil {
		t.Fatal(err)
	}
	mustInsertRGA(t, bob, bob.State().ElementCount, " B")

	aliceUpdate, err := alice.MarshalDeltaSince(base)
	if err != nil {
		t.Fatal(err)
	}
	bobUpdate, err := bob.MarshalDeltaSince(base)
	if err != nil {
		t.Fatal(err)
	}
	for replicaIndex, replica := range []*RGA{alice, bob, carol} {
		frames := [][]byte{aliceUpdate, bobUpdate, aliceUpdate, bobUpdate}
		random := rand.New(rand.NewSource(int64(20260729 + replicaIndex)))
		random.Shuffle(len(frames), func(left, right int) { frames[left], frames[right] = frames[right], frames[left] })
		for _, encoded := range frames {
			delta, err := UnmarshalRGARunDelta(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if err := replica.ApplyDelta(delta); err != nil {
				t.Fatal(err)
			}
		}
	}

	want := alice.String()
	for _, replica := range []*RGA{bob, carol} {
		if got := replica.String(); got != want {
			t.Fatalf("replica text = %q, want %q", got, want)
		}
		if replica.PendingCount() != 0 {
			t.Fatalf("replica retained %d pending nodes", replica.PendingCount())
		}
	}
	saved, err := carol.SnapshotRunCurrentState()
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
