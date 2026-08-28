package persistence

import (
	"bytes"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/darkinno-tech/crdt/snapshot"
	bolt "go.etcd.io/bbolt"
)

func TestStoreMigratesLegacyRecordOnLoad(t *testing.T) {
	config := testConfig()
	config.Format.MigrateOnLoad = true
	store := testStore(t, t.TempDir()+"/checkpoint.db", config)
	defer func() { _ = store.Close() }()

	checkpoint := Checkpoint{Snapshot: testSnapshot(t), Cursor: 41, Outbox: []byte("pending")}
	legacy := marshalLegacyCheckpoint(t, checkpoint)
	putRawCheckpoint(t, store, "tasks", legacy)

	loaded, found, err := store.Load("tasks")
	if err != nil || !found {
		t.Fatalf("Load() found=%t err=%v", found, err)
	}
	if loaded.Cursor != checkpoint.Cursor || string(loaded.Outbox) != string(checkpoint.Outbox) {
		t.Fatalf("migrated checkpoint = %+v", loaded)
	}
	migrated := storedCheckpoint(t, store, "tasks")
	if migrated[len(recordMagic)] != RecordFormatV2 {
		t.Fatalf("stored record version = %d, want %d", migrated[len(recordMagic)], RecordFormatV2)
	}
	if _, version, err := decodeCheckpoint(migrated, store.config); err != nil || version != RecordFormatV2 {
		t.Fatalf("migrated record version=%d err=%v", version, err)
	}
}

func TestStoreMigrationUsesSourceValidatorAndTransform(t *testing.T) {
	counterState, err := testCounterState()
	if err != nil {
		t.Fatal(err)
	}
	legacyConfig := testConfig()
	legacyConfig.Validate = validateTestCounter
	legacyConfig.Format = FormatConfig{Version: RecordFormatV1, Compatibility: CompatibilityCurrentOnly}
	legacySnapshot, err := snapshot.NewValidated(counterState, nil, validateTestCounter)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := marshalCheckpoint(Checkpoint{Snapshot: legacySnapshot, Cursor: 7}, legacyConfig)
	if err != nil {
		t.Fatal(err)
	}

	config := testConfig()
	config.Format = FormatConfig{
		MigrateOnLoad: true,
		Migrations: []Migration{{
			FromVersion: RecordFormatV1,
			Validate:    validateTestCounter,
			Transform: func(Checkpoint) (Checkpoint, error) {
				return Checkpoint{Snapshot: testSnapshot(t), Cursor: 99, Outbox: []byte("rewritten")}, nil
			},
		}},
	}
	store := testStore(t, t.TempDir()+"/checkpoint.db", config)
	defer func() { _ = store.Close() }()
	putRawCheckpoint(t, store, "tasks", legacy)

	loaded, found, err := store.Load("tasks")
	if err != nil || !found {
		t.Fatalf("Load() found=%t err=%v", found, err)
	}
	if loaded.Cursor != 99 || string(loaded.Outbox) != "rewritten" {
		t.Fatalf("transformed checkpoint = %+v", loaded)
	}
}

func TestStoreRejectsLegacyRecordWhenCompatibilityIsDisabled(t *testing.T) {
	config := testConfig()
	config.Format = FormatConfig{Version: RecordFormatV2, Compatibility: CompatibilityCurrentOnly}
	store := testStore(t, t.TempDir()+"/checkpoint.db", config)
	defer func() { _ = store.Close() }()
	legacy := marshalLegacyCheckpoint(t, Checkpoint{Snapshot: testSnapshot(t)})
	putRawCheckpoint(t, store, "tasks", legacy)

	if _, _, err := store.Load("tasks"); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("Load() error = %v, want %v", err, ErrCorruptStore)
	}
	if got := storedCheckpoint(t, store, "tasks"); !bytes.Equal(got, legacy) {
		t.Fatal("rejected legacy record was modified")
	}
}

