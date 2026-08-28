package tombstonegc

import (
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/text"
)

func TestSimpleCollectorCollectsBoundedCanonicalExcess(t *testing.T) {
	tags := []crdt.Tag{
		{ReplicaID: "cache", WallTime: 5},
		{ReplicaID: "cache", WallTime: 1},
		{ReplicaID: "cache", WallTime: 4},
		{ReplicaID: "cache", WallTime: 2},
		{ReplicaID: "cache", WallTime: 3},
	}
	target := &simpleEligibleTarget{tags: tags}
	collector, err := NewSimpleCollector(SimplePolicy{MinRetained: 1, MaxBatch: 2})
	if err != nil {
		t.Fatal(err)
	}

	removed, err := collector.Collect(target)
	if err != nil || removed != 2 {
		t.Fatalf("Collect() = %d, %v; want 2, nil", removed, err)
	}
	wantCompacted := []crdt.Tag{{ReplicaID: "cache", WallTime: 1}, {ReplicaID: "cache", WallTime: 2}}
	if !reflect.DeepEqual(target.compacted, wantCompacted) {
		t.Fatalf("compacted tags = %#v, want %#v", target.compacted, wantCompacted)
	}
	if target.eligibleCalls != 1 || target.plainCalls != 0 {
		t.Fatalf("compactor calls = eligible %d, plain %d; want eligible 1, plain 0", target.eligibleCalls, target.plainCalls)
	}
	if got := target.TombstoneTags(); len(got) != 3 {
		t.Fatalf("retained tags = %#v, want three", got)
	}
}

func TestSimpleCollectorCanonicalizesDuplicateTargetTags(t *testing.T) {
	target := &simpleEligibleTarget{tags: []crdt.Tag{
		{ReplicaID: "cache", WallTime: 3},
		{ReplicaID: "cache", WallTime: 1},
		{ReplicaID: "cache", WallTime: 1},
		{ReplicaID: "cache", WallTime: 2},
	}}
	collector, err := NewSimpleCollector(SimplePolicy{MinRetained: 1, MaxBatch: 2})
	if err != nil {
		t.Fatal(err)
	}

	if removed, err := collector.Collect(target); err != nil || removed != 2 {
		t.Fatalf("Collect() = %d, %v; want 2, nil", removed, err)
	}
	want := []crdt.Tag{{ReplicaID: "cache", WallTime: 1}, {ReplicaID: "cache", WallTime: 2}}
	if !reflect.DeepEqual(target.compacted, want) {
		t.Fatalf("compacted tags = %#v, want %#v", target.compacted, want)
	}
}

func TestSimpleCollectorRetainsConfiguredFloorAcrossCalls(t *testing.T) {
	value := mustSet(t, "cache", stringCodec{})
	for index := 0; index < 5; index++ {
		item := string(rune('a' + index))
		if _, err := value.Add(item); err != nil {
			t.Fatal(err)
		}
		if _, err := value.Remove(item); err != nil {
			t.Fatal(err)
		}
	}
	collector, err := NewSimpleCollector(SimplePolicy{MinRetained: 2, MaxBatch: 2})
	if err != nil {
		t.Fatal(err)
	}

	if removed, err := collector.Collect(value); err != nil || removed != 2 {
		t.Fatalf("first Collect() = %d, %v; want 2, nil", removed, err)
	}
	if got := len(value.TombstoneTags()); got != 3 {
		t.Fatalf("tombstones after first collection = %d, want 3", got)
	}
	if removed, err := collector.Collect(value); err != nil || removed != 1 {
		t.Fatalf("second Collect() = %d, %v; want 1, nil", removed, err)
	}
	if got := len(value.TombstoneTags()); got != 2 {
		t.Fatalf("tombstones after second collection = %d, want 2", got)
	}
	if removed, err := collector.Collect(value); err != nil || removed != 0 {
		t.Fatalf("floor Collect() = %d, %v; want 0, nil", removed, err)
	}
}

func TestSimpleCollectorUsesEligibleCompactionForLocalDeletedRGAChain(t *testing.T) {
	value, err := text.New("cache")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Insert(0, "abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Delete(0, 3); err != nil {
		t.Fatal(err)
	}
	collector, err := NewSimpleCollector(SimplePolicy{MaxBatch: 3})
	if err != nil {
		t.Fatal(err)
	}

	if removed, err := collector.Collect(value); err != nil || removed != 3 {
		t.Fatalf("Collect() = %d, %v; want 3, nil", removed, err)
	}
	if state := value.State(); state.ElementCount != 0 || state.TombstoneCount != 0 {
		t.Fatalf("state after local collection = %#v", state)
	}
}

