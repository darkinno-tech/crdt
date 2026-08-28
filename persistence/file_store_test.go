package persistence

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/set"
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
	privatePath := root + "/private.store"
	if err := os.WriteFile(privatePath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := root + "/checkpoint.link"
	if err := os.Symlink(privatePath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := OpenFile(linkPath, config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("OpenFile() symlink error = %v, want %v", err, ErrInvalidConfig)
	}
}

func TestFileStoreValidatesConfigurationAndOperationBoundaries(t *testing.T) {
	root := t.TempDir()
	valid := testFileConfig()
	if _, err := valid.normalized(); err != nil {
		t.Fatalf("valid FileConfig was rejected: %v", err)
	}
	for _, config := range []FileConfig{
		{Config: valid.Config},
		{Config: Config{MaxRecordBytes: valid.MaxRecordBytes}, MaxStoreBytes: valid.MaxStoreBytes},
	} {
		if _, err := config.normalized(); err == nil {
			t.Fatalf("invalid FileConfig was accepted: %+v", config)
		}
	}
	if _, err := OpenFile("", valid); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("OpenFile(empty path) error = %v, want %v", err, ErrInvalidConfig)
	}
	if _, err := OpenFile(root+"/checkpoint.store", FileConfig{Config: valid.Config}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("OpenFile(invalid config) error = %v, want %v", err, ErrInvalidConfig)
	}
	parentFile := root + "/parent-file"
	if err := os.WriteFile(parentFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(parentFile+"/checkpoint.store", valid); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("OpenFile(file parent) error = %v, want %v", err, ErrInvalidConfig)
	}

	store := testFileStore(t, root+"/checkpoint.store", valid)
	if checkpoint, found, err := store.Load("missing"); err != nil || found || checkpoint.Cursor != 0 || len(checkpoint.Outbox) != 0 {
		t.Fatalf("Load(missing) = %+v, found=%t, err=%v", checkpoint, found, err)
	}
	for _, operation := range []struct {
		name string
		err  error
	}{
		{name: "save", err: store.Save("invalid/name", Checkpoint{Snapshot: testSnapshot(t)})},
		{name: "load", err: func() error { _, _, err := store.Load("invalid/name"); return err }()},
		{name: "delete", err: func() error { _, err := store.Delete("invalid/name"); return err }()},
	} {
		if !errors.Is(operation.err, ErrInvalidCheckpoint) {
			t.Errorf("%s invalid name error = %v, want %v", operation.name, operation.err, ErrInvalidCheckpoint)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close() error = %v, want %v", err, ErrClosed)
	}
	if err := store.Save("active", Checkpoint{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Save() after Close error = %v, want %v", err, ErrClosed)
	}
	if _, _, err := store.Load("active"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Load() after Close error = %v, want %v", err, ErrClosed)
	}
	if _, err := store.Delete("active"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Delete() after Close error = %v, want %v", err, ErrClosed)
	}
}

func TestFileRecordEncodingRejectsMalformedAndOversizedRecords(t *testing.T) {
	config := testFileConfig()
	checkpoint := Checkpoint{Snapshot: testSnapshot(t), Cursor: 7, Outbox: []byte("outbox")}
	record, err := marshalCheckpoint(checkpoint, config.Config)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := marshalFileRecords(map[string][]byte{"active": record}, config)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := unmarshalFileRecords(encoded, config)
	if err != nil || string(decoded["active"]) != string(record) {
		t.Fatalf("unmarshalFileRecords() records=%v err=%v", decoded, err)
	}
	for _, records := range []map[string][]byte{
		{"invalid/name": record},
		{"active": []byte("not-a-checkpoint")},
	} {
		if _, err := marshalFileRecords(records, config); !errors.Is(err, ErrInvalidCheckpoint) && !errors.Is(err, ErrCorruptStore) {
			t.Fatalf("marshalFileRecords(%v) error = %v", records, err)
		}
	}
	small := config
	small.MaxStoreBytes = len(encoded) - 1
	if _, err := marshalFileRecords(map[string][]byte{"active": record}, small); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("marshalFileRecords(over budget) error = %v, want %v", err, ErrInvalidCheckpoint)
	}
	if _, err := unmarshalFileRecords(nil, config); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("unmarshalFileRecords(short) error = %v, want %v", err, ErrCorruptStore)
	}
	tampered := append([]byte(nil), encoded...)
	tampered[len(tampered)-1] ^= 1
	if _, err := unmarshalFileRecords(tampered, config); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("unmarshalFileRecords(tampered) error = %v, want %v", err, ErrCorruptStore)
	}
	trailing := append([]byte(nil), encoded[:len(encoded)-sha256.Size]...)
	trailing = append(trailing, 0)
	digest := sha256.Sum256(trailing)
	trailing = append(trailing, digest[:]...)
	if _, err := unmarshalFileRecords(trailing, config); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("unmarshalFileRecords(trailing data) error = %v, want %v", err, ErrCorruptStore)
	}
	if _, err := replaceFile(t.TempDir()+"/missing/checkpoint.store", []byte("value")); err == nil {
		t.Fatal("replaceFile() accepted a missing parent directory")
	}
}

