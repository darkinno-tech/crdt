package durable

import (
	"errors"
	"testing"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/replica"
)

func TestWireRoundTripsAndRejectsInvalidControl(t *testing.T) {
	manifest := durableTestManifest(t)
	hello, err := marshalHello(manifest, 4)
	if err != nil {
		t.Fatal(err)
	}
	remote, resume, err := unmarshalHello(hello)
	if err != nil || resume != 4 || manifest.Compatible(remote) != nil {
		t.Fatalf("hello = %#v resume=%d err=%v", remote, resume, err)
	}
	welcome, err := marshalWelcome(manifest, 8)
	if err != nil {
		t.Fatal(err)
	}
	remote, highWater, err := unmarshalWelcome(welcome)
	if err != nil || highWater != 8 || manifest.Compatible(remote) != nil {
		t.Fatalf("welcome = %#v high=%d err=%v", remote, highWater, err)
	}
	if err := unmarshalError([]byte(`{"version":1,"code":"replay_unavailable"}`)); !errors.Is(err, ErrReplayUnavailable) {
		t.Fatalf("replay error = %v", err)
	}
	vector, err := replica.NewFrontier(map[string]uint64{"alice": 4, "bob": 2})
	if err != nil {
		t.Fatal(err)
	}
	stateHello, err := marshalStateVectorHello(manifest, vector, 4, 128)
	if err != nil {
		t.Fatal(err)
	}
	remote, restoredVector, err := unmarshalStateVectorHello(stateHello, 4, 128)
	if err != nil || manifest.Compatible(remote) != nil || restoredVector.Counter("alice") != 4 || restoredVector.Counter("bob") != 2 {
		t.Fatalf("state hello remote=%#v vector=%#v err=%v", remote, restoredVector.Entries(), err)
	}
	stateWelcome, err := marshalStateVectorWelcome(manifest, 8)
	if err != nil {
		t.Fatal(err)
	}
	remote, highWater, err = unmarshalStateVectorWelcome(stateWelcome)
	if err != nil || highWater != 8 || manifest.Compatible(remote) != nil {
		t.Fatalf("state welcome remote=%#v high=%d err=%v", remote, highWater, err)
	}
	complete, err := marshalCatchUpComplete(8)
	if err != nil {
		t.Fatal(err)
	}
	if highWater, err := unmarshalCatchUpComplete(complete); err != nil || highWater != 8 {
		t.Fatalf("catch-up complete high=%d err=%v", highWater, err)
	}
	for _, invalid := range [][]byte{nil, []byte(`{"version":1}`), append(hello, 'x')} {
		if _, _, err := unmarshalHello(invalid); err == nil {
			t.Fatalf("unmarshal invalid hello %q succeeded", invalid)
		}
	}
}

func TestEventWireRoundTrip(t *testing.T) {
	manifest := durableTestManifest(t)
	change := durableTestChange(t, manifest, "alice", 1, 2)
	encoded, err := marshalEvent(Event{Sequence: 3, Change: change})
	if err != nil {
		t.Fatal(err)
	}
	sequence, dot, delta, err := unmarshalEvent(encoded, 1<<20, 128)
	if err != nil {
		t.Fatal(err)
	}
	event, err := newEventFromWire(manifest, crdt.ProtocolPolicy{}, sequence, dot, delta)
	if err != nil || event.Sequence != 3 || event.Change.Dot != change.Dot {
		t.Fatalf("event = %+v err=%v", event, err)
	}
	if _, _, _, err := unmarshalEvent([]byte{eventMessage, 0}, 1<<20, 128); err == nil {
		t.Fatal("zero sequence event succeeded")
	}
	if _, err := replica.NewChange(manifest, replica.Dot{}, change.Delta()); err == nil {
		t.Fatal("invalid replica dot succeeded")
	}
}

