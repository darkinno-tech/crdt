package persistence

import "testing"

func BenchmarkStoreSave(b *testing.B) {
	store := benchmarkStore(b)
	defer func() { _ = store.Close() }()
	checkpoint := benchmarkCheckpoint(b)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := store.Save("maintenance", checkpoint); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStoreLoad(b *testing.B) {
	store := benchmarkStore(b)
	defer func() { _ = store.Close() }()
	checkpoint := benchmarkCheckpoint(b)
	if err := store.Save("maintenance", checkpoint); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, found, err := store.Load("maintenance"); err != nil || !found {
			b.Fatalf("Load() found=%t err=%v", found, err)
		}
	}
}

func BenchmarkStoreSaveParallel(b *testing.B) {
	store := benchmarkStore(b)
	defer func() { _ = store.Close() }()
	checkpoint := benchmarkCheckpoint(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(parallel *testing.PB) {
		for parallel.Next() {
			if err := store.Save("maintenance", checkpoint); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

// BenchmarkStoreLoadLegacyMigration measures the one-time read-plus-rewrite
// path. Fixture insertion is excluded so the result captures the migration
// transaction rather than test setup.
func BenchmarkStoreLoadLegacyMigration(b *testing.B) {
	config := testConfig()
	config.Format.MigrateOnLoad = true
	store, err := Open(b.TempDir()+"/checkpoint.db", config)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	legacy := marshalLegacyCheckpoint(b, benchmarkCheckpoint(b))
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		b.StopTimer()
		putRawCheckpoint(b, store, "maintenance", legacy)
		b.StartTimer()
		if _, found, err := store.Load("maintenance"); err != nil || !found {
			b.Fatalf("Load() found=%t err=%v", found, err)
		}
	}
}

func benchmarkStore(b *testing.B) *Store {
	b.Helper()
	store, err := Open(b.TempDir()+"/checkpoint.db", testConfig())
	if err != nil {
		b.Fatal(err)
	}
	return store
}

func benchmarkCheckpoint(b *testing.B) Checkpoint {
	b.Helper()
	value, err := setForBenchmark()
	if err != nil {
		b.Fatal(err)
	}
	saved, err := value.SnapshotCurrentState()
	if err != nil {
		b.Fatal(err)
	}
	return Checkpoint{Snapshot: saved, Cursor: 41, Outbox: []byte("canonical-pending-change")}
}
