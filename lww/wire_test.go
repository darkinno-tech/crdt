package lww

import (
	"bytes"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/clock"
	"github.com/darkinno-tech/crdt/delta"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/snapshot"
)

func TestMapDeltaConvergesAcrossDuplicateAndReverseDelivery(t *testing.T) {
	source, err := NewMap("source")
	if err != nil {
		t.Fatal(err)
	}
	write, err := source.SetWithDelta("title", []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	remove, err := source.DeleteWithDelta("title")
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewMap("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(remove); err != nil {
		t.Fatal(err)
	}
	clockBeforeDuplicate := target.ClockState()
	if err := target.ApplyDelta(remove); err != nil {
		t.Fatal(err)
	}
	if target.ClockState() != clockBeforeDuplicate {
		t.Fatal("duplicate delta advanced the persisted HLC state")
	}
	if err := target.ApplyDelta(write); err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(remove); err != nil {
		t.Fatal(err)
	}
	if _, ok := target.Get("title"); ok {
		t.Fatal("older write resurrected a delete")
	}
	if err := source.Merge(target); err != nil {
		t.Fatal(err)
	}
	if source.State().ElementCount != target.State().ElementCount || source.State().TombstoneCount != target.State().TombstoneCount {
		t.Fatalf("replicas diverged: %#v %#v", source.State(), target.State())
	}

	merged, err := write.Merge(remove)
	if err != nil {
		t.Fatal(err)
	}
	third, err := NewMap("third")
	if err != nil {
		t.Fatal(err)
	}
	if err := third.ApplyDelta(merged); err != nil {
		t.Fatal(err)
	}
	if _, ok := third.Get("title"); ok {
		t.Fatal("merged delta resurrected a delete")
	}
}

func TestMapWireGoldenStateDeltaAndSnapshot(t *testing.T) {
	// This frame is deliberately built without MapDelta.MarshalBinary. It fixes
	// the public payload order: count, key, tag, present flag, and value.
	payload := frame.AppendUvarint(nil, 1)
	payload = frame.AppendUvarint(payload, 1)
	payload = append(payload, 'a')
	payload = frame.AppendTag(payload, crdt.Tag{ReplicaID: "replica", WallTime: 7, Logical: 2})
	payload = frame.AppendUvarint(payload, 1)
	payload = frame.AppendUvarint(payload, 1)
	payload = append(payload, 'x')
	golden, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDLWWMapDelta, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalMapDelta(golden)
	if err != nil {
		t.Fatal(err)
	}
	if encoded, err := decoded.MarshalBinary(); err != nil || !bytes.Equal(encoded, golden) {
		t.Fatalf("delta re-encoding = %x, %v; want %x", encoded, err, golden)
	}
	target, err := NewMap("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
	if got, ok := target.Get("a"); !ok || string(got) != "x" {
		t.Fatalf("golden delta value = %q, %v", got, ok)
	}

	if err := target.Set("z", []byte("last")); err != nil {
		t.Fatal(err)
	}
	first, err := target.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	second, err := target.MarshalBinary()
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("state encoding is not canonical: %v", err)
	}
	restoredState, err := NewMap("restored-state")
	if err != nil {
		t.Fatal(err)
	}
	if err := restoredState.UnmarshalBinary(first); err != nil {
		t.Fatal(err)
	}
	if got, ok := restoredState.Get("z"); !ok || string(got) != "last" {
		t.Fatalf("state restore = %q, %v", got, ok)
	}
	saved, err := target.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewMapFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := restored.Get("a"); !ok || string(got) != "x" {
		t.Fatalf("snapshot restore = %q, %v", got, ok)
	}
	if got, want := saved.Frontier(), target.Frontier(); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot frontier = %#v, want %#v", got, want)
	}
	if got, ok := saved.ClockState(); !ok || got != target.ClockState() {
		t.Fatalf("snapshot clock = %#v, %v", got, ok)
	}
	if _, err := snapshot.NewRecoveryPlan(saved, [][]byte{golden}, len(golden)); err != nil {
		t.Fatalf("LWW-Map recovery plan = %v", err)
	}
}

func TestMapSnapshotRecoveryWitnessesSuppliedFrontier(t *testing.T) {
	value, err := NewMap("local")
	if err != nil {
		t.Fatal(err)
	}
	if err := value.Set("saved", []byte("value")); err != nil {
		t.Fatal(err)
	}
	future := crdt.Tag{ReplicaID: "remote", WallTime: 10_000_000_000_000, Logical: 3}
	saved, err := value.Snapshot(map[string]crdt.Tag{"remote": future})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewMapFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restored.SetWithDelta("after-recovery", []byte("value")); err != nil {
		t.Fatal(err)
	}
	if got := restored.entries["after-recovery"].tag; got.Compare(future) <= 0 {
		t.Fatalf("post-recovery tag = %#v, want greater than supplied frontier %#v", got, future)
	}
}