func TestFileRecordDecoderRejectsNonCanonicalEntries(t *testing.T) {
	config := testFileConfig()
	checkpoint, err := marshalCheckpoint(Checkpoint{Snapshot: testSnapshot(t)}, config.Config)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(count uint64, entries ...[]byte) []byte {
		payload := append([]byte(nil), fileMagic[:]...)
		payload = append(payload, fileVersion)
		payload = frame.AppendUvarint(payload, count)
		for _, entry := range entries {
			payload = appendBytes(payload, entry)
		}
		digest := sha256.Sum256(payload)
		return append(payload, digest[:]...)
	}
	for _, data := range [][]byte{
		encode(1),
		encode(2),
		encode(1, []byte("invalid/name"), checkpoint),
		encode(1, []byte("active"), nil),
		encode(1, []byte("active"), []byte("not-a-checkpoint")),
		encode(2, []byte("same"), checkpoint, []byte("same"), checkpoint),
	} {
		if _, err := unmarshalFileRecords(data, config); !errors.Is(err, ErrCorruptStore) {
			t.Fatalf("unmarshalFileRecords(non-canonical entry) error = %v, want %v", err, ErrCorruptStore)
		}
	}
	for _, mutate := range []func([]byte){
		func(data []byte) { data[0] ^= 1 },
		func(data []byte) { data[len(fileMagic)]++ },
	} {
		data := encode(0)
		mutate(data)
		payloadEnd := len(data) - sha256.Size
		digest := sha256.Sum256(data[:payloadEnd])
		copy(data[payloadEnd:], digest[:])
		if _, err := unmarshalFileRecords(data, config); !errors.Is(err, ErrCorruptStore) {
			t.Fatalf("unmarshalFileRecords(bad header) error = %v, want %v", err, ErrCorruptStore)
		}
	}
}