func TestSimpleCollectorRejectsInvalidInputsWithoutCompaction(t *testing.T) {
	for _, policy := range []SimplePolicy{
		{},
		{MinRetained: -1, MaxBatch: 1},
		{MaxBatch: MaxSimpleBatch + 1},
	} {
		if _, err := NewSimpleCollector(policy); !errors.Is(err, ErrInvalidSimplePolicy) {
			t.Fatalf("NewSimpleCollector(%#v) error = %v, want %v", policy, err, ErrInvalidSimplePolicy)
		}
	}
	collector, err := NewSimpleCollector(DefaultSimplePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := collector.Policy(), DefaultSimplePolicy(); got != want {
		t.Fatalf("Policy() = %#v, want %#v", got, want)
	}
	if removed, err := collector.Collect(nil); !errors.Is(err, ErrNilTarget) || removed != 0 {
		t.Fatalf("Collect(nil) = %d, %v; want 0, %v", removed, err, ErrNilTarget)
	}
	invalid := &simplePlainTarget{tags: []crdt.Tag{{}}}
	if removed, err := collector.Collect(invalid); !errors.Is(err, ErrInvalidTag) || removed != 0 {
		t.Fatalf("Collect(invalid) = %d, %v; want 0, %v", removed, err, ErrInvalidTag)
	}
	if invalid.calls != 0 {
		t.Fatalf("invalid target compaction calls = %d, want 0", invalid.calls)
	}
	var nilCollector *SimpleCollector
	if removed, err := nilCollector.Collect(invalid); !errors.Is(err, ErrNilSimpleCollector) || removed != 0 {
		t.Fatalf("nil Collect() = %d, %v; want 0, %v", removed, err, ErrNilSimpleCollector)
	}
	if got := nilCollector.Policy(); got != (SimplePolicy{}) {
		t.Fatalf("nil Policy() = %#v, want zero", got)
	}
}

// This is a safety-boundary regression test, not a supported replication
// workflow. It demonstrates why SimpleCollector is limited to local-only
// disposable state: delayed replicated operations can resurrect after local
// tombstone metadata is discarded.
func TestSimpleCollectorDoesNotAuthorizeReplicatedDeltaRetirement(t *testing.T) {
	source := mustSet(t, "source", stringCodec{})
	remote := mustSet(t, "remote", stringCodec{})
	add, err := source.Add("old")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyDelta(add); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Remove("old"); err != nil {
		t.Fatal(err)
	}
	collector, err := NewSimpleCollector(SimplePolicy{MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := collector.Collect(source); err != nil || removed != 1 {
		t.Fatalf("Collect() = %d, %v; want 1, nil", removed, err)
	}
	if err := source.Merge(remote); err != nil {
		t.Fatal(err)
	}
	if !source.Contains("old") {
		t.Fatal("local-only collection unexpectedly protected against delayed remote add")
	}
}

func TestSimpleCollectorConcurrentLocalCollection(t *testing.T) {
	value := mustSet(t, "cache", stringCodec{})
	for index := 0; index < 64; index++ {
		item := string(rune('a' + index))
		if _, err := value.Add(item); err != nil {
			t.Fatal(err)
		}
		if _, err := value.Remove(item); err != nil {
			t.Fatal(err)
		}
	}
	collector, err := NewSimpleCollector(SimplePolicy{MaxBatch: 8})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		for index := 0; index < 64; index++ {
			if _, err := collector.Collect(value); err != nil {
				errs <- err
				return
			}
		}
	}()
	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		for index := 0; index < 64; index++ {
			item := string(rune(0x100 + index))
			if _, err := value.Add(item); err != nil {
				errs <- err
				return
			}
			if _, err := value.Remove(item); err != nil {
				errs <- err
				return
			}
		}
	}()
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent simple collection: %v", err)
		}
	}
}

func FuzzNewSimpleCollector(f *testing.F) {
	f.Add(0, 1)
	f.Add(DefaultSimpleMinRetained, DefaultSimpleMaxBatch)
	f.Add(-1, 1)
	f.Add(0, MaxSimpleBatch+1)
	f.Fuzz(func(t *testing.T, minRetained, maxBatch int) {
		collector, err := NewSimpleCollector(SimplePolicy{MinRetained: minRetained, MaxBatch: maxBatch})
		valid := minRetained >= 0 && maxBatch > 0 && maxBatch <= MaxSimpleBatch
		if valid {
			if err != nil || collector == nil {
				t.Fatalf("NewSimpleCollector(%d, %d) = %#v, %v; want collector, nil", minRetained, maxBatch, collector, err)
			}
			if got := collector.Policy(); got.MinRetained != minRetained || got.MaxBatch != maxBatch {
				t.Fatalf("Policy() = %#v, want {%d %d}", got, minRetained, maxBatch)
			}
			return
		}
		if !errors.Is(err, ErrInvalidSimplePolicy) || collector != nil {
			t.Fatalf("NewSimpleCollector(%d, %d) = %#v, %v; want nil, %v", minRetained, maxBatch, collector, err, ErrInvalidSimplePolicy)
		}
	})
}

type simplePlainTarget struct {
	tags  []crdt.Tag
	calls int
}

func (t *simplePlainTarget) TombstoneTags() []crdt.Tag {
	return append([]crdt.Tag(nil), t.tags...)
}

func (t *simplePlainTarget) CompactTombstones(tags []crdt.Tag) (int, error) {
	t.calls++
	return len(tags), nil
}

type simpleEligibleTarget struct {
	tags          []crdt.Tag
	compacted     []crdt.Tag
	plainCalls    int
	eligibleCalls int
}

func (t *simpleEligibleTarget) TombstoneTags() []crdt.Tag {
	return append([]crdt.Tag(nil), t.tags...)
}

func (t *simpleEligibleTarget) CompactTombstones(tags []crdt.Tag) (int, error) {
	t.plainCalls++
	return t.compact(tags), nil
}

func (t *simpleEligibleTarget) CompactEligibleTombstones(tags []crdt.Tag) (int, error) {
	t.eligibleCalls++
	return t.compact(tags), nil
}

func (t *simpleEligibleTarget) compact(tags []crdt.Tag) int {
	t.compacted = append(t.compacted, tags...)
	retained := make([]crdt.Tag, 0, len(t.tags)-len(tags))
	for _, existing := range t.tags {
		remove := false
		for _, candidate := range tags {
			if existing == candidate {
				remove = true
				break
			}
		}
		if !remove {
			retained = append(retained, existing)
		}
	}
	t.tags = retained
	return len(tags)
}
