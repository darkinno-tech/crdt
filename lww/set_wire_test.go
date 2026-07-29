package lww

import (
	"bytes"
	"errors"
	"sync"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/delta"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

type setStringCodec struct{ id string }

func (c setStringCodec) ID() string                          { return c.id }
func (setStringCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (setStringCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }

type failingSetCodec struct{ setStringCodec }

func (failingSetCodec) Marshal(string) ([]byte, error) { return nil, errors.New("encode failure") }

type collidingSetCodec struct{ setStringCodec }

func (collidingSetCodec) Marshal(string) ([]byte, error) { return []byte("same"), nil }

type nilSetCodec struct{}

func (*nilSetCodec) ID() string                       { return "example.com/nil/v1" }
func (*nilSetCodec) Marshal(string) ([]byte, error)   { return nil, nil }
func (*nilSetCodec) Unmarshal([]byte) (string, error) { return "", nil }

func TestSetDeltaWireGoldenRecoveryAndCoalescing(t *testing.T) {
	codec := setStringCodec{id: "example.com/lww-set-string/v1"}
	tag := crdt.Tag{ReplicaID: "remote", WallTime: 7, Logical: 2}
	payload := frame.AppendUvarint(nil, 1)
	payload = frame.AppendUvarint(payload, 1)
	payload = append(payload, 'a')
	payload = frame.AppendTag(payload, tag)
	payload = frame.AppendUvarint(payload, 1)
	golden, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDLWWSetDelta, CodecID: codec.ID(), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalSetDelta(golden, codec)
	if err != nil {
		t.Fatal(err)
	}
	if encoded, err := decoded.MarshalBinary(codec); err != nil || !bytes.Equal(encoded, golden) {
		t.Fatalf("delta re-encoding = %x, %v; want %x", encoded, err, golden)
	}
	target, err := NewSet[string]("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(decoded); err != nil || !target.Contains("a") {
		t.Fatalf("ApplyDelta() = %v, contains=%v", err, target.Contains("a"))
	}
	clockBeforeDuplicate := target.ClockState()
	if err := target.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
	if target.ClockState() != clockBeforeDuplicate {
		t.Fatal("duplicate delta advanced the persisted HLC state")
	}

	first, err := target.AddWithDelta("first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := target.RemoveWithDelta("first")
	if err != nil {
		t.Fatal(err)
	}
	merged, err := first.Merge(second)
	if err != nil {
		t.Fatal(err)
	}
	coalescer, err := delta.NewCoalescer(1, len(golden)+512, func(left, right []byte) ([]byte, error) {
		leftDelta, err := UnmarshalSetDelta(left, codec)
		if err != nil {
			return nil, err
		}
		rightDelta, err := UnmarshalSetDelta(right, codec)
		if err != nil {
			return nil, err
		}
		joined, err := leftDelta.Merge(rightDelta)
		if err != nil {
			return nil, err
		}
		return joined.MarshalBinary(codec)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []SetDelta[string]{first, second} {
		encoded, err := candidate.MarshalBinary(codec)
		if err != nil {
			t.Fatal(err)
		}
		if err := coalescer.Add(encoded); err != nil {
			t.Fatal(err)
		}
	}
	items := coalescer.Drain().Items()
	if len(items) != 1 {
		t.Fatalf("coalesced item count = %d", len(items))
	}
	coalesced, err := UnmarshalSetDelta(items[0], codec)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(coalesced); err != nil {
		t.Fatal(err)
	}
	if target.Contains("first") {
		t.Fatal("coalesced delta resurrected a delete")
	}
	if err := target.ApplyDelta(merged); err != nil {
		t.Fatal(err)
	}

	state, err := target.MarshalBinary(codec)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewSet[string]("restored")
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.UnmarshalBinary(state, codec); err != nil {
		t.Fatal(err)
	}
	if reencoded, err := restored.MarshalBinary(codec); err != nil || !bytes.Equal(reencoded, state) {
		t.Fatalf("state re-encoding = %x, %v; want %x", reencoded, err, state)
	}
	saved, err := target.SnapshotCurrentState(codec)
	if err != nil {
		t.Fatal(err)
	}
	fromSnapshot, err := NewSetFromSnapshot(saved, codec)
	if err != nil || fromSnapshot.Contains("first") || !fromSnapshot.Contains("a") {
		t.Fatalf("NewSetFromSnapshot() = %v, state=%#v", err, fromSnapshot.State())
	}
	if _, err := snapshot.NewRecoveryPlan(saved, [][]byte{golden}, len(golden)); err != nil {
		t.Fatalf("LWW-Set recovery plan = %v", err)
	}
}

func TestSetWireRejectsMalformedAndUnsafeInputAtomically(t *testing.T) {
	codec := setStringCodec{id: "example.com/lww-set-wire/v1"}
	value, err := NewSet[string]("local")
	if err != nil {
		t.Fatal(err)
	}
	if err := value.Add("safe"); err != nil {
		t.Fatal(err)
	}
	before, err := value.MarshalBinary(codec)
	if err != nil {
		t.Fatal(err)
	}
	wrongCodec, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDLWWSetState, CodecID: "wrong", Payload: []byte{0}})
	if err != nil {
		t.Fatal(err)
	}
	if err := value.UnmarshalBinary(wrongCodec, codec); !errors.Is(err, ErrCodecMismatch) {
		t.Fatalf("wrong codec = %v", err)
	}
	payload := frame.AppendUvarint(nil, 2)
	for _, element := range []string{"z", "a"} {
		payload = frame.AppendUvarint(payload, 1)
		payload = append(payload, element...)
		payload = frame.AppendTag(payload, crdt.Tag{ReplicaID: "remote", WallTime: 1})
		payload = frame.AppendUvarint(payload, 1)
	}
	unsorted, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDLWWSetState, CodecID: codec.ID(), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := value.UnmarshalBinary(unsorted, codec); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("unsorted state = %v", err)
	}
	if after, err := value.MarshalBinary(codec); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("receiver changed after rejected state: %v", err)
	}
	if _, err := marshalSet(crdt.TypeIDLWWSetState, collidingSetCodec{setStringCodec{id: "collision"}}, map[string]setEntry[string]{
		"a": {tag: crdt.Tag{ReplicaID: "a"}, present: true},
		"b": {tag: crdt.Tag{ReplicaID: "b"}, present: true},
	}, frame.DefaultLimits()); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("codec collision = %v", err)
	}
	if _, err := marshalSet(crdt.TypeIDLWWSetState, failingSetCodec{setStringCodec{id: "failing"}}, map[string]setEntry[string]{
		"a": {tag: crdt.Tag{ReplicaID: "a"}, present: true},
	}, frame.DefaultLimits()); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("codec failure = %v", err)
	}
	limits := frame.DefaultLimits()
	limits.MaxElements = 0
	if _, err := marshalSet(crdt.TypeIDLWWSetState, codec, map[string]setEntry[string]{
		"a": {tag: crdt.Tag{ReplicaID: "a"}, present: true},
	}, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("element limit = %v", err)
	}
	if _, err := NewSetFromSnapshot(snapshot.Snapshot{TypeID: crdt.TypeIDLWWSetState}, codec); !errors.Is(err, ErrInvalidSetSnap) {
		t.Fatalf("snapshot without clock = %v", err)
	}
}