func TestWireBoundaries(t *testing.T) {
	if controlLimit(7) != 7 || controlLimit(maxControlBytes+1) != maxControlBytes {
		t.Fatal("control limit mismatch")
	}
	if _, err := marshalError("unexpected"); err == nil {
		t.Fatal("unknown error code encoded")
	}
	if _, err := marshalChange(replica.Change{}); err == nil {
		t.Fatal("empty change encoded")
	}
	if _, err := marshalEvent(Event{}); err == nil {
		t.Fatal("empty event encoded")
	}
	manifest := durableTestManifest(t)
	valid := durableTestChange(t, manifest, "alice", 1, 1)
	invalidActor, err := replica.NewChange(manifest, replica.Dot{Actor: string([]byte{0xff}), Counter: 1}, valid.Delta())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := marshalChange(invalidActor); err == nil {
		t.Fatal("invalid UTF-8 actor encoded")
	}
	if err := unmarshalError([]byte(`{"version":1,"code":"other"}`)); err == nil {
		t.Fatal("unknown error decoded")
	}
	if _, _, err := unmarshalWelcome(nil); err == nil {
		t.Fatal("empty welcome decoded")
	}
	if _, _, err := unmarshalWelcome(append([]byte(`{"version":1,"manifest":{}}`), 'x')); err == nil {
		t.Fatal("trailing welcome decoded")
	}
	if err := unmarshalError(nil); err == nil {
		t.Fatal("empty error decoded")
	}
	if _, _, err := unmarshalStateVectorHello([]byte(`{"version":2,"manifest":{},"state_vector":[{"actor":"z","counter":1},{"actor":"a","counter":1}]}`), 2, 128); err == nil {
		t.Fatal("non-canonical state vector succeeded")
	}
	if _, err := marshalStateVectorHello(durableTestManifest(t), replica.Frontier{}, 0, 128); err == nil {
		t.Fatal("invalid state-vector limits succeeded")
	}
	if _, _, _, err := unmarshalEvent([]byte{eventMessage, 1}, 1<<20, 128); err == nil {
		t.Fatal("truncated event decoded")
	}
	for _, invalid := range [][]byte{
		{changeMessage, 0, 1, 1, 'x'},
		{changeMessage, 1, 'a', 0, 1, 'x'},
		{changeMessage, 1, 'a', 1, 0},
		{changeMessage, 1, 0xff, 1, 1, 'x'},
	} {
		if _, _, err := unmarshalChange(invalid, 1<<20, 128); err == nil {
			t.Fatalf("invalid change %v decoded", invalid)
		}
	}
	if _, err := newEventFromWire(replica.Manifest{}, crdt.ProtocolPolicy{}, 1, replica.Dot{Actor: "a", Counter: 1}, []byte{1}); err == nil {
		t.Fatal("invalid event constructed")
	}
}

func FuzzWire(f *testing.F) {
	manifest, err := replica.NewManifest("fuzz-counter", "example.com/fuzz-counter/v1", 1, replica.Protocol{
		StateID:          crdt.TypeIDGCounterState,
		DeltaID:          crdt.TypeIDGCounterDelta,
		SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		f.Fatal(err)
	}
	hello, err := marshalHello(manifest, 0)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(hello)
	vectorHello, err := marshalStateVectorHello(manifest, replica.Frontier{}, 4, 128)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(vectorHello)
	merkleHello, err := marshalMerkleHello(manifest, NewMerkleIndex().Root())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(merkleHello)
	f.Add([]byte{changeMessage})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = unmarshalHello(data)
		_, _, _ = unmarshalStateVectorHello(data, 4, 128)
		_, _, _ = unmarshalWelcome(data)
		_, _, _ = unmarshalStateVectorWelcome(data)
		_, _ = unmarshalCatchUpComplete(data)
		_, _, _ = unmarshalMerkleHello(data)
		_, _, _ = unmarshalMerkleWelcome(data, 128)
		_, _, _ = unmarshalMerkleInventory(data, 128)
		_, _, _ = unmarshalMerkleRequest(data, 128)
		_, _ = unmarshalMerkleComplete(data, 128)
		_ = unmarshalError(data)
		_, _, _ = unmarshalChange(data, 1<<20, 128)
		_, _, _, _ = unmarshalEvent(data, 1<<20, 128)
		_, _, _, _, _ = unmarshalMerkleEvent(data, 1<<20, 128)
	})
}
