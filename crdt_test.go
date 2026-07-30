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
		{TypeIDLWWSetState, TypeIDLWWSetDelta, true},
		{TypeIDLWWMapState, TypeIDLWWMapDelta, true},
		{TypeIDRGAState, TypeIDRGADelta, true},
		{TypeIDGSetState, TypeIDGSetDelta, false},
		{TypeIDMVRegisterState, TypeIDMVRegisterDelta, false},
		{TypeIDORTreeState, TypeIDORTreeDelta, true},
		{TypeIDRGARunState, TypeIDRGARunDelta, true},
		{TypeIDListRGAState, TypeIDListRGADelta, true},
		{TypeIDRichTextState, TypeIDRichTextDelta, true},
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
}

func TestProtocolPolicyExcludesExperimentalProtocolsByDefault(t *testing.T) {
	stable := (ProtocolPolicy{}).FrameTypes()
	if len(stable) != 7 {
		t.Fatalf("stable protocol count = %d, want 7", len(stable))
	}
	for _, kind := range stable {
		if IsExperimentalFrame(kind.StateID) || IsExperimentalFrame(kind.DeltaID) {
			t.Fatalf("default policy advertised experimental protocol %#v", kind)
		}
	}
	if (ProtocolPolicy{}).SupportsFrame(TypeIDRGAState) {
		t.Fatal("default policy supports experimental scalar RGA state")
	}
	if !(ProtocolPolicy{}).SupportsFrame(TypeIDRGARunState) || !(ProtocolPolicy{}).SupportsFrame(TypeIDRGARunDelta) {
		t.Fatal("default policy omitted run RGA frames")
	}
	if !(ProtocolPolicy{}).SupportsFrame(TypeIDRichTextState) || !(ProtocolPolicy{}).SupportsFrame(TypeIDRichTextDelta) {
		t.Fatal("default policy omitted stable rich-text frames")
	}
	if (ProtocolPolicy{}).SupportsFrame(TypeIDORTreeDelta) {
		t.Fatal("default policy supports OR-Tree delta")
	}
}

func TestProtocolPolicyOptInAndUnknownFrameHandling(t *testing.T) {
	policy := ProtocolPolicy{AllowExperimental: true}
	types := policy.FrameTypes()
	if len(types) != 12 {
		t.Fatalf("experimental protocol count = %d, want 12", len(types))
	}
	if !policy.SupportsFrame(TypeIDLWWSetState) || !policy.SupportsFrame(TypeIDLWWMapState) || !policy.SupportsFrame(TypeIDRGAState) || !policy.SupportsFrame(TypeIDRGARunDelta) || !policy.SupportsFrame(TypeIDListRGADelta) || !policy.SupportsFrame(TypeIDORTreeDelta) || !policy.SupportsFrame(TypeIDRichTextDelta) {
		t.Fatal("experimental policy omitted an implemented experimental protocol")
	}
	if policy.SupportsFrame(999) {
		t.Fatal("policy supported an unknown frame")
	}
	if !IsExperimentalFrame(TypeIDLWWSetDelta) || !IsExperimentalFrame(TypeIDLWWMapDelta) || !IsExperimentalFrame(TypeIDRGAState) || IsExperimentalFrame(TypeIDRGARunState) || !IsExperimentalFrame(TypeIDListRGAState) || !IsExperimentalFrame(TypeIDORTreeDelta) || IsExperimentalFrame(TypeIDRichTextState) {
		t.Fatal("implemented experimental protocol was not marked experimental")
	}
	if IsExperimentalFrame(999) {
		t.Fatal("unknown protocol was marked experimental")
	}

	types[0] = FrameType{}
	if got := policy.FrameTypes()[0]; got.StateID == 0 {
		t.Fatal("FrameTypes returned a shared slice")
	}
}

func TestDefaultRGAFrameTypeUsesRunV2(t *testing.T) {
	kind := DefaultRGAFrameType()
	if kind.StateID != TypeIDRGARunState || kind.DeltaID != TypeIDRGARunDelta || !kind.UsesHLC {
		t.Fatalf("DefaultRGAFrameType() = %#v", kind)
	}
}
