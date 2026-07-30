package persistence

import (
	"testing"

	"github.com/DarkInno/crdt/snapshot"
)

func FuzzUnmarshalCheckpoint(f *testing.F) {
	value, err := setForBenchmark()
	if err != nil {
		f.Fatal(err)
	}
	saved, err := value.SnapshotCurrentState()
	if err != nil {
		f.Fatal(err)
	}
	encoded, err := marshalCheckpoint(Checkpoint{Snapshot: saved, Cursor: 1, Outbox: []byte("pending")}, testConfig())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(encoded)
	f.Add([]byte("not-a-checkpoint"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = unmarshalCheckpoint(data, testConfig())
	})
}

func FuzzUnmarshalFileRecords(f *testing.F) {
	checkpoint := Checkpoint{Snapshot: testSnapshotForFuzz(f), Cursor: 1, Outbox: []byte("pending")}
	record, err := marshalCheckpoint(checkpoint, testConfig())
	if err != nil {
		f.Fatal(err)
	}
	encoded, err := marshalFileRecords(map[string][]byte{"checkpoint": record}, testFileConfig())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(encoded)
	f.Add([]byte("not-a-file-store"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = unmarshalFileRecords(data, testFileConfig())
	})
}

func testSnapshotForFuzz(t testing.TB) snapshot.Snapshot {
	t.Helper()
	value, err := setForBenchmark()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := value.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	return saved
}
