package persistence

import (
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/clock"
	"github.com/darkinno-tech/crdt/counter"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/snapshot"
)

func TestCheckpointCodecRoundTripAndRejectsUnknownHeaders(t *testing.T) {
	checkpoint := Checkpoint{Snapshot: testSnapshot(t), Cursor: 7, Outbox: []byte("pending")}
	normalized, err := normalizeCheckpoint(checkpoint, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := marshalCheckpoint(normalized, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := unmarshalCheckpoint(encoded, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Cursor != checkpoint.Cursor || string(decoded.Outbox) != string(checkpoint.Outbox) || decoded.Snapshot.TypeID != checkpoint.Snapshot.TypeID {
		t.Fatalf("round trip = %+v", decoded)
	}
	for _, mutate := range []func([]byte){
		func(data []byte) { data[0] ^= 1 },
		func(data []byte) { data[len(recordMagic)]++ },
		func(data []byte) { data[len(recordMagic)+1] = 2 },
		func(data []byte) { data[len(data)-1] ^= 1 },
	} {
		bad := append([]byte(nil), encoded...)
		mutate(bad)
		if bad[len(bad)-1] != encoded[len(encoded)-1] {
			if _, err := unmarshalCheckpoint(bad, testConfig()); !errors.Is(err, ErrCorruptStore) {
				t.Fatalf("checksum corruption error = %v, want %v", err, ErrCorruptStore)
			}
			continue
		}
		resignCheckpoint(bad)
		if _, err := unmarshalCheckpoint(bad, testConfig()); !errors.Is(err, ErrCorruptStore) {
			t.Fatalf("unknown header error = %v, want %v", err, ErrCorruptStore)
		}
	}
}

func TestCheckpointCodecEnforcesStateKindAndRecordBudget(t *testing.T) {
	hlcSnapshot := testSnapshot(t)
	if _, err := decodedCheckpoint(hlcSnapshot.Bytes(), hlcSnapshot.Frontier(), nil, 0, nil, testConfig()); err == nil {
		t.Fatal("HLC state without clock accepted")
	}
	unknown, err := frame.MarshalFrame(frame.Frame{TypeID: 99})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodedCheckpoint(unknown, nil, nil, 0, nil, testConfig()); err == nil {
		t.Fatal("unknown state type accepted")
	}
	counterState, err := testCounterState()
	if err != nil {
		t.Fatal(err)
	}
	counterConfig := testConfig()
	counterConfig.Validate = validateTestCounter
	if _, err := decodedCheckpoint(counterState, nil, &clock.State{ReplicaID: "counter"}, 0, nil, counterConfig); err == nil {
		t.Fatal("non-HLC state with clock accepted")
	}
	counterSnapshot, err := snapshot.NewValidated(counterState, nil, validateTestCounter)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeCheckpoint(Checkpoint{Snapshot: counterSnapshot}, counterConfig); err != nil {
		t.Fatalf("non-HLC checkpoint rejected: %v", err)
	}
	tiny := testConfig()
	tiny.MaxRecordBytes = 1
	if _, err := checkpointSize(Checkpoint{Snapshot: hlcSnapshot}, tiny); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("tiny record budget error = %v, want %v", err, ErrInvalidCheckpoint)
	}
	if addRecordSize(nil, 1, 2) || addRecordSize(new(int), -1, 2) || addRecordSize(new(int), 1, -1) {
		t.Fatal("invalid record-size input accepted")
	}
}

func TestCheckpointCodecRejectsRecordLimitAndInvalidFrontier(t *testing.T) {
	checkpoint := Checkpoint{Snapshot: testSnapshot(t), Outbox: []byte("pending")}
	normalized, err := normalizeCheckpoint(checkpoint, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := marshalCheckpoint(normalized, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	tiny := testConfig()
	tiny.MaxRecordBytes = len(encoded) - 1
	if _, err := unmarshalCheckpoint(encoded, tiny); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("oversized stored record error = %v, want %v", err, ErrCorruptStore)
	}
	frontier := normalized.Snapshot.Frontier()
	frontier["wrong"] = crdt.Tag{ReplicaID: "other"}
	state := normalized.Snapshot.Bytes()
	clockState, ok := normalized.Snapshot.ClockState()
	if !ok {
		t.Fatal("test snapshot has no HLC state")
	}
	invalid, err := snapshot.NewWithClockState(state, frontier, clockState)
	if err == nil || invalid.TypeID != 0 {
		t.Fatalf("invalid frontier snapshot = %+v, err=%v", invalid, err)
	}
}

func TestCheckpointCodecRejectsEveryTruncatedFieldBeforeAllocation(t *testing.T) {
	hlcState := testSnapshot(t).Bytes()
	counterState, err := testCounterState()
	if err != nil {
		t.Fatal(err)
	}
	for _, encoded := range [][]byte{
		rawCheckpoint(0, []byte{0}),
		rawCheckpoint(0, appendBytes(nil, hlcState)),
		rawCheckpoint(0, append(appendBytes(nil, hlcState), frame.AppendUvarint(nil, uint64(testConfig().MaxFrontierEntries+1))...)),
		rawCheckpoint(0, append(append(appendBytes(nil, hlcState), frame.AppendUvarint(nil, 1)...), frame.AppendTag(nil, crdt.Tag{})...)),
		rawCheckpoint(recordClockPresent, append(appendBytes(nil, hlcState), frame.AppendUvarint(nil, 0)...)),
		rawCheckpoint(0, append(appendBytes(nil, counterState), frame.AppendUvarint(nil, 0)...)),
		rawCheckpoint(0, append(append(appendBytes(nil, counterState), frame.AppendUvarint(nil, 0)...), frame.AppendUvarint(nil, 0)...)),
	} {
		if _, err := unmarshalCheckpoint(encoded, testConfig()); !errors.Is(err, ErrCorruptStore) {
			t.Fatalf("truncated record error = %v, want %v", err, ErrCorruptStore)
		}
	}
	tooSmallID := testConfig()
	tooSmallID.MaxReplicaIDBytes = 2
	if _, err := normalizeCheckpoint(Checkpoint{Snapshot: testSnapshot(t)}, tooSmallID); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("replica-ID limit error = %v, want %v", err, ErrInvalidCheckpoint)
	}
	rejecting := testConfig()
	rejecting.Validate = func([]byte) error { return errors.New("bad schema") }
	if _, err := decodedCheckpoint(counterState, nil, nil, 0, nil, rejecting); err == nil {
		t.Fatal("rejecting validator accepted decoded checkpoint")
	}
	counterSnapshot, err := snapshot.NewValidated(counterState, nil, validateTestCounter)
	if err != nil {
		t.Fatal(err)
	}
	tiny := testConfig()
	tiny.MaxRecordBytes = 1
	if _, err := marshalCheckpoint(Checkpoint{Snapshot: counterSnapshot}, tiny); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("marshal with too-small record config error = %v, want %v", err, ErrInvalidCheckpoint)
	}
}

func resignCheckpoint(data []byte) {
	digest := sha256.Sum256(data[:len(data)-sha256.Size])
	copy(data[len(data)-sha256.Size:], digest[:])
}

func rawCheckpoint(flags byte, body []byte) []byte {
	data := make([]byte, 0, len(recordMagic)+2+len(body)+sha256.Size)
	data = append(data, recordMagic[:]...)
	data = append(data, recordVersion, flags)
	data = append(data, body...)
	digest := sha256.Sum256(data)
	return append(data, digest[:]...)
}

func testCounterState() ([]byte, error) {
	value, err := counter.NewGCounter("counter")
	if err != nil {
		return nil, err
	}
	if _, err := value.Increment(1); err != nil {
		return nil, err
	}
	return value.MarshalBinary()
}

func validateTestCounter(data []byte) error {
	value, err := counter.NewGCounter("validation")
	if err != nil {
		return err
	}
	return value.UnmarshalBinary(data)
}
