package set

import (
	"errors"
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

type stringCodec struct{ id string }

func (c stringCodec) ID() string                          { return c.id }
func (stringCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (stringCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }

func TestORSetObservedRemoveAndConcurrentAdd(t *testing.T) {
	t.Parallel()
	codec := stringCodec{id: "example.com/string/v1"}
	left := mustNewORSet(t, "left", codec)
	right := mustNewORSet(t, "right", codec)

	firstAdd, err := left.Add("x")
	if err != nil {
		t.Fatalf("left.Add() error = %v", err)
	}
	if err := right.ApplyDelta(firstAdd); err != nil {
		t.Fatalf("right.ApplyDelta() error = %v", err)
	}
	if _, err := left.Remove("x"); err != nil {
		t.Fatalf("left.Remove() error = %v", err)
	}
	if _, err := right.Add("x"); err != nil {
		t.Fatalf("right.Add() error = %v", err)
	}
	if err := left.Merge(right); err != nil {
		t.Fatalf("left.Merge() error = %v", err)
	}
	if err := right.Merge(left); err != nil {
		t.Fatalf("right.Merge() error = %v", err)
	}
	if !left.Contains("x") || !right.Contains("x") {
		t.Fatal("concurrent add should win over unobserved remove")
	}
}

func TestORSetRemovePropagatesAndDeltaIsIdempotent(t *testing.T) {
	t.Parallel()
	codec := stringCodec{id: "example.com/string/v1"}
	source := mustNewORSet(t, "source", codec)
	target := mustNewORSet(t, "target", codec)
	add, err := source.Add("x")
	if err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	if err := target.ApplyDelta(add); err != nil {
		t.Fatalf("ApplyDelta(add) error = %v", err)
	}
	remove, err := source.Remove("x")
	if err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if err := target.ApplyDelta(remove); err != nil {
		t.Fatalf("ApplyDelta(remove) error = %v", err)
	}
	if err := target.ApplyDelta(remove); err != nil {
		t.Fatalf("duplicate ApplyDelta(remove) error = %v", err)
	}
	if target.Contains("x") {
		t.Fatal("removed tag remains visible")
	}
}

func TestORSetRejectsCodecMismatchWithoutMutation(t *testing.T) {
	t.Parallel()
	left := mustNewORSet(t, "left", stringCodec{id: "one"})
	right := mustNewORSet(t, "right", stringCodec{id: "two"})
	if _, err := right.Add("x"); err != nil {
		t.Fatalf("right.Add() error = %v", err)
	}
	if err := left.Merge(right); !errors.Is(err, ErrCodecMismatch) {
		t.Fatalf("Merge() error = %v, want %v", err, ErrCodecMismatch)
	}
	if left.Contains("x") {
		t.Fatal("codec mismatch modified receiver")
	}
}

func TestORSetBinaryRoundTripIsDeterministicAndAtomic(t *testing.T) {
	t.Parallel()
	codec := stringCodec{id: "example.com/string/v1"}
	source := mustNewORSet(t, "source", codec)
	if _, err := source.Add("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Add("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Remove("b"); err != nil {
		t.Fatal(err)
	}
	first, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("MarshalBinary is non-deterministic")
	}
	target := mustNewORSet(t, "target", codec)
	if err := target.UnmarshalBinary(first); err != nil {
		t.Fatal(err)
	}
	if !target.Contains("a") || target.Contains("b") || target.State().TombstoneCount != 1 {
		t.Fatalf("round trip state = %#v", target.State())
	}
	before := target.State()
	first[len(first)-1] ^= 1
	if err := target.UnmarshalBinary(first); err == nil {
		t.Fatal("corrupt frame accepted")
	}
	if got := target.State(); got != before {
		t.Fatalf("failed decode modified state: got %#v, want %#v", got, before)
	}
}

func TestORSetBinaryRejectsCodecMismatchWithoutMutation(t *testing.T) {
	t.Parallel()
	source := mustNewORSet(t, "source", stringCodec{id: "one"})
	if _, err := source.Add("x"); err != nil {
		t.Fatal(err)
	}
	encoded, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	target := mustNewORSet(t, "target", stringCodec{id: "two"})
	before := target.State()
	if err := target.UnmarshalBinary(encoded); !errors.Is(err, ErrCodecMismatch) {
		t.Fatalf("UnmarshalBinary() error = %v", err)
	}
	if got := target.State(); got != before {
		t.Fatalf("codec mismatch modified receiver: got %#v, want %#v", got, before)
	}
}

func TestORSetBinaryHonorsCallerLimitsWithoutMutation(t *testing.T) {
	t.Parallel()
	codec := stringCodec{id: "example.com/string/v1"}
	source := mustNewORSet(t, "source", codec)
	for _, element := range []string{"a", "b"} {
		if _, err := source.Add(element); err != nil {
			t.Fatal(err)
		}
	}
	encoded, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	target := mustNewORSet(t, "target", codec)
	before := target.State()
	limits := frame.DefaultLimits()
	limits.MaxElements = 1
	if err := target.UnmarshalBinaryWithLimits(encoded, limits); err == nil {
		t.Fatal("over-limit state accepted")
	}
	if got := target.State(); got != before {
		t.Fatalf("over-limit decode modified receiver: got %#v, want %#v", got, before)
	}
}

func TestORSetMarshalRejectsPayloadBeyondConfiguredLimit(t *testing.T) {
	t.Parallel()
	codec := stringCodec{id: "example.com/string/v1"}
	limits := frame.DefaultLimits()
	limits.MaxPayload = 1
	adds := map[string]map[crdt.Tag]struct{}{
		"x": {{ReplicaID: "a", WallTime: 1, Logical: 1}: {}},
	}
	if _, err := marshalORSetWithLimits(crdt.TypeIDORSetState, codec, adds, nil, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("marshalORSetWithLimits() error = %v, want %v", err, frame.ErrFrameLimit)
	}
}

func TestORSetDeltaBinaryRoundTripAndTypeIsolation(t *testing.T) {
	t.Parallel()
	codec := stringCodec{id: "example.com/string/v1"}
	source := mustNewORSet(t, "source", codec)
	add, err := source.Add("x")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := add.MarshalBinary(codec)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalORSetDelta(encoded, codec)
	if err != nil {
		t.Fatal(err)
	}
	target := mustNewORSet(t, "target", codec)
	if err := target.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
	if !target.Contains("x") {
		t.Fatal("decoded add delta was not applied")
	}
	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalORSetDelta(state, codec); err == nil {
		t.Fatal("state frame accepted as delta")
	}
}

func TestORSetRestoredClockDoesNotReuseAddTags(t *testing.T) {
	t.Parallel()
	codec := stringCodec{id: "example.com/string/v1"}
	state := clock.State{ReplicaID: "replica", WallTime: 1 << 63, Logical: 8}
	restored, err := NewORSetFromClock(state, codec)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.ClockState(); got != state {
		t.Fatalf("ClockState() = %#v, want %#v", got, state)
	}
	delta, err := restored.Add("x")
	if err != nil {
		t.Fatal(err)
	}
	for tag := range delta.adds["x"] {
		if tag.ReplicaID != state.ReplicaID || tag.WallTime != state.WallTime || tag.Logical != state.Logical+1 {
			t.Fatalf("restored add tag = %#v", tag)
		}
	}
}

func TestORSetCompactsOnlyAtStableFrontier(t *testing.T) {
	t.Parallel()
	codec := stringCodec{id: "example.com/string/v1"}
	source := mustNewORSet(t, "replica", codec)
	if _, err := source.Add("x"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Remove("x"); err != nil {
		t.Fatal(err)
	}
	frontier := source.Frontier()
	if len(frontier) != 1 || frontier["replica"].ReplicaID != "replica" {
		t.Fatalf("Frontier() = %#v", frontier)
	}
	if removed, err := source.Compact(map[string]crdt.Tag{"replica": {ReplicaID: "replica"}}); err != nil || removed != 0 {
		t.Fatalf("Compact(unstable) = %d, %v; want 0, nil", removed, err)
	}
	if removed, err := source.Compact(frontier); err != nil || removed != 1 {
		t.Fatalf("Compact() = %d, %v; want 1, nil", removed, err)
	}
	if got := source.State().TombstoneCount; got != 0 {
		t.Fatalf("compacted tombstones = %d, want 0", got)
	}
	if _, err := source.Compact(map[string]crdt.Tag{"replica": {ReplicaID: "other"}}); !errors.Is(err, ErrInvalidFrontier) {
		t.Fatalf("invalid frontier error = %v, want %v", err, ErrInvalidFrontier)
	}
	if removed, err := source.Compact(map[string]crdt.Tag{}); err != nil || removed != 0 {
		t.Fatalf("Compact(empty) = %d, %v; want 0, nil", removed, err)
	}
}

func TestORSetCompactsOnlyExplicitAcknowledgedTombstones(t *testing.T) {
	t.Parallel()
	codec := stringCodec{id: "example.com/string/v1"}
	value := mustNewORSet(t, "replica", codec)
	if _, err := value.Add("first"); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Remove("first"); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Add("second"); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Remove("second"); err != nil {
		t.Fatal(err)
	}
	tags := value.TombstoneTags()
	if len(tags) != 2 || tags[0].Compare(tags[1]) >= 0 {
		t.Fatalf("TombstoneTags() = %#v, want two sorted tags", tags)
	}
	if removed, err := value.CompactTombstones(tags[:1]); err != nil || removed != 1 {
		t.Fatalf("CompactTombstones(first) = %d, %v; want 1, nil", removed, err)
	}
	if got := value.State().TombstoneCount; got != 1 {
		t.Fatalf("tombstones after exact compaction = %d, want 1", got)
	}
	if removed, err := value.CompactTombstones([]crdt.Tag{{}}); !errors.Is(err, ErrInvalidFrontier) || removed != 0 {
		t.Fatalf("CompactTombstones(invalid) = %d, %v; want 0, %v", removed, err, ErrInvalidFrontier)
	}
	if got := value.State().TombstoneCount; got != 1 {
		t.Fatalf("invalid compaction changed tombstones: %d", got)
	}
}

func TestORSetSnapshotCurrentStateCarriesDerivedFrontier(t *testing.T) {
	t.Parallel()
	codec := stringCodec{id: "example.com/string/v1"}
	value := mustNewORSet(t, "replica", codec)
	if _, err := value.Add("x"); err != nil {
		t.Fatal(err)
	}
	saved, err := value.SnapshotCurrentState()
	if err != nil {
		t.Fatalf("SnapshotCurrentState() error = %v", err)
	}
	if got, want := saved.Frontier(), value.Frontier(); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot frontier = %#v, want %#v", got, want)
	}
	if _, ok := saved.ClockState(); !ok {
		t.Fatal("snapshot did not include clock state")
	}
}

func TestORSetApplyingRemoteDeltaAdvancesLocalClock(t *testing.T) {
	t.Parallel()
	codec := stringCodec{id: "example.com/string/v1"}
	const wallTime = uint64(1 << 63)
	remote, err := NewORSetFromClock(clock.State{ReplicaID: "remote", WallTime: wallTime}, codec)
	if err != nil {
		t.Fatal(err)
	}
	remoteDelta, err := remote.Add("remote")
	if err != nil {
		t.Fatal(err)
	}
	local, err := NewORSetFromClock(clock.State{ReplicaID: "local", WallTime: wallTime}, codec)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.ApplyDelta(remoteDelta); err != nil {
		t.Fatalf("ApplyDelta() error = %v", err)
	}
	localDelta, err := local.Add("local")
	if err != nil {
		t.Fatal(err)
	}
	var remoteTag, localTag crdt.Tag
	for tag := range remoteDelta.adds["remote"] {
		remoteTag = tag
	}
	for tag := range localDelta.adds["local"] {
		localTag = tag
	}
	if remoteTag.Compare(localTag) >= 0 {
		t.Fatalf("local tag %#v did not advance past remote tag %#v", localTag, remoteTag)
	}
}

func TestORSetSnapshotRestorationDoesNotReuseTags(t *testing.T) {
	t.Parallel()
	codec := stringCodec{id: "example.com/string/v1"}
	initialClock := clock.State{ReplicaID: "replica", WallTime: 1 << 63}
	source, err := NewORSetFromClock(initialClock, codec)
	if err != nil {
		t.Fatal(err)
	}
	oldAdd, err := source.Add("old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Remove("old"); err != nil {
		t.Fatal(err)
	}
	clockState := source.ClockState()
	frontier := map[string]crdt.Tag{
		"replica": {ReplicaID: "replica", WallTime: clockState.WallTime, Logical: clockState.Logical + 5},
	}
	saved, err := source.Snapshot(frontier)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewORSetFromSnapshot(saved, codec)
	if err != nil {
		t.Fatal(err)
	}
	newAdd, err := restored.Add("new")
	if err != nil {
		t.Fatal(err)
	}
	var oldTag, newTag crdt.Tag
	for tag := range oldAdd.adds["old"] {
		oldTag = tag
	}
	for tag := range newAdd.adds["new"] {
		newTag = tag
	}
	if newTag.Compare(frontier["replica"]) <= 0 || newTag.Compare(oldTag) <= 0 {
		t.Fatalf("recovered add tag = %#v, want after frontier %#v and old tag %#v", newTag, frontier["replica"], oldTag)
	}
	if restored.Contains("old") || !restored.Contains("new") {
		t.Fatalf("restored contents old=%v new=%v", restored.Contains("old"), restored.Contains("new"))
	}
}

func TestORSetUnmarshalAdvancesClockPastRecoveredTags(t *testing.T) {
	t.Parallel()
	codec := stringCodec{id: "example.com/string/v1"}
	initialClock := clock.State{ReplicaID: "replica", WallTime: 1 << 63}
	source, err := NewORSetFromClock(initialClock, codec)
	if err != nil {
		t.Fatal(err)
	}
	oldAdd, err := source.Add("old")
	if err != nil {
		t.Fatal(err)
	}
	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewORSetFromClock(initialClock, codec)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.UnmarshalBinary(state); err != nil {
		t.Fatal(err)
	}
	newAdd, err := restored.Add("new")
	if err != nil {
		t.Fatal(err)
	}
	var oldTag, newTag crdt.Tag
	for tag := range oldAdd.adds["old"] {
		oldTag = tag
	}
	for tag := range newAdd.adds["new"] {
		newTag = tag
	}
	if newTag.Compare(oldTag) <= 0 {
		t.Fatalf("decoded-state add tag = %#v, want after %#v", newTag, oldTag)
	}
}

func TestORSetSnapshotRestoreRejectsMissingClockState(t *testing.T) {
	t.Parallel()
	codec := stringCodec{id: "example.com/string/v1"}
	source := mustNewORSet(t, "replica", codec)
	if _, err := source.Add("item"); err != nil {
		t.Fatal(err)
	}
	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := snapshot.New(state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewORSetFromSnapshot(legacy, codec); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("NewORSetFromSnapshot() error = %v, want %v", err, ErrInvalidSnapshot)
	}
}

func TestORSetMergeConvergesAcrossRandomPartitions(t *testing.T) {
	t.Parallel()
	codec := stringCodec{id: "example.com/string/v1"}
	rng := rand.New(rand.NewSource(99))
	for iteration := 0; iteration < 64; iteration++ {
		sources := []*ORSet[string]{
			mustNewORSet(t, "a", codec),
			mustNewORSet(t, "b", codec),
			mustNewORSet(t, "c", codec),
		}
		for _, source := range sources {
			for operation := 0; operation < 20; operation++ {
				element := string(rune('a' + rng.Intn(5)))
				if rng.Intn(2) == 0 {
					if _, err := source.Add(element); err != nil {
						t.Fatal(err)
					}
				} else if _, err := source.Remove(element); err != nil {
					t.Fatal(err)
				}
			}
		}

		left := cloneORSet(t, sources[0], "left", codec)
		for _, source := range sources[1:] {
			if err := left.Merge(source); err != nil {
				t.Fatal(err)
			}
		}
		right := cloneORSet(t, sources[2], "right", codec)
		for _, source := range []*ORSet[string]{sources[1], sources[0]} {
			if err := right.Merge(source); err != nil {
				t.Fatal(err)
			}
		}
		if got, want := sortedElements(left), sortedElements(right); !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: convergence = %v, want %v", iteration, got, want)
		}
		before := sortedElements(left)
		if err := left.Merge(left); err != nil {
			t.Fatal(err)
		}
		if got := sortedElements(left); !reflect.DeepEqual(got, before) {
			t.Fatalf("iteration %d: self merge = %v, want %v", iteration, got, before)
		}
	}
}

func FuzzORSetUnmarshalBinary(f *testing.F) {
	codec := stringCodec{id: "example.com/string/v1"}
	source, err := NewORSet("seed", codec)
	if err != nil {
		f.Fatal(err)
	}
	if _, err := source.Add("seed"); err != nil {
		f.Fatal(err)
	}
	seed, err := source.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte("CRDT"))
	f.Fuzz(func(t *testing.T, data []byte) {
		target := mustNewORSet(t, "target", codec)
		if err := target.UnmarshalBinary(data); err == nil {
			_ = target.State()
		}
		if delta, err := UnmarshalORSetDelta(data, codec); err == nil {
			if err := target.ApplyDelta(delta); err != nil {
				t.Fatalf("valid decoded delta rejected: %v", err)
			}
		}
	})
}

func cloneORSet(t *testing.T, source *ORSet[string], replicaID string, codec ElementCodec[string]) *ORSet[string] {
	t.Helper()
	encoded, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	clone := mustNewORSet(t, replicaID, codec)
	if err := clone.UnmarshalBinary(encoded); err != nil {
		t.Fatal(err)
	}
	return clone
}

func sortedElements(set *ORSet[string]) []string {
	elements := set.Elements()
	sort.Strings(elements)
	return elements
}

func mustNewORSet(t *testing.T, replicaID string, codec ElementCodec[string]) *ORSet[string] {
	t.Helper()
	set, err := NewORSet(replicaID, codec)
	if err != nil {
		t.Fatalf("NewORSet() error = %v", err)
	}
	return set
}
