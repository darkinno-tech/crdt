package lww

import (
	"errors"
	"reflect"
	"testing"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
)

func TestStableLWWFrameTypes(t *testing.T) {
	if SemanticsVersion != 1 {
		t.Fatalf("SemanticsVersion = %d, want 1", SemanticsVersion)
	}
	if got, want := SetFrameType(), (crdt.FrameType{StateID: crdt.TypeIDLWWSetState, DeltaID: crdt.TypeIDLWWSetDelta, SemanticsVersion: SemanticsVersion, UsesHLC: true}); got != want {
		t.Fatalf("SetFrameType() = %#v, want %#v", got, want)
	}
	if got, want := MapFrameType(), (crdt.FrameType{StateID: crdt.TypeIDLWWMapState, DeltaID: crdt.TypeIDLWWMapDelta, SemanticsVersion: crdt.SemanticsVersionLWWMap, UsesHLC: true}); got != want {
		t.Fatalf("MapFrameType() = %#v, want %#v", got, want)
	}
}

func TestSetOptionsTagUniquenessAndCompaction(t *testing.T) {
	options := SetOptions{MaxEntries: 1}
	value, err := NewSetWithOptions[string]("local", options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.AddWithDelta("one"); err != nil {
		t.Fatal(err)
	}
	before := value.ClockState()
	if _, err := value.AddWithDelta("two"); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("AddWithDelta at capacity = %v", err)
	}
	if after := value.ClockState(); after != before {
		t.Fatalf("rejected local write advanced clock: before=%#v after=%#v", before, after)
	}
	if _, err := value.RemoveWithDelta("one"); err != nil {
		t.Fatal(err)
	}
	tags := value.TombstoneTags()
	if len(tags) != 1 {
		t.Fatalf("tombstone tags = %#v", tags)
	}
	if removed, err := value.CompactTombstones(tags); err != nil || removed != 1 {
		t.Fatalf("CompactTombstones() = %d, %v", removed, err)
	}
	if state := value.State(); state.ElementCount != 0 || state.TombstoneCount != 0 {
		t.Fatalf("compacted state = %#v", state)
	}

	conflict := crdt.Tag{ReplicaID: "remote", WallTime: 1}
	if err := value.ApplyDelta(SetDelta[string]{entries: map[string]setEntry[string]{
		"left":  {tag: conflict, present: true},
		"right": {tag: conflict, present: true},
	}}); !errors.Is(err, ErrTagConflict) {
		t.Fatalf("cross-element tag collision = %v", err)
	}
	if _, err := NewSetWithOptions[string]("invalid", SetOptions{}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid set options = %v", err)
	}
}

func TestMapOptionsTagUniquenessAndCompaction(t *testing.T) {
	options := MapOptions{MaxEntries: 1, MaxKeyBytes: 4, MaxValueBytes: 2}
	value, err := NewMapWithOptions("local", options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.SetWithDelta("one", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	before := value.ClockState()
	if _, err := value.SetWithDelta("two", []byte("ok")); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("SetWithDelta at capacity = %v", err)
	}
	if after := value.ClockState(); after != before {
		t.Fatalf("rejected local write advanced clock: before=%#v after=%#v", before, after)
	}
	if _, err := value.SetWithDelta("longer", []byte("ok")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("oversized key = %v", err)
	}
	if _, err := value.SetWithDelta("one", []byte("long")); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("oversized value = %v", err)
	}
	if _, err := value.DeleteWithDelta("one"); err != nil {
		t.Fatal(err)
	}
	tags := value.TombstoneTags()
	if removed, err := value.CompactTombstones(tags); err != nil || removed != 1 {
		t.Fatalf("CompactTombstones() = %d, %v", removed, err)
	}
	if _, err := value.SetWithDelta("two", []byte("ok")); err != nil {
		t.Fatal(err)
	}
	live := value.Frontier()["local"]
	if removed, err := value.CompactTombstones([]crdt.Tag{live}); !errors.Is(err, ErrUnsafeCompaction) || removed != 0 {
		t.Fatalf("live-entry compaction = %d, %v", removed, err)
	}
	if got, ok := value.Get("two"); !ok || string(got) != "ok" {
		t.Fatalf("live entry changed after rejected compaction: %q, %v", got, ok)
	}

	conflict := crdt.Tag{ReplicaID: "remote", WallTime: 1}
	if err := value.ApplyDelta(MapDelta{entries: map[string]mapEntry{
		"one": {tag: conflict, present: true, value: []byte("a")},
		"two": {tag: conflict, present: true, value: []byte("b")},
	}}); !errors.Is(err, ErrTagConflict) {
		t.Fatalf("cross-key tag collision = %v", err)
	}
	if _, err := NewMapWithOptions("invalid", MapOptions{}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid map options = %v", err)
	}
}