func TestMapWireRejectsMalformedInputWithoutMutation(t *testing.T) {
	value, err := NewMap("local")
	if err != nil {
		t.Fatal(err)
	}
	if err := value.Set("safe", []byte("value")); err != nil {
		t.Fatal(err)
	}
	before, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	wrongType, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRGAState})
	if err != nil {
		t.Fatal(err)
	}
	if err := value.UnmarshalBinary(wrongType); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("wrong type = %v", err)
	}
	// Entries must be ordered. This payload lists b then a.
	payload := frame.AppendUvarint(nil, 2)
	for _, key := range []string{"b", "a"} {
		payload = frame.AppendUvarint(payload, 1)
		payload = append(payload, key...)
		payload = frame.AppendTag(payload, crdt.Tag{ReplicaID: "remote", WallTime: 1})
		payload = frame.AppendUvarint(payload, 0)
	}
	unsorted, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDLWWMapState, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := value.UnmarshalBinary(unsorted); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("unsorted state = %v", err)
	}
	if after, err := value.MarshalBinary(); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("receiver changed after rejected state: %v", err)
	}
	limits := frame.DefaultLimits()
	limits.MaxElements = 0
	if err := value.UnmarshalBinaryWithLimits(before, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("limit error = %v", err)
	}
	if _, err := UnmarshalMapDelta(before); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("state accepted as delta: %v", err)
	}
}

