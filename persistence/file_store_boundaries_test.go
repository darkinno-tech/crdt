package persistence

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestFileStoreConfigurationAndClosedBoundaries(t *testing.T) {
	if (FileConfig{}).valid() {
		t.Fatal("zero FileConfig is valid")
	}
	if _, err := (FileConfig{MaxStoreBytes: 1}).normalized(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("normalized invalid Config error = %v", err)
	}
	if _, err := OpenFile("", testFileConfig()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("OpenFile empty path error = %v", err)
	}
	if _, err := OpenFile(filepath.Join(t.TempDir(), "checkpoint.store"), FileConfig{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("OpenFile invalid config error = %v", err)
	}

	root := t.TempDir()
	parentFile := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(parentFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(filepath.Join(parentFile, "checkpoint"), testFileConfig()); err == nil {
		t.Fatal("OpenFile accepted a file as its parent")
	}
	if _, err := OpenFile(root, testFileConfig()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("OpenFile directory path error = %v", err)
	}

	var nilStore *FileStore
	if err := nilStore.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Close() error = %v", err)
	}
	if _, _, err := nilStore.Load("checkpoint"); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Load() error = %v", err)
	}
	if err := nilStore.Save("checkpoint", Checkpoint{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Save() error = %v", err)
	}

	store := testFileStore(t, filepath.Join(root, "checkpoint.store"), testFileConfig())
	if _, _, err := store.Load("bad/name"); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Load invalid name error = %v", err)
	}
	if err := store.Save("bad/name", Checkpoint{}); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Save invalid name error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close() error = %v", err)
	}
	if _, _, err := store.Load("checkpoint"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Load after Close error = %v", err)
	}
}

func TestFileStoreRejectsOversizeAndSymlinkFiles(t *testing.T) {
	root := t.TempDir()
	config := testFileConfig()
	path := filepath.Join(root, "checkpoint.store")
	if err := os.WriteFile(path, make([]byte, config.MaxStoreBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(path, config); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("OpenFile oversize error = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err == nil {
		if _, err := OpenFile(path, config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("OpenFile symlink error = %v", err)
		}
	}
}

func TestFileRecordEnvelopeRejectsCanonicalViolations(t *testing.T) {
	config := testFileConfig()
	record := testFileRecord(t, config.Config)
	encoded, err := marshalFileRecords(map[string][]byte{"checkpoint": record}, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := marshalFileRecords(map[string][]byte{"bad/name": record}, config); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("marshal invalid name error = %v", err)
	}
	if _, err := marshalFileRecords(map[string][]byte{"checkpoint": []byte("bad")}, config); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("marshal invalid record error = %v", err)
	}
	tooSmall := config
	tooSmall.MaxStoreBytes = len(encoded) - 1
	if _, err := marshalFileRecords(map[string][]byte{"checkpoint": record}, tooSmall); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("marshal oversize store error = %v", err)
	}

	cases := []struct {
		name string
		data []byte
	}{
		{name: "too short", data: []byte("CRFS")},
		{name: "bad magic", data: mutateFileEnvelope(encoded, func(data []byte) { data[0] ^= 1 })},
		{name: "malformed count", data: mutateFileEnvelope(encoded, func(data []byte) { data[len(fileMagic)+1] = 0xff })},
		{name: "excessive count", data: mutateFileEnvelope(encoded, func(data []byte) { data[len(fileMagic)+1] = 127 })},
		{name: "bad count", data: mutateFileEnvelope(encoded, func(data []byte) { data[len(fileMagic)+1] = 2 })},
		{name: "invalid name", data: mutateFileEnvelope(encoded, func(data []byte) { data[len(fileMagic)+3] = '/' })},
		{name: "corrupt record", data: mutateFileEnvelope(encoded, func(data []byte) { data[len(data)-sha256.Size-1] ^= 1 })},
		{name: "trailing payload", data: appendFilePayload(encoded, 1)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := unmarshalFileRecords(test.data, config); !errors.Is(err, ErrCorruptStore) {
				t.Fatalf("unmarshal error = %v, want %v", err, ErrCorruptStore)
			}
		})
	}
}

func TestReplaceFileRejectsDirectoryDestination(t *testing.T) {
	root := t.TempDir()
	if replaced, err := replaceFile(filepath.Join(root, "missing", "checkpoint.store"), []byte("checkpoint")); err == nil || replaced {
		t.Fatalf("replaceFile missing parent = replaced=%t err=%v", replaced, err)
	}
	if replaced, err := replaceFile(root, []byte("checkpoint")); err == nil || replaced {
		t.Fatalf("replaceFile directory = replaced=%t err=%v", replaced, err)
	}
}

func TestReplaceFileWritesPrivateAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.store")
	if replaced, err := replaceFile(path, []byte("first")); err != nil || !replaced {
		t.Fatalf("first replace = replaced=%t err=%v", replaced, err)
	}
	if replaced, err := replaceFile(path, []byte("second")); err != nil || !replaced {
		t.Fatalf("second replace = replaced=%t err=%v", replaced, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "second" {
		t.Fatalf("replacement data=%q err=%v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("replacement mode=%#o err=%v", info.Mode().Perm(), err)
	}
}

func TestFileStoreMissingAndInvalidCheckpointBoundaries(t *testing.T) {
	config := testFileConfig()
	store := testFileStore(t, filepath.Join(t.TempDir(), "checkpoint.store"), config)
	defer func() { _ = store.Close() }()
	if checkpoint, found, err := store.Load("missing"); err != nil || found || checkpoint.Snapshot.TypeID != 0 {
		t.Fatalf("Load missing checkpoint=%+v found=%t err=%v", checkpoint, found, err)
	}
	if err := store.Save("checkpoint", Checkpoint{}); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Save invalid checkpoint error = %v, want %v", err, ErrInvalidCheckpoint)
	}
	if err := store.Save("checkpoint", Checkpoint{Snapshot: testSnapshotForFuzz(t), Outbox: make([]byte, config.MaxOutboxBytes+1)}); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Save oversized outbox error = %v, want %v", err, ErrInvalidCheckpoint)
	}
}

