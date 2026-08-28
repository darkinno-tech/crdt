package persistence

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/darkinno-tech/crdt/set"
	"github.com/darkinno-tech/crdt/snapshot"
	bolt "go.etcd.io/bbolt"
)

type testStringCodec struct{}

func (testStringCodec) ID() string                            { return "example.com/persistence-string/v1" }
func (testStringCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (testStringCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }

func TestStorePersistsHLCCheckpointAcrossRestart(t *testing.T) {
	path := t.TempDir() + "/checkpoint.db"
	config := testConfig()
	store := testStore(t, path, config)
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
		t.Fatalf("Close() error = %v", err)
	}

	store = testStore(t, path, config)
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
	if !restored.Contains("inspect-filter") {
		t.Fatal("recovered set lost saved item")
	}
	if _, err := restored.Add("replace-filter"); err != nil {
		t.Fatalf("restored Add() error = %v", err)
	}
	if !restored.Contains("replace-filter") {
		t.Fatal("restored set cannot continue with its persisted HLC")
	}
	checkpoint.Outbox[0] = 'X'
	again, found, err := store.Load("maintenance")
	if err != nil || !found || string(again.Outbox) != string(wantOutbox) {
		t.Fatalf("Load() after caller mutation = %+v, found=%t, err=%v", again, found, err)
	}
}

// TestThreeReplicaCheckpointRecoverySimulation models a mobile replica that
// persists while partitioned, restarts with the same replica ID, and later
// converges with the replicas that continued accepting updates.
func TestThreeReplicaCheckpointRecoverySimulation(t *testing.T) {
	codec := testStringCodec{}
	left := testORSet(t, "left")
	right := testORSet(t, "right")
	mobile := testORSet(t, "mobile")
	shared, err := left.Add("shared")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []*set.ORSet[string]{right, mobile} {
		if err := target.ApplyDelta(shared); err != nil {
			t.Fatal(err)
		}
	}
	offline, err := mobile.Add("offline")
	if err != nil {
		t.Fatal(err)
	}
	offlineBytes, err := offline.MarshalBinary(codec)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := mobile.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/mobile.db"
	store := testStore(t, path, testConfig())
	if err := store.Save("tasks", Checkpoint{Snapshot: saved, Cursor: 3, Outbox: offlineBytes}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	remote, err := right.Add("remote")
	if err != nil {
		t.Fatal(err)
	}
	if err := left.ApplyDelta(remote); err != nil {
		t.Fatal(err)
	}
	store = testStore(t, path, testConfig())
	defer func() { _ = store.Close() }()
	checkpoint, found, err := store.Load("tasks")
	if err != nil || !found || string(checkpoint.Outbox) != string(offlineBytes) {
		t.Fatalf("Load() found=%t checkpoint=%+v err=%v", found, checkpoint, err)
	}
	mobile, err = set.NewORSetFromSnapshot(checkpoint.Snapshot, codec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mobile.Add("after-restart"); err != nil {
		t.Fatalf("mobile mutation after restore: %v", err)
	}
	if err := mobile.ApplyDelta(remote); err != nil {
		t.Fatal(err)
	}
	for _, target := range []*set.ORSet[string]{left, right, mobile} {
		for _, source := range []*set.ORSet[string]{left, right, mobile} {
			if err := target.Merge(source); err != nil {
				t.Fatal(err)
			}
		}
		for _, item := range []string{"shared", "offline", "remote", "after-restart"} {
			if !target.Contains(item) {
				t.Fatalf("replica lost %q after restart recovery", item)
			}
		}
	}
}

func TestStoreRejectsMissingHLCStateAndUnsafeInputs(t *testing.T) {
	store := testStore(t, t.TempDir()+"/checkpoint.db", testConfig())
	defer func() { _ = store.Close() }()
	source := testORSet(t, "maintenance")
	if _, err := source.Add("inspect-filter"); err != nil {
		t.Fatal(err)
	}
	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	missingClock, err := snapshot.New(state, source.Frontier())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("maintenance", Checkpoint{Snapshot: missingClock}); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Save() without HLC state error = %v, want %v", err, ErrInvalidCheckpoint)
	}
	valid, err := source.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("invalid/name", Checkpoint{Snapshot: valid}); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Save() unsafe name error = %v, want %v", err, ErrInvalidCheckpoint)
	}
	if err := store.Save("maintenance", Checkpoint{Snapshot: valid, Outbox: make([]byte, 129)}); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Save() oversized outbox error = %v, want %v", err, ErrInvalidCheckpoint)
	}
}