func TestStoreMigrationFailureLeavesLegacyRecordUntouched(t *testing.T) {
	config := testConfig()
	config.Format = FormatConfig{
		MigrateOnLoad: true,
		Migrations: []Migration{{
			FromVersion: RecordFormatV1,
			Transform: func(Checkpoint) (Checkpoint, error) {
				return Checkpoint{}, errors.New("schema transform failed")
			},
		}},
	}
	store := testStore(t, t.TempDir()+"/checkpoint.db", config)
	defer func() { _ = store.Close() }()
	legacy := marshalLegacyCheckpoint(t, Checkpoint{Snapshot: testSnapshot(t)})
	putRawCheckpoint(t, store, "tasks", legacy)

	if _, _, err := store.Load("tasks"); !errors.Is(err, ErrMigration) {
		t.Fatalf("Load() error = %v, want %v", err, ErrMigration)
	}
	if got := storedCheckpoint(t, store, "tasks"); !bytes.Equal(got, legacy) {
		t.Fatal("failed migration modified the source record")
	}
}

func TestStoreMigrationPanicLeavesLegacyRecordUntouched(t *testing.T) {
	config := testConfig()
	config.Format = FormatConfig{
		MigrateOnLoad: true,
		Migrations: []Migration{{
			FromVersion: RecordFormatV1,
			Transform: func(Checkpoint) (Checkpoint, error) {
				panic("schema transform failed")
			},
		}},
	}
	store := testStore(t, t.TempDir()+"/checkpoint.db", config)
	defer func() { _ = store.Close() }()
	legacy := marshalLegacyCheckpoint(t, Checkpoint{Snapshot: testSnapshot(t)})
	putRawCheckpoint(t, store, "tasks", legacy)

	if _, _, err := store.Load("tasks"); !errors.Is(err, ErrMigration) {
		t.Fatalf("Load() error = %v, want %v", err, ErrMigration)
	}
	if got := storedCheckpoint(t, store, "tasks"); !bytes.Equal(got, legacy) {
		t.Fatal("panicking migration modified the source record")
	}
}

func TestStoreConcurrentLegacyMigration(t *testing.T) {
	config := testConfig()
	config.Format.MigrateOnLoad = true
	store := testStore(t, t.TempDir()+"/checkpoint.db", config)
	defer func() { _ = store.Close() }()
	putRawCheckpoint(t, store, "tasks", marshalLegacyCheckpoint(t, Checkpoint{Snapshot: testSnapshot(t)}))

	var failures atomic.Int32
	const readers = 16
	done := make(chan struct{})
	for reader := 0; reader < readers; reader++ {
		go func() {
			defer func() { done <- struct{}{} }()
			checkpoint, found, err := store.Load("tasks")
			if err != nil || !found || checkpoint.Snapshot.TypeID == 0 {
				failures.Add(1)
			}
		}()
	}
	for reader := 0; reader < readers; reader++ {
		<-done
	}
	if failures.Load() != 0 {
		t.Fatalf("concurrent migration failures = %d", failures.Load())
	}
	if got := storedCheckpoint(t, store, "tasks"); got[len(recordMagic)] != RecordFormatV2 {
		t.Fatalf("stored record version = %d, want %d", got[len(recordMagic)], RecordFormatV2)
	}
}

func TestStoreCopiesMigrationConfiguration(t *testing.T) {
	migrations := []Migration{{
		FromVersion: RecordFormatV1,
		Transform: func(checkpoint Checkpoint) (Checkpoint, error) {
			checkpoint.Cursor = 23
			return checkpoint, nil
		},
	}}
	config := testConfig()
	config.Format = FormatConfig{MigrateOnLoad: true, Migrations: migrations}
	store := testStore(t, t.TempDir()+"/checkpoint.db", config)
	defer func() { _ = store.Close() }()
	migrations[0].Transform = func(Checkpoint) (Checkpoint, error) {
		return Checkpoint{}, errors.New("caller mutation")
	}
	putRawCheckpoint(t, store, "tasks", marshalLegacyCheckpoint(t, Checkpoint{Snapshot: testSnapshot(t)}))

	checkpoint, found, err := store.Load("tasks")
	if err != nil || !found || checkpoint.Cursor != 23 {
		t.Fatalf("Load() checkpoint=%+v found=%t err=%v", checkpoint, found, err)
	}
}