func TestFileStoreCoversMigrationAndFileBoundaryFailures(t *testing.T) {
	path := t.TempDir() + "/checkpoint.store"
	config := testFileConfig()
	config.Format.MigrateOnLoad = true
	store := testFileStore(t, path, config)
	if checkpoint, found, err := store.migrateAndLoad("missing"); err != nil || found || checkpoint.Cursor != 0 {
		t.Fatalf("migrateAndLoad(missing) = %+v, found=%t, err=%v", checkpoint, found, err)
	}
	store.records["corrupt"] = []byte("not-a-checkpoint")
	if _, _, err := store.migrateAndLoad("corrupt"); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("migrateAndLoad(corrupt) error = %v, want %v", err, ErrCorruptStore)
	}
	if err := store.Save("empty", Checkpoint{}); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Save(empty checkpoint) error = %v, want %v", err, ErrInvalidCheckpoint)
	}
	store.config.Format.Version = 99
	if err := store.Save("invalid-format", Checkpoint{Snapshot: testSnapshot(t)}); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Save(invalid format) error = %v, want %v", err, ErrInvalidCheckpoint)
	}
	store.config.Format = config.Format
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.migrateAndLoad("missing"); !errors.Is(err, ErrClosed) {
		t.Fatalf("migrateAndLoad() after Close error = %v, want %v", err, ErrClosed)
	}

	root := t.TempDir()
	if _, err := loadFileRecords(root, testFileConfig()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("loadFileRecords(directory) error = %v, want %v", err, ErrInvalidConfig)
	}
	parentFile := root + "/parent-file"
	if err := os.WriteFile(parentFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFileRecords(parentFile+"/checkpoint.store", config); err == nil {
		t.Fatal("loadFileRecords() accepted a child of a regular file")
	}
	oversized := root + "/oversized.store"
	if err := os.WriteFile(oversized, make([]byte, config.MaxStoreBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFileRecords(oversized, config); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("loadFileRecords(oversized) error = %v, want %v", err, ErrCorruptStore)
	}
	link := root + "/checkpoint.link"
	if err := os.Symlink(oversized, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFileRecords(link, config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("loadFileRecords(symlink) error = %v, want %v", err, ErrInvalidConfig)
	}
}

func TestFileStoreCoversCurrentAndRejectedMigrationPaths(t *testing.T) {
	path := t.TempDir() + "/checkpoint.store"
	currentConfig := testFileConfig()
	currentConfig.Format.MigrateOnLoad = true
	store := testFileStore(t, path, currentConfig)
	checkpoint := Checkpoint{Snapshot: testSnapshot(t)}
	if err := store.Save("active", checkpoint); err != nil {
		t.Fatal(err)
	}
	if loaded, found, err := store.migrateAndLoad("active"); err != nil || !found || loaded.Snapshot.TypeID != checkpoint.Snapshot.TypeID {
		t.Fatalf("migrateAndLoad(current) = %+v, found=%t, err=%v", loaded, found, err)
	}
	store.records["corrupt"] = []byte("not-a-checkpoint")
	if _, err := store.Delete("active"); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("Delete() with corrupt sibling error = %v, want %v", err, ErrCorruptStore)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	failingConfig := testFileConfig()
	failingConfig.Format = FormatConfig{
		MigrateOnLoad: true,
		Migrations: []Migration{{
			FromVersion: RecordFormatV1,
			Transform: func(Checkpoint) (Checkpoint, error) {
				return Checkpoint{}, errors.New("transform rejected")
			},
		}},
	}
	legacy := marshalLegacyCheckpoint(t, checkpoint)
	encoded, err := marshalFileRecords(map[string][]byte{"legacy": legacy}, failingConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	store = testFileStore(t, path, failingConfig)
	defer func() { _ = store.Close() }()
	if _, _, err := store.Load("legacy"); !errors.Is(err, ErrMigration) {
		t.Fatalf("Load(legacy migration failure) error = %v, want %v", err, ErrMigration)
	}
	if _, err := replaceFile(t.TempDir(), []byte("value")); err == nil {
		t.Fatal("replaceFile() replaced a directory")
	}
	replacedPath := t.TempDir() + "/replaced.store"
	if replaced, err := replaceFile(replacedPath, []byte("durable")); err != nil || !replaced {
		t.Fatalf("replaceFile() replaced=%t err=%v", replaced, err)
	}
	if contents, err := os.ReadFile(replacedPath); err != nil || string(contents) != "durable" {
		t.Fatalf("replaceFile() contents=%q err=%v", contents, err)
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

func TestFileStoreDeletePersistsRetentionBoundary(t *testing.T) {
	path := t.TempDir() + "/checkpoint.store"
	store := testFileStore(t, path, testFileConfig())
	saved := testSnapshot(t)
	for _, name := range []string{"active", "retired"} {
		if err := store.Save(name, Checkpoint{Snapshot: saved}); err != nil {
			t.Fatalf("Save(%q) error = %v", name, err)
		}
	}
	deleted, err := store.Delete("retired")
	if err != nil || !deleted {
		t.Fatalf("Delete(retired) deleted=%t err=%v", deleted, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = testFileStore(t, path, testFileConfig())
	defer func() { _ = store.Close() }()
	if _, found, err := store.Load("retired"); err != nil || found {
		t.Fatalf("restart Load(retired) found=%t err=%v", found, err)
	}
	if checkpoint, found, err := store.Load("active"); err != nil || !found || checkpoint.Snapshot.TypeID != saved.TypeID {
		t.Fatalf("restart Load(active) checkpoint=%+v found=%t err=%v", checkpoint, found, err)
	}
	if deleted, err := store.Delete("retired"); err != nil || deleted {
		t.Fatalf("second Delete(retired) deleted=%t err=%v", deleted, err)
	}
	if _, err := store.Delete("invalid/name"); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Delete(invalid name) error = %v, want %v", err, ErrInvalidCheckpoint)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Delete("active"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Delete() after Close error = %v, want %v", err, ErrClosed)
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
				if err != nil {
					errorsSeen <- fmt.Errorf("worker %d load: %w", worker, err)
					return
				}
				if !found || checkpoint.Snapshot.TypeID != saved.TypeID {
					errorsSeen <- fmt.Errorf("worker %d load found=%t checkpoint=%+v", worker, found, checkpoint)
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

func TestFileStoreConfigurationClosedAndCodecBoundaries(t *testing.T) {
	root := t.TempDir()
	config := testFileConfig()
	if _, err := OpenFile("", config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("OpenFile() empty path error = %v", err)
	}
	invalidSize := config
	invalidSize.MaxStoreBytes = 0
	if _, err := OpenFile(root+"/checkpoint.store", invalidSize); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("OpenFile() invalid size error = %v", err)
	}
	invalidValidator := config
	invalidValidator.Validate = nil
	if _, err := OpenFile(root+"/checkpoint.store", invalidValidator); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("OpenFile() invalid validator error = %v", err)
	}
	parentFile := root + "/parent-file"
	if err := os.WriteFile(parentFile, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(parentFile+"/checkpoint.store", config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("OpenFile() file parent error = %v", err)
	}
	if _, err := OpenFile(root, config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("OpenFile() directory path error = %v", err)
	}

	store := testFileStore(t, root+"/checkpoint.store", config)
	if _, found, err := store.Load("missing"); err != nil || found {
		t.Fatalf("Load(missing) found=%t err=%v", found, err)
	}
	if err := store.Save("invalid/name", Checkpoint{Snapshot: testSnapshot(t)}); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Save(invalid name) error = %v", err)
	}
	if err := store.Save("invalid", Checkpoint{}); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Save(invalid checkpoint) error = %v", err)
	}
	if _, _, err := store.Load("invalid/name"); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Load(invalid name) error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close() error = %v", err)
	}
	if err := store.Save("checkpoint", Checkpoint{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Save() after Close error = %v", err)
	}
	var nilStore *FileStore
	if err := nilStore.Save("checkpoint", Checkpoint{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Save() error = %v", err)
	}
	if _, _, err := nilStore.Load("checkpoint"); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Load() error = %v", err)
	}
	if _, err := nilStore.Delete("checkpoint"); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Delete() error = %v", err)
	}

	record, err := marshalCheckpoint(Checkpoint{Snapshot: testSnapshot(t)}, config.Config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unmarshalCheckpoint(record, config.Config); err != nil {
		t.Fatalf("unmarshal current checkpoint error = %v", err)
	}
	if _, err := unmarshalCheckpoint([]byte("corrupt"), config.Config); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("unmarshal corrupt checkpoint error = %v", err)
	}
	migrationConfig := config.Config
	migrationConfig.Format.MigrateOnLoad = true
	legacyConfig := migrationConfig
	legacyConfig.Format = FormatConfig{Version: RecordFormatV1, Compatibility: CompatibilityCurrentOnly}
	legacy, err := marshalCheckpoint(Checkpoint{Snapshot: testSnapshot(t)}, legacyConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unmarshalCheckpoint(legacy, migrationConfig); err != nil {
		t.Fatalf("unmarshal legacy checkpoint error = %v", err)
	}
	encoded, err := marshalFileRecords(map[string][]byte{"checkpoint": record}, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unmarshalFileRecords(encoded[:len(encoded)-1], config); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("unmarshal truncated record error = %v", err)
	}
	corrupt := append([]byte(nil), encoded...)
	corrupt[0] ^= 1
	if _, err := unmarshalFileRecords(corrupt, config); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("unmarshal corrupt magic error = %v", err)
	}
	if _, err := loadFileRecords(root, config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("load directory error = %v", err)
	}
	smallStore := config
	smallStore.MaxStoreBytes = 1
	oversized := root + "/oversized.store"
	if err := os.WriteFile(oversized, []byte("too large"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFileRecords(oversized, smallStore); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("load oversized file error = %v", err)
	}
	if _, err := marshalFileRecords(map[string][]byte{"invalid/name": record}, config); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("marshal invalid name error = %v", err)
	}
	if _, err := marshalFileRecords(map[string][]byte{"checkpoint": []byte("not-a-record")}, config); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("marshal corrupt record error = %v", err)
	}
	invalidFormat := config.Config
	invalidFormat.Format.Version = 99
	if _, err := marshalCheckpoint(Checkpoint{Snapshot: testSnapshot(t)}, invalidFormat); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("marshal invalid format error = %v", err)
	}
	if _, err := normalizeCheckpoint(Checkpoint{}, config.Config); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("normalize empty checkpoint error = %v", err)
	}
	if format := effectiveFormat(Config{Format: FormatConfig{Version: 99}}); format.Version != 0 {
		t.Fatalf("effective invalid format = %+v", format)
	}
	tightRecord := config.Config
	tightRecord.MaxRecordBytes = 1
	if _, err := checkpointSize(Checkpoint{Snapshot: testSnapshot(t)}, tightRecord); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("checkpoint size over budget error = %v", err)
	}
	if _, err := migrateCheckpoint(Checkpoint{Snapshot: testSnapshot(t)}, RecordFormatV2, config.Config); !errors.Is(err, ErrMigration) {
		t.Fatalf("migrate current format error = %v", err)
	}
	if _, err := applyMigration(func(Checkpoint) (Checkpoint, error) { panic("migration panic") }, Checkpoint{}); !errors.Is(err, ErrMigration) {
		t.Fatalf("panic migration error = %v", err)
	}
	if replaced, err := replaceFile(root+"/missing/checkpoint.store", []byte("state")); replaced || err == nil {
		t.Fatalf("replace missing parent replaced=%t err=%v", replaced, err)
	}
	if replaced, err := replaceFile(root, []byte("state")); replaced || err == nil {
		t.Fatalf("replace directory target replaced=%t err=%v", replaced, err)
	}
	tightStore := config
	tightStore.MaxStoreBytes = len(record)
	if _, err := marshalFileRecords(map[string][]byte{"checkpoint": record}, tightStore); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("marshal over-budget record error = %v", err)
	}
}

