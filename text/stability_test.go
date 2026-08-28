package text

import (
	"testing"

	"github.com/im10furry/crdt"
)

func TestStableFrameTypeUsesRunV2Contract(t *testing.T) {
	kind := StableFrameType()
	if kind != crdt.DefaultRGAFrameType() {
		t.Fatalf("StableFrameType() = %#v, want %#v", kind, crdt.DefaultRGAFrameType())
	}
	if RunV2SemanticsVersion != 2 {
		t.Fatalf("RunV2SemanticsVersion = %d, want 2", RunV2SemanticsVersion)
	}
}

func TestLegacyFrameTypeUsesScalarV1Contract(t *testing.T) {
	if LegacySemanticsVersion != 1 {
		t.Fatalf("LegacySemanticsVersion = %d, want 1", LegacySemanticsVersion)
	}
	if got, want := LegacyFrameType(), (crdt.FrameType{StateID: crdt.TypeIDRGAState, DeltaID: crdt.TypeIDRGADelta, SemanticsVersion: LegacySemanticsVersion, UsesHLC: true}); got != want {
		t.Fatalf("LegacyFrameType() = %#v, want %#v", got, want)
	}
}

func TestPackedFrameTypeUsesDistinctV3Contract(t *testing.T) {
	if PackedV3SemanticsVersion != 3 {
		t.Fatalf("PackedV3SemanticsVersion = %d, want 3", PackedV3SemanticsVersion)
	}
	if got, want := PackedFrameType(), (crdt.FrameType{StateID: crdt.TypeIDRGAPackedState, DeltaID: crdt.TypeIDRGAPackedDelta, SemanticsVersion: PackedV3SemanticsVersion, UsesHLC: true}); got != want {
		t.Fatalf("PackedFrameType() = %#v, want %#v", got, want)
	}
	if PackedFrameType() == StableFrameType() {
		t.Fatal("packed-v3 unexpectedly aliases default run-v2")
	}
}

func TestRGARetainsPositionUntilCompaction(t *testing.T) {
	value := mustRGA(t, "writer")
	insert := mustInsertRGA(t, value, 0, "a")
	position := parentNodeID(insert)
	if !value.RetainsPosition(position) {
		t.Fatal("inserted position was not retained")
	}
	if _, err := value.Delete(0, 1); err != nil {
		t.Fatal(err)
	}
	if !value.RetainsPosition(position) {
		t.Fatal("deleted structural position was not retained")
	}
	if removed, err := value.CompactTombstones([]Position{position}); err != nil || removed != 1 {
		t.Fatalf("CompactTombstones() = %d, %v; want 1, nil", removed, err)
	}
	if value.RetainsPosition(position) {
		t.Fatal("compacted position remained retained")
	}
	if value.RetainsPosition(Position{}) {
		t.Fatal("root position reported retained")
	}

	pendingID := Position{ReplicaID: "remote", WallTime: 2}
	missingParent := Position{ReplicaID: "remote", WallTime: 1}
	if err := value.ApplyDelta(Delta{nodes: map[Position]node{
		pendingID: {parent: missingParent, rune: 'b'},
	}, tombstones: map[Position]struct{}{}}); err != nil {
		t.Fatal(err)
	}
	if !value.RetainsPosition(pendingID) {
		t.Fatal("pending position was not retained")
	}

	var nilValue *RGA
	if nilValue.RetainsPosition(pendingID) {
		t.Fatal("nil RGA reported a retained position")
	}
}