func TestFileStoreDeleteFailureBoundaries(t *testing.T) {
	var nilStore *FileStore
	if deleted, err := nilStore.Delete("checkpoint"); deleted || !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Delete() deleted=%t err=%v", deleted, err)
	}

	root := t.TempDir()
	path := filepath.Join(root, "checkpoint.store")
	store := testFileStore(t, path, testFileConfig())
	defer func() { _ = store.Close() }()
	if err := store.Save("checkpoint", Checkpoint{Snapshot: testSnapshotForFuzz(t)}); err != nil {
		t.Fatal(err)
	}

	store.mu.Lock()
	store.closed.Store(true)
	store.mu.Unlock()
	if deleted, err := store.Delete("checkpoint"); deleted || !errors.Is(err, ErrClosed) {
		t.Fatalf("Delete() after lock boundary deleted=%t err=%v", deleted, err)
	}
	store.closed.Store(false)

	store.records["corrupt"] = []byte("not a checkpoint")
	if deleted, err := store.Delete("checkpoint"); deleted || !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("Delete() corrupt retained record deleted=%t err=%v", deleted, err)
	}
	delete(store.records, "corrupt")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	if deleted, err := store.Delete("checkpoint"); deleted || err == nil {
		t.Fatalf("Delete() missing directory deleted=%t err=%v", deleted, err)
	}
	if checkpoint, found, err := store.Load("checkpoint"); err != nil || !found || checkpoint.Snapshot.TypeID == 0 {
		t.Fatalf("failed Delete() changed in-memory checkpoint=%+v found=%t err=%v", checkpoint, found, err)
	}
}

func TestBoltStoreDeleteFailureBoundaries(t *testing.T) {
	var nilStore *BoltStore
	if deleted, err := nilStore.Delete("checkpoint"); deleted || !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Delete() deleted=%t err=%v", deleted, err)
	}

	store := testStore(t, filepath.Join(t.TempDir(), "checkpoint.db"), testConfig())
	defer func() { _ = store.Close() }()
	if err := store.db.Update(func(transaction *bolt.Tx) error {
		return transaction.DeleteBucket(checkpointBucket)
	}); err != nil {
		t.Fatal(err)
	}
	if deleted, err := store.Delete("checkpoint"); deleted || !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("Delete() missing bucket deleted=%t err=%v", deleted, err)
	}
}

func TestFileStoreMigrationFailureBoundaries(t *testing.T) {
	config := testFileConfig()
	config.Format.MigrateOnLoad = true
	legacyConfig := config.Config
	legacyConfig.Format = FormatConfig{Version: RecordFormatV1, Compatibility: CompatibilityCurrentOnly}
	legacy := testFileRecord(t, legacyConfig)
	current := testFileRecord(t, config.Config)

	store := testFileStore(t, filepath.Join(t.TempDir(), "checkpoint.store"), config)
	defer func() { _ = store.Close() }()
	if _, found, err := store.migrateAndLoad("missing"); err != nil || found {
		t.Fatalf("migrate missing found=%t err=%v", found, err)
	}
	store.records["legacy"] = legacy
	if checkpoint, found, err := store.migrateAndLoad("legacy"); err != nil || !found || checkpoint.Cursor != 7 {
		t.Fatalf("migrate legacy checkpoint=%+v found=%t err=%v", checkpoint, found, err)
	}
	store.records["current"] = current
	if checkpoint, found, err := store.migrateAndLoad("current"); err != nil || !found || checkpoint.Cursor != 7 {
		t.Fatalf("migrate current checkpoint=%+v found=%t err=%v", checkpoint, found, err)
	}
	store.records["corrupt"] = []byte("corrupt")
	if _, _, err := store.migrateAndLoad("corrupt"); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("migrate corrupt error = %v", err)
	}

	failing := config
	failing.Format.Migrations = []Migration{{
		FromVersion: RecordFormatV1,
		Transform:   func(Checkpoint) (Checkpoint, error) { return Checkpoint{}, errors.New("transform failed") },
	}}
	failedStore := testFileStore(t, filepath.Join(t.TempDir(), "failed.store"), failing)
	defer func() { _ = failedStore.Close() }()
	failedStore.records["legacy"] = legacy
	if _, _, err := failedStore.migrateAndLoad("legacy"); !errors.Is(err, ErrMigration) {
		t.Fatalf("migrate transform error = %v, want %v", err, ErrMigration)
	}
}

func testFileRecord(t testing.TB, config Config) []byte {
	t.Helper()
	record, err := marshalCheckpoint(Checkpoint{Snapshot: testSnapshotForFuzz(t), Cursor: 7, Outbox: []byte("pending")}, config)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func mutateFileEnvelope(encoded []byte, mutate func([]byte)) []byte {
	copy := append([]byte(nil), encoded...)
	payloadEnd := len(copy) - sha256.Size
	mutate(copy[:payloadEnd])
	digest := sha256.Sum256(copy[:payloadEnd])
	copy = append(copy[:payloadEnd], digest[:]...)
	return copy
}

func appendFilePayload(encoded []byte, value byte) []byte {
	payloadEnd := len(encoded) - sha256.Size
	copy := append([]byte(nil), encoded[:payloadEnd]...)
	copy = append(copy, value)
	digest := sha256.Sum256(copy)
	return append(copy, digest[:]...)
}