func TestFileStoreMigrationClosedMissingAndCorruptBoundaries(t *testing.T) {
	config := testFileConfig()
	config.Format.MigrateOnLoad = true
	store := testFileStore(t, t.TempDir()+"/checkpoint.store", config)
	if _, found, err := store.migrateAndLoad("missing"); err != nil || found {
		t.Fatalf("migrate missing found=%t err=%v", found, err)
	}
	store.records["corrupt"] = []byte("not-a-checkpoint")
	if _, _, err := store.migrateAndLoad("corrupt"); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("migrate corrupt record error = %v", err)
	}
	delete(store.records, "corrupt")
	if err := store.Save("current", Checkpoint{Snapshot: testSnapshot(t)}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.migrateAndLoad("current"); err != nil || !found {
		t.Fatalf("migrate current record found=%t err=%v", found, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.migrateAndLoad("missing"); !errors.Is(err, ErrClosed) {
		t.Fatalf("migrate after Close error = %v", err)
	}
}

func TestFileStoreMigrationFailsClosedWhenTransformPanics(t *testing.T) {
	config := testFileConfig()
	config.Format.MigrateOnLoad = true
	config.Format.Migrations = []Migration{{
		FromVersion: RecordFormatV1,
		Transform:   func(Checkpoint) (Checkpoint, error) { panic("migration panic") },
	}}
	legacyConfig := config.Config
	legacyConfig.Format = FormatConfig{Version: RecordFormatV1, Compatibility: CompatibilityCurrentOnly}
	legacy, err := marshalCheckpoint(Checkpoint{Snapshot: testSnapshot(t)}, legacyConfig)
	if err != nil {
		t.Fatal(err)
	}
	store := testFileStore(t, t.TempDir()+"/checkpoint.store", config)
	defer func() { _ = store.Close() }()
	store.records["legacy"] = legacy
	if _, _, err := store.migrateAndLoad("legacy"); !errors.Is(err, ErrMigration) {
		t.Fatalf("panic migration error = %v", err)
	}
}

func TestUnmarshalFileRecordsRejectsCanonicalStructureViolations(t *testing.T) {
	config := testFileConfig()
	record, err := marshalCheckpoint(Checkpoint{Snapshot: testSnapshot(t)}, config.Config)
	if err != nil {
		t.Fatal(err)
	}
	for name, payload := range map[string][]byte{
		"count-exceeds-bytes": filePayloadWithCount(2),
		"missing-name":        filePayloadWithCount(1),
		"trailing-bytes":      append(filePayloadWithCount(0), 'x'),
		"empty-record":        append(appendBytes(filePayloadWithCount(1), []byte("checkpoint")), 0),
		"invalid-record":      appendBytes(appendBytes(filePayloadWithCount(1), []byte("checkpoint")), []byte("invalid")),
		"duplicate-name": appendBytes(
			appendBytes(
				appendBytes(
					appendBytes(filePayloadWithCount(2), []byte("checkpoint")), record,
				),
				[]byte("checkpoint"),
			),
			record,
		),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := unmarshalFileRecords(sealFilePayload(payload), config); !errors.Is(err, ErrCorruptStore) {
				t.Fatalf("unmarshal error = %v, want %v", err, ErrCorruptStore)
			}
		})
	}
}

func filePayloadWithCount(count uint64) []byte {
	payload := append([]byte(nil), fileMagic[:]...)
	payload = append(payload, fileVersion)
	return frame.AppendUvarint(payload, count)
}

func sealFilePayload(payload []byte) []byte {
	digest := sha256.Sum256(payload)
	return append(payload, digest[:]...)
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
