package persistence

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/DarkInno/crdt/set"
)

func TestFileStorePersistsHLCCheckpointAcrossRestart(t *testing.T) {
	path := t.TempDir() + "/checkpoint.store"
	store := testFileStore(t, path, testFileConfig())
	source := testORSet(t, "maintenance")
	if _, err := source.Add("inspect-filter"); err != nil {
		t.Fatal(err)
	}
	saved, err := source.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	wantOutbox := []byte("canonical-pending-change")
	if err := store.Save("maintenance", Checkpoint{Snapshot: saved, Cursor: 41, Outbox: wantOutbox}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = testFileStore(t, path, testFileConfig())
	defer func() { _ = store.Close() }()
	checkpoint, found, err := store.Load("maintenance")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !found || checkpoint.Cursor != 41 || string(checkpoint.Outbox) != string(wantOutbox) {
		t.Fatalf("Load() = %+v, found=%t", checkpoint, found)
	}
	restored, err := set.NewORSetFromSnapshot(checkpoint.Snapshot, testStringCodec{})
	if err != nil {
		t.Fatalf("NewORSetFromSnapshot() error = %v", err)
	}
	if _, err := restored.Add("replace-filter"); err != nil || !restored.Contains("replace-filter") {
		t.Fatalf("restored mutation err=%v contains=%t", err, restored.Contains("replace-filter"))
	}
	checkpoint.Outbox[0] = 'X'
	again, found, err := store.Load("maintenance")
	if err != nil || !found || string(again.Outbox) != string(wantOutbox) {
		t.Fatalf("Load() after caller mutation = %+v, found=%t, err=%v", again, found, err)
	}
}

func TestFileStoreRejectsCorruptionAndUnsafePaths(t *testing.T) {
	root := t.TempDir()
	config := testFileConfig()
	if _, err := OpenFile(root+"/missing/checkpoint.store", config); err == nil {
		t.Fatal("OpenFile() accepted missing parent")
	}
	path := root + "/checkpoint.store"
	store := testFileStore(t, path, config)
	if err := store.Save("maintenance", Checkpoint{Snapshot: testSnapshot(t)}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.records["maintenance"] = []byte("not-a-checkpoint")
	store.mu.Unlock()
	if _, _, err := store.Load("maintenance"); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("Load() corrupted in-memory record error = %v, want %v", err, ErrCorruptStore)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not-a-store"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(path, config); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("OpenFile() corrupt store error = %v, want %v", err, ErrCorruptStore)
	}
	if err := os.WriteFile(path, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(path, config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("OpenFile() public permissions error = %v, want %v", err, ErrInvalidConfig)
	}
}

func TestFileStoreRejectsOverBudgetSaveWithoutReplacingCheckpoint(t *testing.T) {
	path := t.TempDir() + "/checkpoint.store"
	baseConfig := testFileConfig()
	checkpoint := Checkpoint{Snapshot: testSnapshot(t)}
	record, err := marshalCheckpoint(checkpoint, baseConfig.Config)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := marshalFileRecords(map[string][]byte{"first": record}, baseConfig)
	if err != nil {
		t.Fatal(err)
	}
	config := baseConfig
	config.MaxStoreBytes = len(encoded)
	store := testFileStore(t, path, config)
	defer func() { _ = store.Close() }()
	if err := store.Save("first", checkpoint); err != nil {
		t.Fatalf("Save(first) error = %v", err)
	}
	if err := store.Save("second", checkpoint); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Save(second) error = %v, want %v", err, ErrInvalidCheckpoint)
	}
	if _, found, err := store.Load("first"); err != nil || !found {
		t.Fatalf("Load(first) after failed replacement found=%t err=%v", found, err)
	}
	if _, found, err := store.Load("second"); err != nil || found {
		t.Fatalf("Load(second) after failed replacement found=%t err=%v", found, err)
	}
}

func TestFileStoreMigratesLegacyRecordOnLoad(t *testing.T) {
	path := t.TempDir() + "/checkpoint.store"
	config := testFileConfig()
	config.Format.MigrateOnLoad = true
	legacyConfig := config.Config
	legacyConfig.Format = FormatConfig{Version: RecordFormatV1, Compatibility: CompatibilityCurrentOnly}
	legacy, err := marshalCheckpoint(Checkpoint{Snapshot: testSnapshot(t), Cursor: 41, Outbox: []byte("pending")}, legacyConfig)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := marshalFileRecords(map[string][]byte{"tasks": legacy}, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	store := testFileStore(t, path, config)
	defer func() { _ = store.Close() }()
	checkpoint, found, err := store.Load("tasks")
	if err != nil || !found || checkpoint.Cursor != 41 {
		t.Fatalf("Load() checkpoint=%+v found=%t err=%v", checkpoint, found, err)
	}
	store.mu.RLock()
	migrated := append([]byte(nil), store.records["tasks"]...)
	store.mu.RUnlock()
	if _, version, err := decodeCheckpoint(migrated, store.config.Config); err != nil || version != RecordFormatV2 {
		t.Fatalf("migrated record version=%d err=%v, want %d", version, err, RecordFormatV2)
	}
}

func TestFileStoreConcurrentSavesAndLoads(t *testing.T) {
	store := testFileStore(t, t.TempDir()+"/checkpoint.store", testFileConfig())
	defer func() { _ = store.Close() }()
	saved := testSnapshot(t)
	const workers = 12
	const writesPerWorker = 12
	errorsSeen := make(chan error, workers*writesPerWorker)
	var writers sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		writers.Add(1)
		go func(worker int) {
			defer writers.Done()
			name := fmt.Sprintf("document-%d", worker)
			for write := 0; write < writesPerWorker; write++ {
				if err := store.Save(name, Checkpoint{Snapshot: saved, Cursor: uint64(write), Outbox: []byte{byte(worker)}}); err != nil {
					errorsSeen <- err
					return
				}
				checkpoint, found, err := store.Load(name)
				if err != nil || !found || checkpoint.Snapshot.TypeID != saved.TypeID {
					errorsSeen <- fmt.Errorf("worker %d load found=%t checkpoint=%+v err=%v", worker, found, checkpoint, err)
					return
				}
			}
		}(worker)
	}
	writers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Error(err)
	}
}

func testFileConfig() FileConfig {
	return FileConfig{Config: testConfig(), MaxStoreBytes: 64 << 10}
}

func testFileStore(t *testing.T, path string, config FileConfig) *FileStore {
	t.Helper()
	store, err := OpenFile(path, config)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	return store
}