func TestSetWireBoundaryAndDiagnosticPaths(t *testing.T) {
	codec := setStringCodec{id: "example.com/lww-set-boundary/v1"}
	value, err := NewSet[string]("local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.AddWithDelta("a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := value.MarshalBinaryWithClockState(codec); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Snapshot(codec, map[string]crdt.Tag{"wrong": {ReplicaID: "other"}}); err == nil {
		t.Fatal("invalid supplied frontier accepted")
	}
	if _, err := value.Snapshot(codec, value.Frontier()); err != nil {
		t.Fatal(err)
	}
	if _, err := value.MarshalJSON(); err != nil {
		t.Fatal(err)
	}
	if _, err := (SetDelta[string]{}).MarshalJSON(); err != nil {
		t.Fatal(err)
	}
	if _, err := (MapDelta{}).MarshalJSON(); err != nil {
		t.Fatal(err)
	}

	var nilCodec *nilSetCodec
	if _, err := value.MarshalBinary(nilCodec); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("typed nil codec = %v", err)
	}
	if _, err := marshalSet(999, codec, nil, frame.DefaultLimits()); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("invalid type = %v", err)
	}
	if _, err := marshalSet(crdt.TypeIDLWWSetState, codec, map[string]setEntry[string]{"a": {present: true}}, frame.DefaultLimits()); !errors.Is(err, ErrInvalidSetDelta) {
		t.Fatalf("invalid entry = %v", err)
	}
	limits := frame.DefaultLimits()
	limits.MaxStringBytes = 1
	if _, err := marshalSet(crdt.TypeIDLWWSetState, codec, map[string]setEntry[string]{
		"long": {tag: crdt.Tag{ReplicaID: "remote"}, present: true},
	}, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("string limit = %v", err)
	}
	state, err := value.MarshalBinary(codec)
	if err != nil {
		t.Fatal(err)
	}
	limits = frame.DefaultLimits()
	limits.MaxElements = 0
	if err := value.UnmarshalBinaryWithLimits(state, codec, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("decode element limit = %v", err)
	}
	if _, err := UnmarshalSetDelta(state, codec); !errors.Is(err, ErrCodecMismatch) {
		t.Fatalf("state accepted as delta = %v", err)
	}
	badState, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDLWWSetState, CodecID: codec.ID(), Payload: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	badSnapshot, err := snapshot.NewWithClockState(badState, nil, value.ClockState())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSetFromSnapshot(badSnapshot, codec); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("bad snapshot restore = %v", err)
	}
	if _, err := NewSetFromSnapshot(snapshot.Snapshot{TypeID: crdt.TypeIDGCounterState}, codec); !errors.Is(err, ErrInvalidSetSnap) {
		t.Fatalf("wrong snapshot type = %v", err)
	}

	var nilSet *Set[string]
	if _, err := nilSet.MarshalBinary(codec); !errors.Is(err, ErrNilSet) {
		t.Fatalf("nil MarshalBinary = %v", err)
	}
	if _, _, err := nilSet.MarshalBinaryWithClockState(codec); !errors.Is(err, ErrNilSet) {
		t.Fatalf("nil MarshalBinaryWithClockState = %v", err)
	}
	if _, err := nilSet.SnapshotCurrentState(codec); !errors.Is(err, ErrNilSet) {
		t.Fatalf("nil SnapshotCurrentState = %v", err)
	}
	if err := nilSet.UnmarshalBinary(nil, codec); !errors.Is(err, ErrNilSet) {
		t.Fatalf("nil UnmarshalBinary = %v", err)
	}
}

