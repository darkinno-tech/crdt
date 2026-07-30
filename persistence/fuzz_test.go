package persistence

import "testing"

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
