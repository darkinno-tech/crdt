package list

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

func TestMoveRGAConvergesAndPreservesElementIdentity(t *testing.T) {
	source := mustMoveList(t, "source")
	base, err := source.Append([]string{"a", "b", "c", "d"})
	if err != nil {
		t.Fatal(err)
	}
	original := source.Positions()
	left, right, observer := mustMoveList(t, "left"), mustMoveList(t, "right"), mustMoveList(t, "observer")
	for _, target := range []*MoveRGA[string]{left, right, observer} {
		if err := target.ApplyDelta(base); err != nil {
			t.Fatal(err)
		}
	}
	moveLeft, err := left.Move(0, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	moveRight, err := right.Move(3, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for seed, target := range map[int64]*MoveRGA[string]{1: left, 2: right, 3: observer} {
		deltas := []MoveDelta{moveLeft, moveRight, moveLeft}
		random := rand.New(rand.NewSource(seed))
		random.Shuffle(len(deltas), func(left, right int) { deltas[left], deltas[right] = deltas[right], deltas[left] })
		for _, delta := range deltas {
			encoded, err := delta.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := UnmarshalMoveDelta(encoded, stringCodec{})
			if err != nil {
				t.Fatal(err)
			}
			if err := target.ApplyDelta(decoded); err != nil {
				t.Fatal(err)
			}
		}
	}
	want, err := left.Values()
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []*MoveRGA[string]{right, observer} {
		got, err := target.Values()
		if err != nil || !sameStrings(got, want) {
			t.Fatalf("convergence got=%q want=%q err=%v", got, want, err)
		}
	}
	positions := left.Positions()
	if len(positions) != len(original) {
		t.Fatalf("positions len = %d, want %d", len(positions), len(original))
	}
	seen := make(map[Position]struct{}, len(positions))
	for _, position := range positions {
		seen[position] = struct{}{}
	}
	for _, position := range original {
		if _, exists := seen[position]; !exists {
			t.Fatalf("move replaced permanent identity %#v", position)
		}
	}
	saved, err := observer.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewMoveRGAFromSnapshot(saved, stringCodec{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := recovered.Values()
	if err != nil || !sameStrings(got, want) {
		t.Fatalf("recovered values=%q want=%q err=%v", got, want, err)
	}
}

func TestMoveRGAConcurrentCycleHasDeterministicProjection(t *testing.T) {
	source := mustMoveList(t, "source")
	base, err := source.Append([]string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	left, right, first, second := mustMoveList(t, "left"), mustMoveList(t, "right"), mustMoveList(t, "first"), mustMoveList(t, "second")
	for _, target := range []*MoveRGA[string]{left, right, first, second} {
		if err := target.ApplyDelta(base); err != nil {
			t.Fatal(err)
		}
	}
	// These concurrent placements form a -> b and b -> a. The wire state
	// keeps both user intents, while the pure projection drops one attachment
	// deterministically instead of allowing traversal order to choose a repair.
	moveA, err := left.Move(0, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	moveB, err := right.Move(1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []MoveDelta{moveA, moveB} {
		if err := first.ApplyDelta(delta); err != nil {
			t.Fatal(err)
		}
	}
	for _, delta := range []MoveDelta{moveB, moveA} {
		if err := second.ApplyDelta(delta); err != nil {
			t.Fatal(err)
		}
	}
	leftValues, err := first.Values()
	if err != nil {
		t.Fatal(err)
	}
	rightValues, err := second.Values()
	if err != nil || !sameStrings(leftValues, rightValues) || len(leftValues) != 2 {
		t.Fatalf("cycle projection left=%q right=%q err=%v", leftValues, rightValues, err)
	}
}

func TestMoveRGARejectsMalformedStateWithoutMutation(t *testing.T) {
	value := mustMoveList(t, "writer")
	if _, err := value.Append([]string{"safe"}); err != nil {
		t.Fatal(err)
	}
	before, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := value.UnmarshalBinary([]byte("not a frame")); err == nil {
		t.Fatal("malformed state accepted")
	}
	after, err := value.MarshalBinary()
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected frame mutated state: %v", err)
	}
	tight := frame.DefaultLimits()
	tight.MaxPayload = 1
	if _, err := value.MarshalBinaryWithLimits(tight); err == nil {
		t.Fatal("tight state limit accepted")
	}
	if !(crdt.ProtocolPolicy{}).SupportsFrame(crdt.TypeIDMoveRGADelta) {
		t.Fatal("default policy omitted the implemented move protocol")
	}
}

func TestMoveRGABoundariesMergeAndDeltaRejection(t *testing.T) {
	if _, err := NewMoveRGA("", stringCodec{}); err == nil {
		t.Fatal("empty replica accepted")
	}
	if _, err := NewMoveRGA[string]("writer", nil); err == nil {
		t.Fatal("nil codec accepted")
	}
	if _, err := NewMoveRGAWithOptions("writer", stringCodec{}, Options{}); err == nil {
		t.Fatal("invalid options accepted")
	}
	value := mustMoveList(t, "writer")
	if _, err := value.Insert(-1, []string{"bad"}); err == nil {
		t.Fatal("negative insert accepted")
	}
	if _, err := value.Delete(0, 1); err == nil {
		t.Fatal("out-of-range delete accepted")
	}
	if _, err := value.Move(0, 1, 0); err == nil {
		t.Fatal("out-of-range move accepted")
	}
	insert, err := value.Append([]string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Delete(1, 1); err != nil {
		t.Fatal(err)
	}
	if got, err := value.Values(); err != nil || !sameStrings(got, []string{"a", "c"}) {
		t.Fatalf("delete values=%q err=%v", got, err)
	}
	other := mustMoveList(t, "other")
	if err := other.ApplyDelta(insert); err != nil {
		t.Fatal(err)
	}
	if err := other.Merge(value); err != nil {
		t.Fatal(err)
	}
	if got, err := other.Values(); err != nil || !sameStrings(got, []string{"a", "c"}) {
		t.Fatalf("merge values=%q err=%v", got, err)
	}
	wrongCodec, err := NewMoveRGA("wrong", nonCanonicalCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Merge(wrongCodec); err == nil {
		t.Fatal("codec-mismatched merge accepted")
	}
	before, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	position := value.Positions()[0]
	conflict := MoveDelta{codecID: value.codecID, nodes: map[Position]node{position: {value: []byte("other")}}, tombstones: map[Position]struct{}{}, moves: map[Position]moveRecord{}}
	if err := value.ApplyDelta(conflict); err == nil {
		t.Fatal("conflicting node accepted")
	}
	invalidMove := MoveDelta{codecID: value.codecID, nodes: map[Position]node{}, tombstones: map[Position]struct{}{}, moves: map[Position]moveRecord{position: {tag: Position{}, anchor: Position{}}}}
	if err := value.ApplyDelta(invalidMove); err == nil {
		t.Fatal("invalid move accepted")
	}
	after, err := value.MarshalBinary()
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected delta mutated state: %v", err)
	}
	tiny, err := NewMoveRGAWithOptions("tiny", stringCodec{}, Options{MaxNodes: 1, MaxTombstones: 1, MaxPendingNodes: 1, MaxPendingBytes: 1, MaxValueBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if err := tiny.ApplyDelta(insert); err == nil {
		t.Fatal("node retention limit accepted")
	}
}

func TestMoveRGAWireLimitsAndSnapshotValidation(t *testing.T) {
	value := mustMoveList(t, "writer")
	if _, err := value.Append([]string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Move(1, 1, 0); err != nil {
		t.Fatal(err)
	}
	encoded, state, err := value.MarshalBinaryWithClockState()
	if err != nil || state.ReplicaID != "writer" {
		t.Fatalf("clock state=%#v err=%v", state, err)
	}
	tight := frame.DefaultLimits()
	tight.MaxElements = 1
	if _, err := value.MarshalBinaryWithLimits(tight); err == nil {
		t.Fatal("element limit accepted")
	}
	if _, err := UnmarshalMoveDelta(encoded, stringCodec{}); err == nil {
		t.Fatal("state frame decoded as delta")
	}
	if err := value.UnmarshalBinary(encoded); err != nil {
		t.Fatal(err)
	}
	saved, err := value.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	wrong := saved
	wrong.TypeID = crdt.TypeIDListRGAState
	if _, err := NewMoveRGAFromSnapshot(wrong, stringCodec{}); err == nil {
		t.Fatal("wrong snapshot type accepted")
	}
	withoutClock, err := snapshot.New(encoded, saved.Frontier())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewMoveRGAFromSnapshot(withoutClock, stringCodec{}); err == nil {
		t.Fatal("snapshot without clock accepted")
	}
	invalid := MoveDelta{codecID: value.codecID, nodes: map[Position]node{{ReplicaID: "missing", WallTime: 1}: {parent: Position{ReplicaID: "gone", WallTime: 1}, value: []byte("x")}}, tombstones: map[Position]struct{}{}, moves: map[Position]moveRecord{}}
	if _, err := marshalMoveRGA(crdt.TypeIDMoveRGAState, invalid, frame.DefaultLimits()); err == nil {
		t.Fatal("incomplete state marshaled")
	}
}

func TestMoveRGAInternalSafetyBoundaries(t *testing.T) {
	var nilList *MoveRGA[string]
	if _, err := nilList.Insert(0, nil); err == nil {
		t.Fatal("nil insert accepted")
	}
	if _, err := nilList.Append(nil); err == nil {
		t.Fatal("nil append accepted")
	}
	if _, err := nilList.Delete(0, 0); err == nil {
		t.Fatal("nil delete accepted")
	}
	if _, err := nilList.Move(0, 0, 0); err == nil {
		t.Fatal("nil move accepted")
	}
	if _, err := nilList.Values(); err == nil || nilList.Positions() != nil || nilList.State().Type != "move-rga" {
		t.Fatal("nil list boundary mismatch")
	}
	if _, err := nilList.MarshalBinary(); err == nil {
		t.Fatal("nil list marshaled")
	}
	if err := nilList.UnmarshalBinary(nil); err == nil {
		t.Fatal("nil list unmarshaled")
	}
	if err := nilList.ApplyDelta(MoveDelta{}); err == nil {
		t.Fatal("nil list applied delta")
	}
	if nilList.ClockState().ReplicaID != "" {
		t.Fatal("nil clock state")
	}

	value := mustMoveList(t, "writer")
	if _, err := value.Insert(1, []string{"late"}); err == nil {
		t.Fatal("insert beyond visible tail accepted")
	}
	if empty, err := value.Insert(0, nil); err != nil || len(empty.nodes) != 0 {
		t.Fatalf("empty insert=%#v err=%v", empty, err)
	}
	if _, err := value.Delete(-1, 0); err == nil {
		t.Fatal("negative delete accepted")
	}
	if _, err := value.Move(-1, 0, 0); err == nil {
		t.Fatal("negative move accepted")
	}
	if err := validateMoveDelta(MoveDelta{}); err == nil {
		t.Fatal("empty delta accepted")
	}
	valid := MoveDelta{codecID: value.codecID, nodes: map[Position]node{}, tombstones: map[Position]struct{}{}, moves: map[Position]moveRecord{}}
	if err := validateMoveDelta(valid); err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalMoveDelta[string]([]byte("bad"), nil); err == nil {
		t.Fatal("nil delta codec accepted")
	}
	invalidNode := valid
	invalidNode.nodes = map[Position]node{{}: {value: []byte("x")}}
	if err := validateMoveDelta(invalidNode); err == nil {
		t.Fatal("invalid node accepted")
	}
	invalidTombstone := valid
	invalidTombstone.tombstones = map[Position]struct{}{{}: {}}
	if err := validateMoveDelta(invalidTombstone); err == nil {
		t.Fatal("invalid tombstone accepted")
	}
	invalidAnchor := valid
	invalidAnchor.moves = map[Position]moveRecord{{ReplicaID: "id", WallTime: 1}: {tag: Position{ReplicaID: "move", WallTime: 1}, anchor: Position{ReplicaID: "id", WallTime: 1}}}
	if err := validateMoveDelta(invalidAnchor); err == nil {
		t.Fatal("self anchor accepted")
	}
	if err := validateMoveCodecValues(MoveDelta{codecID: value.codecID, nodes: map[Position]node{{ReplicaID: "id", WallTime: 1}: {value: []byte{0xff}}}, tombstones: map[Position]struct{}{}, moves: map[Position]moveRecord{}}, stringCodec{}); err == nil {
		t.Fatal("invalid codec bytes accepted")
	}
	missing := MoveDelta{codecID: value.codecID, nodes: map[Position]node{{ReplicaID: "id", WallTime: 1}: {parent: Position{ReplicaID: "gone", WallTime: 1}, value: []byte("x")}}, tombstones: map[Position]struct{}{}, moves: map[Position]moveRecord{}}
	if completeMoveState(missing) {
		t.Fatal("missing parent marked complete")
	}
	missing.moves = map[Position]moveRecord{{ReplicaID: "missing", WallTime: 1}: {tag: Position{ReplicaID: "op", WallTime: 1}}}
	if completeMoveState(missing) {
		t.Fatal("missing move target marked complete")
	}
	if _, err := unmarshalMoveRGA([]byte("invalid"), crdt.TypeIDMoveRGAState, value.codecID, frame.DefaultLimits(), true); err == nil {
		t.Fatal("invalid move frame accepted")
	}
	if _, err := marshalMoveRGA(999, valid, frame.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Delete(0, 0); err != nil {
		t.Fatal(err)
	}
	if empty, err := value.Move(0, 0, 0); err != nil || len(empty.moves) != 0 {
		t.Fatalf("empty move=%#v err=%v", empty, err)
	}
	if _, err := value.Move(0, 0, 1); err == nil {
		t.Fatal("move destination outside remaining list accepted")
	}
	if err := value.UnmarshalBinaryWithLimits([]byte("bad"), frame.DefaultLimits()); err == nil {
		t.Fatal("invalid bounded state accepted")
	}
	limited := frame.DefaultLimits()
	limited.MaxPayload = 1
	if _, err := UnmarshalMoveDeltaWithLimits([]byte("bad"), stringCodec{}, limited); err == nil {
		t.Fatal("invalid bounded delta accepted")
	}
	pending, err := NewMoveRGAWithOptions("pending", stringCodec{}, Options{MaxNodes: 2, MaxTombstones: 2, MaxPendingNodes: 1, MaxPendingBytes: 1, MaxValueBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	foreign := MoveDelta{codecID: pending.codecID, nodes: map[Position]node{}, tombstones: map[Position]struct{}{}, moves: map[Position]moveRecord{{ReplicaID: "future", WallTime: 1}: {tag: Position{ReplicaID: "op", WallTime: 1}}}}
	if err := pending.ApplyDelta(foreign); err != nil {
		t.Fatal(err)
	}
	foreign.moves[Position{ReplicaID: "other", WallTime: 1}] = moveRecord{tag: Position{ReplicaID: "op", WallTime: 2}}
	if err := pending.ApplyDelta(foreign); err == nil {
		t.Fatal("pending move limit accepted")
	}
	small, err := NewMoveRGAWithOptions("small", stringCodec{}, Options{MaxNodes: 2, MaxTombstones: 2, MaxPendingNodes: 1, MaxPendingBytes: 1, MaxValueBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := small.Insert(0, []string{"too large"}); err == nil {
		t.Fatal("oversized canonical value accepted")
	}
	failing, err := NewMoveRGA("failing", failingMoveCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failing.Insert(0, []string{"x"}); err == nil {
		t.Fatal("failing codec accepted an insert")
	}
	if err := value.validateMoveValues(MoveDelta{codecID: value.codecID, nodes: map[Position]node{{ReplicaID: "id", WallTime: 1}: {value: make([]byte, value.options.MaxValueBytes+1)}}, tombstones: map[Position]struct{}{}, moves: map[Position]moveRecord{}}); err == nil {
		t.Fatal("oversized remote value accepted")
	}
	if _, _, err := nilList.MarshalBinaryWithClockState(); err == nil {
		t.Fatal("nil clock snapshot accepted")
	}
	if _, err := nilList.SnapshotCurrentState(); err == nil {
		t.Fatal("nil snapshot accepted")
	}
	source := mustMoveList(t, "source")
	initial, err := source.Append([]string{"one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	initialWire, err := initial.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decodedInitial, err := UnmarshalMoveDelta(initialWire, stringCodec{})
	if err != nil {
		t.Fatal(err)
	}
	receiver := mustMoveList(t, "receiver")
	if err := receiver.ApplyDelta(decodedInitial); err != nil {
		t.Fatal(err)
	}
	deleted, err := source.Delete(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	deletedWire, err := deleted.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decodedDelete, err := UnmarshalMoveDelta(deletedWire, stringCodec{})
	if err != nil || receiver.ApplyDelta(decodedDelete) != nil {
		t.Fatalf("delete delta decode/apply err=%v", err)
	}
	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	tinyState, err := NewMoveRGAWithOptions("tiny-state", stringCodec{}, Options{MaxNodes: 1, MaxTombstones: 2, MaxPendingNodes: 1, MaxPendingBytes: 1, MaxValueBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if err := tinyState.UnmarshalBinary(state); err == nil {
		t.Fatal("oversized snapshot accepted")
	}
	brokenValues := mustMoveList(t, "broken")
	if _, err := brokenValues.Append([]string{"x"}); err != nil {
		t.Fatal(err)
	}
	brokenValues.codec = failingMoveCodec{}
	if _, err := brokenValues.Values(); err == nil {
		t.Fatal("failing value decoder accepted projection")
	}
}

type failingMoveCodec struct{}

func (failingMoveCodec) ID() string                       { return "failing-move/v1" }
func (failingMoveCodec) Marshal(string) ([]byte, error)   { return nil, errors.New("marshal") }
func (failingMoveCodec) Unmarshal([]byte) (string, error) { return "", errors.New("unmarshal") }

func FuzzMoveRGAUnmarshal(f *testing.F) {
	value, err := NewMoveRGA("seed", stringCodec{})
	if err != nil {
		f.Fatal(err)
	}
	if _, err := value.Append([]string{"seed"}); err != nil {
		f.Fatal(err)
	}
	state, err := value.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(state)
	f.Fuzz(func(t *testing.T, data []byte) {
		target := mustMoveList(t, "target")
		if err := target.UnmarshalBinary(data); err == nil && target.State().ElementCount < 0 {
			t.Fatal("negative projection")
		}
		if delta, err := UnmarshalMoveDelta(data, stringCodec{}); err == nil {
			if err := target.ApplyDelta(delta); err != nil {
				t.Fatalf("decoded delta rejected: %v", err)
			}
		}
	})
}

func mustMoveList(t testing.TB, replicaID string) *MoveRGA[string] {
	t.Helper()
	value, err := NewMoveRGA(replicaID, stringCodec{})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func BenchmarkMoveRGARelocateThousand(b *testing.B) {
	value, err := NewMoveRGA("writer", stringCodec{})
	if err != nil {
		b.Fatal(err)
	}
	seed := make([]string, 1000)
	for index := range seed {
		seed[index] = "item"
	}
	if _, err := value.Append(seed); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		from := (index * 37) % 990
		if _, err := value.Move(from, 10, (index*53)%991); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMoveRGAValuesThousand(b *testing.B) {
	value, err := NewMoveRGA("writer", stringCodec{})
	if err != nil {
		b.Fatal(err)
	}
	seed := make([]string, 1000)
	for index := range seed {
		seed[index] = "item"
	}
	if _, err := value.Append(seed); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		values, err := value.Values()
		if err != nil || len(values) != len(seed) {
			b.Fatalf("values len=%d err=%v", len(values), err)
		}
	}
}