func TestSetConcurrentWritesFramesAndRestores(t *testing.T) {
	codec := setStringCodec{id: "example.com/lww-set-concurrent/v1"}
	value, err := NewSet[string]("concurrent")
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
				if _, err := value.AddWithDelta(key); err != nil {
					t.Errorf("AddWithDelta() = %v", err)
				}
				if index%3 == 0 {
					if _, err := value.RemoveWithDelta(key); err != nil {
						t.Errorf("RemoveWithDelta() = %v", err)
					}
				}
				if _, err := value.MarshalBinary(codec); err != nil {
					t.Errorf("MarshalBinary() = %v", err)
				}
			}
		}(worker)
	}
	group.Wait()
	state, err := value.MarshalBinary(codec)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewSet[string]("restored")
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.UnmarshalBinary(state, codec); err != nil {
		t.Fatal(err)
	}
}

func FuzzSetUnmarshalBinary(f *testing.F) {
	codec := setStringCodec{id: "example.com/lww-set-fuzz/v1"}
	value, err := NewSet[string]("seed")
	if err != nil {
		f.Fatal(err)
	}
	if err := value.Add("seed"); err != nil {
		f.Fatal(err)
	}
	state, err := value.MarshalBinary(codec)
	if err != nil {
		f.Fatal(err)
	}
	delta, err := value.AddWithDelta("delta")
	if err != nil {
		f.Fatal(err)
	}
	encodedDelta, err := delta.MarshalBinary(codec)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(state)
	f.Add(encodedDelta)
	f.Add([]byte("not a frame"))
	f.Fuzz(func(t *testing.T, data []byte) {
		target, err := NewSet[string]("target")
		if err != nil {
			t.Fatal(err)
		}
		if err := target.UnmarshalBinary(data, codec); err == nil && target.State().ElementCount < 0 {
			t.Fatal("successful decode produced impossible count")
		}
		if decoded, err := UnmarshalSetDelta(data, codec); err == nil {
			if err := target.ApplyDelta(decoded); err != nil {
				t.Fatalf("decoded delta rejected: %v", err)
			}
		}
	})
}
