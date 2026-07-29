package text

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
)

func TestRGARunStateSliceSnapshotMatchesCanonicalMapEncoding(t *testing.T) {
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Insert(0, "ab"); err != nil {
		t.Fatal(err)
	}
	peer, err := New("peer")
	if err != nil {
		t.Fatal(err)
	}
	peerDelta, err := peer.Insert(0, "xy")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.ApplyDelta(peerDelta); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Delete(1, 1); err != nil {
		t.Fatal(err)
	}

	source.mu.RLock()
	nodes := cloneNodes(source.nodes)
	tombstones := cloneTombstones(source.tombstones)
	source.mu.RUnlock()
	if blocks := makeRunBlocks(nodes); len(blocks) < 2 {
		t.Fatalf("test topology collapsed to %d run blocks", len(blocks))
	}
	want, err := marshalRGARun(crdt.TypeIDRGARunState, nodes, tombstones, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	got, err := source.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("slice state encoding = %x, want canonical map encoding %x", got, want)
	}

	saved, err := source.SnapshotRunCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved.Bytes(), got) {
		t.Fatalf("snapshot bytes = %x, want state encoding %x", saved.Bytes(), got)
	}
	if got, want := saved.Frontier(), frontierForState(nodes, tombstones); !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot frontier = %#v, want %#v", got, want)
	}
}

func TestRGARunStateSliceSnapshotRetainsStateValidation(t *testing.T) {
	incomplete, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	id := Position{ReplicaID: "source", WallTime: 1}
	incomplete.mu.Lock()
	incomplete.nodes[id] = node{parent: Position{ReplicaID: "missing", WallTime: 1}, rune: 'x'}
	incomplete.mu.Unlock()
	if _, err := incomplete.MarshalRunBinary(); !errors.Is(err, ErrIncompleteState) {
		t.Fatalf("incomplete run state encoding = %v, want %v", err, ErrIncompleteState)
	}
	if _, err := incomplete.SnapshotRunCurrentState(); !errors.Is(err, ErrIncompleteState) {
		t.Fatalf("incomplete run snapshot = %v, want %v", err, ErrIncompleteState)
	}

	invalid, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	invalid.mu.Lock()
	invalid.nodes[id] = node{rune: -1}
	invalid.mu.Unlock()
	if _, err := invalid.MarshalRunBinary(); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("invalid run state encoding = %v, want %v", err, ErrInvalidDelta)
	}

	cyclic, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	first := Position{ReplicaID: "source", WallTime: 1}
	second := Position{ReplicaID: "source", WallTime: 2}
	cyclic.mu.Lock()
	cyclic.nodes[first] = node{parent: second, rune: 'a'}
	cyclic.nodes[second] = node{parent: first, rune: 'b'}
	cyclic.mu.Unlock()
	if _, err := cyclic.MarshalRunBinary(); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("cyclic run state encoding = %v, want %v", err, ErrInvalidDelta)
	}
	if _, err := cyclic.SnapshotRunCurrentState(); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("cyclic run snapshot = %v, want %v", err, ErrInvalidDelta)
	}
}

func TestRGARunFramesRoundTripAndCompactLinearInsert(t *testing.T) {
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := source.Insert(0, "collaborative text")
	if err != nil {
		t.Fatal(err)
	}
	v1, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	run, err := delta.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(run) >= len(v1) {
		t.Fatalf("run delta size = %d, v1 size = %d", len(run), len(v1))
	}
	decoded, err := UnmarshalRGARunDelta(run)
	if err != nil {
		t.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
	if got := target.String(); got != source.String() {
		t.Fatalf("run delta text = %q, want %q", got, source.String())
	}

	if _, err := source.Delete(3, 5); err != nil {
		t.Fatal(err)
	}
	state, err := source.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := New("recovered")
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.UnmarshalRunBinary(state); err != nil {
		t.Fatal(err)
	}
	if got := recovered.String(); got != source.String() {
		t.Fatalf("run state text = %q, want %q", got, source.String())
	}
	snapshot, err := source.SnapshotRunCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TypeID != crdt.TypeIDRGARunState {
		t.Fatalf("snapshot type = %d", snapshot.TypeID)
	}
	fromSnapshot, err := NewFromSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if got := fromSnapshot.String(); got != source.String() {
		t.Fatalf("run snapshot text = %q, want %q", got, source.String())
	}
}

func TestRGARunDeltaWithLimits(t *testing.T) {
	value, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := value.Insert(0, "bounded run")
	if err != nil {
		t.Fatal(err)
	}
	limits := frame.DefaultLimits()
	encoded, err := delta.MarshalRunBinaryWithLimits(limits)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalRGARunDeltaWithLimits(encoded, limits)
	if err != nil {
		t.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(decoded); err != nil || target.String() != "bounded run" {
		t.Fatalf("bounded run delta apply = %q, %v", target.String(), err)
	}
	limited := limits
	limited.MaxElements = 1
	if _, err := delta.MarshalRunBinaryWithLimits(limited); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("bounded run delta marshal limit = %v", err)
	}
	if _, err := UnmarshalRGARunDeltaWithLimits(encoded, limited); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("bounded run delta decode limit = %v", err)
	}
}

func TestRGARunFramesRejectWrongTypeAndNonCanonicalPayload(t *testing.T) {
	value, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Insert(0, "abc"); err != nil {
		t.Fatal(err)
	}
	state, err := value.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalRGARunDelta(state); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("run wrong type = %v", err)
	}
	decoded, err := frame.UnmarshalFrame(state, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	// A valid envelope with a non-canonical run block must not be accepted.
	decoded.Payload[0] = 2
	malformed, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRGARunState, Payload: decoded.Payload})
	if err != nil {
		t.Fatal(err)
	}
	target, err := New("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.UnmarshalRunBinary(malformed); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("non-canonical run frame = %v", err)
	}
}

func FuzzRGARunUnmarshal(f *testing.F) {
	value, err := New("seed")
	if err != nil {
		f.Fatal(err)
	}
	if _, err := value.Insert(0, "seed"); err != nil {
		f.Fatal(err)
	}
	state, err := value.MarshalRunBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(state)
	f.Fuzz(func(t *testing.T, data []byte) {
		target, err := New("target")
		if err != nil {
			t.Fatal(err)
		}
		if err := target.UnmarshalRunBinary(data); err == nil && target.State().ElementCount < 0 {
			t.Fatal("negative visible count")
		}
		if delta, err := UnmarshalRGARunDelta(data); err == nil {
			if err := target.ApplyDelta(delta); err != nil {
				t.Fatalf("decoded run delta rejected: %v", err)
			}
		}
	})
}
