package crdt

import "testing"

func TestTagValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tag  Tag
		want bool
	}{
		{name: "non-empty replica", tag: Tag{ReplicaID: "replica-a"}, want: true},
		{name: "replica with surrounding whitespace", tag: Tag{ReplicaID: " replica-a "}, want: true},
		{name: "empty replica", tag: Tag{}, want: false},
		{name: "whitespace-only replica", tag: Tag{ReplicaID: " \t\n "}, want: false},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.tag.Valid(); got != test.want {
				t.Fatalf("Tag.Valid() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestTagCompare(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		left  Tag
		right Tag
		want  int
	}{
		{
			name:  "wall time sorts first",
			left:  Tag{ReplicaID: "a", WallTime: 1, Logical: 99},
			right: Tag{ReplicaID: "z", WallTime: 2},
			want:  -1,
		},
		{
			name:  "logical time breaks wall time tie",
			left:  Tag{ReplicaID: "a", WallTime: 2, Logical: 3},
			right: Tag{ReplicaID: "a", WallTime: 2, Logical: 2},
			want:  1,
		},
		{
			name:  "replica ID breaks clock tie",
			left:  Tag{ReplicaID: "a", WallTime: 2, Logical: 3},
			right: Tag{ReplicaID: "b", WallTime: 2, Logical: 3},
			want:  -1,
		},
		{
			name:  "equal tags compare equal",
			left:  Tag{ReplicaID: "a", WallTime: 2, Logical: 3},
			right: Tag{ReplicaID: "a", WallTime: 2, Logical: 3},
			want:  0,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.left.Compare(test.right); got != test.want {
				t.Fatalf("Tag.Compare() = %d, want %d", got, test.want)
			}
			if test.want != 0 && test.right.Compare(test.left) != -test.want {
				t.Fatal("Tag.Compare() is not antisymmetric")
			}
		})
	}
}

func TestFrameTypeRegistryAdmitsOnlyImplementedProtocols(t *testing.T) {
	for _, test := range []struct {
		stateID uint64
		deltaID uint64
		usesHLC bool
	}{
		{TypeIDGCounterState, TypeIDGCounterDelta, false},
		{TypeIDORSetState, TypeIDORSetDelta, true},
		{TypeIDPNCounterState, TypeIDPNCounterDelta, false},
		{TypeIDRGAState, TypeIDRGADelta, true},
		{TypeIDORTreeState, TypeIDORTreeDelta, true},
	} {
		kind, ok := FrameTypeForState(test.stateID)
		if !ok || kind.DeltaID != test.deltaID || kind.UsesHLC != test.usesHLC {
			t.Fatalf("FrameTypeForState(%d) = %#v, %v", test.stateID, kind, ok)
		}
		fromDelta, ok := FrameTypeForDelta(test.deltaID)
		if !ok || fromDelta != kind {
			t.Fatalf("FrameTypeForDelta(%d) = %#v, %v", test.deltaID, fromDelta, ok)
		}
	}
	if _, ok := FrameTypeForState(TypeIDLWWMapState); ok {
		t.Fatal("reserved LWW state type is not wire-ready")
	}
	if _, ok := FrameTypeForDelta(TypeIDLWWMapDelta); ok {
		t.Fatal("reserved LWW delta type is not wire-ready")
	}
}