func TestFormatConfigurationValidation(t *testing.T) {
	defaultConfig, err := testConfig().normalized()
	if err != nil {
		t.Fatal(err)
	}
	if defaultConfig.Format.Version != RecordFormatV2 || defaultConfig.Format.Compatibility != CompatibilityCurrentAndPrevious {
		t.Fatalf("default format = %+v", defaultConfig.Format)
	}

	for _, format := range []FormatConfig{
		{Version: 99},
		{Version: RecordFormatV2, Compatibility: Compatibility(99)},
		{Version: RecordFormatV1, Compatibility: CompatibilityCurrentAndPrevious},
		{Migrations: []Migration{{FromVersion: RecordFormatV2}}},
		{Migrations: []Migration{{FromVersion: RecordFormatV1}, {FromVersion: RecordFormatV1}}},
	} {
		config := testConfig()
		config.Format = format
		if _, err := config.normalized(); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("normalized(%+v) error = %v, want %v", format, err, ErrInvalidConfig)
		}
	}
	config := testConfig()
	config.OpenTimeout = -1
	if _, err := config.normalized(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative timeout error = %v, want %v", err, ErrInvalidConfig)
	}
}

func TestMigrationCodecBoundaryPaths(t *testing.T) {
	checkpoint := Checkpoint{Snapshot: testSnapshot(t)}
	legacy := marshalLegacyCheckpoint(t, checkpoint)
	invalid := testConfig()
	invalid.Format.Version = 99
	if _, err := unmarshalCheckpoint(legacy, invalid); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("invalid format decode error = %v, want %v", err, ErrCorruptStore)
	}
	config := testConfig()
	if _, err := migrateCheckpoint(checkpoint, RecordFormatV2, config); !errors.Is(err, ErrMigration) {
		t.Fatalf("current version migration error = %v, want %v", err, ErrMigration)
	}
	config.Format.MigrateOnLoad = true
	config.Format.Migrations = []Migration{{
		FromVersion: RecordFormatV1,
		Transform: func(Checkpoint) (Checkpoint, error) {
			return Checkpoint{}, nil
		},
	}}
	if _, err := migrateCheckpoint(checkpoint, RecordFormatV1, config); !errors.Is(err, ErrMigration) {
		t.Fatalf("invalid target migration error = %v, want %v", err, ErrMigration)
	}
}

func TestMigrateAndLoadHandlesMissingAndCurrentRecords(t *testing.T) {
	store := testStore(t, t.TempDir()+"/checkpoint.db", testConfig())
	defer func() { _ = store.Close() }()
	if _, found, err := store.migrateAndLoad("missing"); err != nil || found {
		t.Fatalf("missing migrate found=%t err=%v", found, err)
	}
	checkpoint := Checkpoint{Snapshot: testSnapshot(t), Cursor: 52}
	if err := store.Save("tasks", checkpoint); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := store.migrateAndLoad("tasks")
	if err != nil || !found || loaded.Cursor != checkpoint.Cursor {
		t.Fatalf("current migrate checkpoint=%+v found=%t err=%v", loaded, found, err)
	}
}

func marshalLegacyCheckpoint(t testing.TB, checkpoint Checkpoint) []byte {
	t.Helper()
	config := testConfig()
	config.Format = FormatConfig{Version: RecordFormatV1, Compatibility: CompatibilityCurrentOnly}
	encoded, err := marshalCheckpoint(checkpoint, config)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func putRawCheckpoint(t testing.TB, store *BoltStore, name string, encoded []byte) {
	t.Helper()
	if err := store.db.Update(func(transaction *bolt.Tx) error {
		bucket := transaction.Bucket(checkpointBucket)
		if bucket == nil {
			return ErrCorruptStore
		}
		return bucket.Put([]byte(name), append([]byte(nil), encoded...))
	}); err != nil {
		t.Fatal(err)
	}
}

func storedCheckpoint(t *testing.T, store *BoltStore, name string) []byte {
	t.Helper()
	var encoded []byte
	if err := store.db.View(func(transaction *bolt.Tx) error {
		bucket := transaction.Bucket(checkpointBucket)
		if bucket == nil {
			return ErrCorruptStore
		}
		encoded = append([]byte(nil), bucket.Get([]byte(name))...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return encoded
}