func TestStoreFailsClosedForDamagedOrSemanticallyInvalidRecords(t *testing.T) {
	path := t.TempDir() + "/checkpoint.db"
	config := testConfig()
	store := testStore(t, path, config)
	saved := testSnapshot(t)
	if err := store.Save("maintenance", Checkpoint{Snapshot: saved}); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(transaction *bolt.Tx) error {
		bucket := transaction.Bucket(checkpointBucket)
		if bucket == nil {
			return ErrCorruptStore
		}
		return bucket.Put([]byte("maintenance"), []byte("not-a-checkpoint"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load("maintenance"); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("Load() damaged record error = %v, want %v", err, ErrCorruptStore)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = testStore(t, path, config)
	if err := store.Save("maintenance", Checkpoint{Snapshot: saved}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	rejecting := config
	rejecting.Validate = func([]byte) error { return errors.New("schema no longer accepted") }
	store = testStore(t, path, rejecting)
	defer func() { _ = store.Close() }()
	if _, _, err := store.Load("maintenance"); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("Load() rejected schema error = %v, want %v", err, ErrCorruptStore)
	}
	if err := store.Save("maintenance", Checkpoint{Snapshot: saved}); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Save() rejecting validator error = %v, want %v", err, ErrInvalidCheckpoint)
	}
}

func TestStoreConcurrentSavesAndLoads(t *testing.T) {
	store := testStore(t, t.TempDir()+"/checkpoint.db", testConfig())
	defer func() { _ = store.Close() }()
	saved := testSnapshot(t)
	const workers = 12
	const writesPerWorker = 24
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

func TestBoltStoreDeleteIsAtomicAndIdempotent(t *testing.T) {
	path := t.TempDir() + "/checkpoint.db"
	store := testStore(t, path, testConfig())
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
	if _, found, err := store.Load("retired"); err != nil || found {
		t.Fatalf("Load(retired) after Delete found=%t err=%v", found, err)
	}
	if checkpoint, found, err := store.Load("active"); err != nil || !found || checkpoint.Snapshot.TypeID != saved.TypeID {
		t.Fatalf("Load(active) checkpoint=%+v found=%t err=%v", checkpoint, found, err)
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

	store = testStore(t, path, testConfig())
	defer func() { _ = store.Close() }()
	if _, found, err := store.Load("retired"); err != nil || found {
		t.Fatalf("restart Load(retired) found=%t err=%v", found, err)
	}
	if _, found, err := store.Load("active"); err != nil || !found {
		t.Fatalf("restart Load(active) found=%t err=%v", found, err)
	}
}

func TestStoresSerializeConcurrentSaveLoadAndDelete(t *testing.T) {
	for name, open := range map[string]func(*testing.T) Store{
		"bbolt": func(t *testing.T) Store {
			return testStore(t, t.TempDir()+"/checkpoint.db", testConfig())
		},
		"file": func(t *testing.T) Store {
			return testFileStore(t, t.TempDir()+"/checkpoint.store", testFileConfig())
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			defer func() { _ = store.Close() }()
			saved := testSnapshot(t)
			const workers = 12
			const operationsPerWorker = 20
			errorsSeen := make(chan error, workers)
			var workersGroup sync.WaitGroup
			for worker := 0; worker < workers; worker++ {
				workersGroup.Add(1)
				go func(worker int) {
					defer workersGroup.Done()
					for operation := 0; operation < operationsPerWorker; operation++ {
						switch (worker + operation) % 3 {
						case 0:
							if err := store.Save("mobile", Checkpoint{Snapshot: saved, Cursor: uint64(operation)}); err != nil {
								errorsSeen <- fmt.Errorf("save: %w", err)
								return
							}
						case 1:
							if _, _, err := store.Load("mobile"); err != nil {
								errorsSeen <- fmt.Errorf("load: %w", err)
								return
							}
						default:
							if _, err := store.Delete("mobile"); err != nil {
								errorsSeen <- fmt.Errorf("delete: %w", err)
								return
							}
						}
					}
				}(worker)
			}
			workersGroup.Wait()
			close(errorsSeen)
			for err := range errorsSeen {
				t.Error(err)
			}
			if err := store.Save("mobile", Checkpoint{Snapshot: saved, Cursor: 99}); err != nil {
				t.Fatal(err)
			}
			if checkpoint, found, err := store.Load("mobile"); err != nil || !found || checkpoint.Cursor != 99 {
				t.Fatalf("final Load() checkpoint=%+v found=%t err=%v", checkpoint, found, err)
			}
		})
	}
}

func TestStoreConfigurationAndClosedBoundaries(t *testing.T) {
	if _, err := Open("", testConfig()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Open() empty path error = %v, want %v", err, ErrInvalidConfig)
	}
	config := testConfig()
	config.Validate = nil
	if _, err := Open(t.TempDir()+"/checkpoint.db", config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Open() missing validator error = %v, want %v", err, ErrInvalidConfig)
	}
	store := testStore(t, t.TempDir()+"/checkpoint.db", testConfig())
	if _, found, err := store.Load("unknown"); err != nil || found {
		t.Fatalf("Load() missing = found=%t err=%v", found, err)
	}
	if _, _, err := store.Load("invalid/name"); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("Load() invalid name error = %v, want %v", err, ErrInvalidCheckpoint)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close() error = %v, want %v", err, ErrClosed)
	}
	if err := store.Save("maintenance", Checkpoint{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Save() after Close error = %v, want %v", err, ErrClosed)
	}
	var nilStore *BoltStore
	if err := nilStore.Save("maintenance", Checkpoint{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Save() error = %v, want %v", err, ErrClosed)
	}
	if _, _, err := nilStore.Load("maintenance"); !errors.Is(err, ErrClosed) {
		t.Fatalf("nil Load() error = %v, want %v", err, ErrClosed)
	}
}

func TestStoreOpenAndBucketCorruptionBoundaries(t *testing.T) {
	root := t.TempDir()
	if _, err := Open(root+"/missing/checkpoint.db", testConfig()); err == nil {
		t.Fatal("Open() accepted missing parent directory")
	}
	parentFile := root + "/parent-file"
	if err := os.WriteFile(parentFile, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(parentFile+"/checkpoint.db", testConfig()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Open() file parent error = %v, want %v", err, ErrInvalidConfig)
	}
	if _, err := Open(root, testConfig()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Open() directory path error = %v, want %v", err, ErrInvalidConfig)
	}
	config := testConfig()
	config.OpenTimeout = -1
	if _, err := Open(root+"/checkpoint.db", config); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Open() negative timeout error = %v, want %v", err, ErrInvalidConfig)
	}
	store := testStore(t, root+"/checkpoint.db", testConfig())
	defer func() { _ = store.Close() }()
	if err := store.db.Update(func(transaction *bolt.Tx) error { return transaction.DeleteBucket(checkpointBucket) }); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("maintenance", Checkpoint{Snapshot: testSnapshot(t)}); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("Save() missing bucket error = %v, want %v", err, ErrCorruptStore)
	}
	if _, _, err := store.Load("maintenance"); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("Load() missing bucket error = %v, want %v", err, ErrCorruptStore)
	}
	if store.config.validName("") || store.config.validName("name/with-slash") || store.config.validName(string(make([]byte, store.config.MaxNameBytes+1))) {
		t.Fatal("invalid checkpoint name accepted")
	}
}

func testConfig() Config {
	return Config{
		MaxRecordBytes:     8 << 10,
		MaxStateBytes:      4 << 10,
		MaxFrontierEntries: 32,
		MaxReplicaIDBytes:  128,
		MaxOutboxBytes:     128,
		MaxNameBytes:       64,
		Validate:           validateTestORSet,
	}
}

func testStore(t *testing.T, path string, config Config) *BoltStore {
	t.Helper()
	store, err := Open(path, config)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return store
}

func testORSet(t *testing.T, replicaID string) *set.ORSet[string] {
	t.Helper()
	value, err := set.NewORSet(replicaID, testStringCodec{})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func testSnapshot(t *testing.T) snapshot.Snapshot {
	t.Helper()
	value := testORSet(t, "maintenance")
	if _, err := value.Add("inspect-filter"); err != nil {
		t.Fatal(err)
	}
	saved, err := value.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func validateTestORSet(data []byte) error {
	value, err := set.NewORSet("validation", testStringCodec{})
	if err != nil {
		return err
	}
	return value.UnmarshalBinary(data)
}