func TestLWWSnapshotRespectsReceiverLimitsAtomically(t *testing.T) {
	source, err := NewMap("source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.SetWithDelta("a", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := source.SetWithDelta("b", []byte("two")); err != nil {
		t.Fatal(err)
	}
	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	target, err := NewMapWithOptions("target", MapOptions{MaxEntries: 1, MaxKeyBytes: 8, MaxValueBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	before, err := target.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	beforeClock := target.ClockState()
	if err := target.UnmarshalBinary(state); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("over-limit state = %v", err)
	}
	after, err := target.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) || target.ClockState() != beforeClock {
		t.Fatalf("rejected state changed target: state=%x clock=%#v", after, target.ClockState())
	}
	saved, err := source.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewMapFromSnapshotWithOptions(saved, MapOptions{MaxEntries: 1, MaxKeyBytes: 8, MaxValueBytes: 8}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("over-limit snapshot restore = %v", err)
	}
	setSource, err := NewSet[string]("set-source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setSource.AddWithDelta("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := setSource.AddWithDelta("b"); err != nil {
		t.Fatal(err)
	}
	setSnapshot, err := setSource.SnapshotCurrentState(setStringCodec{id: "limits"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSetFromSnapshotWithOptions(setSnapshot, setStringCodec{id: "limits"}, SetOptions{MaxEntries: 1}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("over-limit set snapshot restore = %v", err)
	}
}

func TestLWWWiresRejectSharedMutationTags(t *testing.T) {
	tag := crdt.Tag{ReplicaID: "remote", WallTime: 7, Logical: 1}
	mapPayload := frame.AppendUvarint(nil, 2)
	for _, key := range []string{"a", "b"} {
		mapPayload = frame.AppendUvarint(mapPayload, uint64(len(key)))
		mapPayload = append(mapPayload, key...)
		mapPayload = frame.AppendTag(mapPayload, tag)
		mapPayload = frame.AppendUvarint(mapPayload, 0)
	}
	mapFrame, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDLWWMapDelta, Payload: mapPayload})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalMapDelta(mapFrame); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("map shared tag = %v", err)
	}

	codec := setStringCodec{id: "example.com/lww-set-shared-tag/v1"}
	setPayload := frame.AppendUvarint(nil, 2)
	for _, value := range []string{"a", "b"} {
		setPayload = frame.AppendUvarint(setPayload, uint64(len(value)))
		setPayload = append(setPayload, value...)
		setPayload = frame.AppendTag(setPayload, tag)
		setPayload = frame.AppendUvarint(setPayload, 1)
	}
	setFrame, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDLWWSetDelta, CodecID: codec.ID(), Payload: setPayload})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalSetDelta(setFrame, codec); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("set shared tag = %v", err)
	}
}

func TestLWWStabilityBoundaryPaths(t *testing.T) {
	set, err := NewSet[string]("set")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.AddWithDelta("live"); err != nil {
		t.Fatal(err)
	}
	liveSetTag := set.entries["live"].tag
	if err := set.ApplyDelta(SetDelta[string]{entries: map[string]setEntry[string]{
		"other": {tag: liveSetTag, present: true},
	}}); !errors.Is(err, ErrTagConflict) {
		t.Fatalf("existing set tag collision = %v", err)
	}
	if removed, err := set.CompactTombstones([]crdt.Tag{liveSetTag}); !errors.Is(err, ErrUnsafeCompaction) || removed != 0 {
		t.Fatalf("live set compaction = %d, %v", removed, err)
	}
	if removed, err := set.CompactTombstones([]crdt.Tag{{}}); !errors.Is(err, ErrUnsafeCompaction) || removed != 0 {
		t.Fatalf("invalid set compaction = %d, %v", removed, err)
	}
	if removed, err := set.CompactTombstones([]crdt.Tag{{ReplicaID: "missing", WallTime: 1}}); err != nil || removed != 0 {
		t.Fatalf("unknown set compaction = %d, %v", removed, err)
	}
	if _, err := set.MarshalJSON(); err != nil {
		t.Fatal(err)
	}
	if _, err := (SetDelta[string]{entries: map[string]setEntry[string]{"deleted": {tag: crdt.Tag{ReplicaID: "delta"}, present: false}}}).MarshalJSON(); err != nil {
		t.Fatal(err)
	}

	value, err := NewMap("map")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.SetWithDelta("live", []byte("value")); err != nil {
		t.Fatal(err)
	}
	liveMapTag := value.entries["live"].tag
	if err := value.ApplyDelta(MapDelta{entries: map[string]mapEntry{
		"other": {tag: liveMapTag, present: true, value: []byte("value")},
	}}); !errors.Is(err, ErrTagConflict) {
		t.Fatalf("existing map tag collision = %v", err)
	}
	if removed, err := value.CompactTombstones([]crdt.Tag{liveMapTag}); !errors.Is(err, ErrUnsafeCompaction) || removed != 0 {
		t.Fatalf("live map compaction = %d, %v", removed, err)
	}
	if removed, err := value.CompactTombstones([]crdt.Tag{{}}); !errors.Is(err, ErrUnsafeCompaction) || removed != 0 {
		t.Fatalf("invalid map compaction = %d, %v", removed, err)
	}
	if removed, err := value.CompactTombstones([]crdt.Tag{{ReplicaID: "missing", WallTime: 1}}); err != nil || removed != 0 {
		t.Fatalf("unknown map compaction = %d, %v", removed, err)
	}
	if _, err := value.MarshalJSON(); err != nil {
		t.Fatal(err)
	}
	if _, err := (MapDelta{entries: map[string]mapEntry{"deleted": {tag: crdt.Tag{ReplicaID: "delta"}, present: false}}}).MarshalJSON(); err != nil {
		t.Fatal(err)
	}

	setSnapshot, err := set.SnapshotCurrentState(setStringCodec{id: "options"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSetFromSnapshotWithOptions(setSnapshot, setStringCodec{id: "options"}, SetOptions{}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid set snapshot options = %v", err)
	}
	mapSnapshot, err := value.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewMapFromSnapshotWithOptions(mapSnapshot, MapOptions{}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid map snapshot options = %v", err)
	}
}