func TestMapWireRejectsImpossibleEntryCount(t *testing.T) {
	payload := frame.AppendUvarint(nil, uint64(frame.DefaultLimits().MaxElements))
	for _, typeID := range []uint64{crdt.TypeIDLWWMapState, crdt.TypeIDLWWMapDelta} {
		encoded, err := frame.MarshalFrame(frame.Frame{TypeID: typeID, Payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := unmarshalMap(encoded, typeID, frame.DefaultLimits()); !errors.Is(err, frame.ErrInvalidFrame) {
			t.Fatalf("type %d impossible entry count = %v", typeID, err)
		}
	}
}

func TestMapDeltaCoalescerAndErrorPaths(t *testing.T) {
	value, err := NewMap("source")
	if err != nil {
		t.Fatal(err)
	}
	first, err := value.SetWithDelta("a", []byte("one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := value.SetWithDelta("b", []byte("two"))
	if err != nil {
		t.Fatal(err)
	}
	encodedFirst, err := first.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	encodedSecond, err := second.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	coalescer, err := delta.NewCoalescer(1, len(encodedFirst)+len(encodedSecond)+128, func(left, right []byte) ([]byte, error) {
		leftDelta, err := UnmarshalMapDelta(left)
		if err != nil {
			return nil, err
		}
		rightDelta, err := UnmarshalMapDelta(right)
		if err != nil {
			return nil, err
		}
		merged, err := leftDelta.Merge(rightDelta)
		if err != nil {
			return nil, err
		}
		return merged.MarshalBinary()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coalescer.Add(encodedFirst); err != nil {
		t.Fatal(err)
	}
	if err := coalescer.Add(encodedSecond); err != nil {
		t.Fatal(err)
	}
	items := coalescer.Drain().Items()
	if len(items) != 1 {
		t.Fatalf("coalesced item count = %d", len(items))
	}
	merged, err := UnmarshalMapDelta(items[0])
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewMap("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(merged); err != nil {
		t.Fatal(err)
	}
	if got, ok := target.Get("a"); !ok || string(got) != "one" {
		t.Fatalf("coalesced a = %q, %v", got, ok)
	}
	if got, ok := target.Get("b"); !ok || string(got) != "two" {
		t.Fatalf("coalesced b = %q, %v", got, ok)
	}
	conflictTag := crdt.Tag{ReplicaID: "conflict", WallTime: 1}
	if err := target.ApplyDelta(MapDelta{entries: map[string]mapEntry{
		"conflict": {tag: conflictTag, present: true, value: []byte("left")},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(MapDelta{entries: map[string]mapEntry{
		"conflict": {tag: conflictTag, present: true, value: []byte("right")},
	}}); err != ErrTagConflict {
		t.Fatalf("equal-tag delta conflict = %v", err)
	}
	if got, ok := target.Get("conflict"); !ok || string(got) != "left" {
		t.Fatalf("conflict changed receiver: %q, %v", got, ok)
	}

	var nilMap *Map
	if _, err := nilMap.SetWithDelta("x", nil); err != ErrNilMap {
		t.Fatalf("nil SetWithDelta = %v", err)
	}
	if err := nilMap.ApplyDelta(MapDelta{}); err != ErrNilMap {
		t.Fatalf("nil ApplyDelta = %v", err)
	}
	empty, err := (MapDelta{}).MarshalBinary()
	if err != nil {
		t.Fatalf("zero delta marshal = %v", err)
	}
	emptyDelta, err := UnmarshalMapDelta(empty)
	if err != nil {
		t.Fatalf("zero delta decode = %v", err)
	}
	if err := target.ApplyDelta(emptyDelta); err != nil {
		t.Fatalf("zero delta apply = %v", err)
	}
	if _, err := NewMapFromSnapshot(snapshot.Snapshot{TypeID: crdt.TypeIDGCounterState}); err != ErrInvalidSnapshot {
		t.Fatalf("wrong snapshot = %v", err)
	}
	if _, err := NewMapFromSnapshot(snapshot.Snapshot{TypeID: crdt.TypeIDLWWMapState}); err != ErrInvalidSnapshot {
		t.Fatalf("snapshot without clock = %v", err)
	}
	if _, err := NewMapFromClock(clock.State{}); err != ErrInvalidReplicaID {
		t.Fatalf("clock state = %v", err)
	}
}

func TestMapWireBoundaryAndNilPaths(t *testing.T) {
	var nilMap *Map
	if _, err := nilMap.MarshalBinary(); err != ErrNilMap {
		t.Fatalf("nil MarshalBinary = %v", err)
	}
	if _, _, err := nilMap.MarshalBinaryWithClockState(); err != ErrNilMap {
		t.Fatalf("nil MarshalBinaryWithClockState = %v", err)
	}
	if _, err := nilMap.Snapshot(nil); err != ErrNilMap {
		t.Fatalf("nil Snapshot = %v", err)
	}
	if _, err := nilMap.SnapshotCurrentState(); err != ErrNilMap {
		t.Fatalf("nil SnapshotCurrentState = %v", err)
	}
	if err := nilMap.UnmarshalBinary(nil); err != ErrNilMap {
		t.Fatalf("nil UnmarshalBinary = %v", err)
	}
	if nilMap.Frontier() != nil {
		t.Fatal("nil Frontier is not nil")
	}

	validTag := crdt.Tag{ReplicaID: "remote", WallTime: 1}
	invalid := map[string]mapEntry{" ": {tag: validTag, present: true}}
	if _, err := marshalMapWithLimits(crdt.TypeIDLWWMapState, invalid, frame.DefaultLimits()); err != ErrInvalidDelta {
		t.Fatalf("invalid entry marshal = %v", err)
	}
	if _, err := marshalMapWithLimits(999, map[string]mapEntry{}, frame.DefaultLimits()); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("invalid type marshal = %v", err)
	}
	limits := frame.DefaultLimits()
	limits.MaxElements = 0
	if _, err := marshalMapWithLimits(crdt.TypeIDLWWMapState, map[string]mapEntry{"a": {tag: validTag, present: true}}, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("element limit marshal = %v", err)
	}
	limits = frame.DefaultLimits()
	limits.MaxStringBytes = 1
	if _, err := marshalMapWithLimits(crdt.TypeIDLWWMapState, map[string]mapEntry{"a": {tag: validTag, present: true, value: []byte("too long")}}, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("value limit marshal = %v", err)
	}

	value, err := NewMap("local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.SetWithDelta("a", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := value.MarshalBinaryWithClockState(); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Snapshot(map[string]crdt.Tag{"wrong": {ReplicaID: "other"}}); err == nil {
		t.Fatal("invalid supplied frontier accepted")
	}
	if _, err := value.Snapshot(value.Frontier()); err != nil {
		t.Fatal(err)
	}
	badState, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDLWWMapState, Payload: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	badSnapshot, err := snapshot.NewWithClockState(badState, nil, value.ClockState())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewMapFromSnapshot(badSnapshot); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("bad snapshot restore = %v", err)
	}
}

func TestMapConcurrentWritesReadsAndFrames(t *testing.T) {
	value, err := NewMap("concurrent")
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for index := 0; index < 100; index++ {
				key := string(rune('a' + worker))
				if _, err := value.SetWithDelta(key, []byte{byte(index)}); err != nil {
					t.Errorf("SetWithDelta() = %v", err)
				}
				_, _ = value.Get(key)
				if _, err := value.MarshalBinary(); err != nil {
					t.Errorf("MarshalBinary() = %v", err)
				}
			}
		}(worker)
	}
	group.Wait()
}

func FuzzMapUnmarshal(f *testing.F) {
	value, err := NewMap("seed")
	if err != nil {
		f.Fatal(err)
	}
	delta, err := value.SetWithDelta("seed", []byte("value"))
	if err != nil {
		f.Fatal(err)
	}
	state, err := value.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	encodedDelta, err := delta.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	impossibleCount, err := frame.MarshalFrame(frame.Frame{
		TypeID:  crdt.TypeIDLWWMapState,
		Payload: frame.AppendUvarint(nil, uint64(frame.DefaultLimits().MaxElements)),
	})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(state)
	f.Add(encodedDelta)
	f.Add(impossibleCount)
	f.Fuzz(func(t *testing.T, data []byte) {
		target, err := NewMap("target")
		if err != nil {
			t.Fatal(err)
		}
		if err := target.UnmarshalBinary(data); err == nil && target.State().ElementCount < 0 {
			t.Fatal("negative element count")
		}
		if decoded, err := UnmarshalMapDelta(data); err == nil {
			if err := target.ApplyDelta(decoded); err != nil {
				t.Fatalf("decoded delta rejected: %v", err)
			}
		}
	})
}
