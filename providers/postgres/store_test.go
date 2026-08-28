package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"testing"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/counter"
	"github.com/darkinno-tech/crdt/durable"
	"github.com/darkinno-tech/crdt/replica"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v3"
)

func TestStoreAppendsRetriesConflictsAndReplays(t *testing.T) {
	mock := testPool(t)
	store := testStore(t, mock, 8, 1<<20)
	manifest := testManifest(t)
	change := testChange(t, manifest, "alice", 1, 2)
	encoded, err := durable.EncodeChange(change)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)

	expectAppendStart(mock, manifest.GroupID, change, encoded, 0, 0, 0)
	mock.ExpectCommit()
	result, err := store.Append(manifest.GroupID, change)
	if err != nil || result.Duplicate || result.Event.Sequence != 1 {
		t.Fatalf("append = %+v, %v", result, err)
	}

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectExec("INSERT INTO crdt_durable_groups").WithArgs(manifest.GroupID).WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT high_water, event_count, used_bytes").WithArgs(manifest.GroupID).WillReturnRows(mock.NewRows([]string{"high_water", "event_count", "used_bytes"}).AddRow(int64(1), int64(1), int64(len(encoded))))
	mock.ExpectQuery("SELECT sequence, digest").WithArgs(manifest.GroupID, change.Dot.Actor, int64(change.Dot.Counter)).WillReturnRows(mock.NewRows([]string{"sequence", "digest"}).AddRow(int64(1), digest[:]))
	mock.ExpectCommit()
	result, err = store.Append(manifest.GroupID, change)
	if err != nil || !result.Duplicate || result.Event.Sequence != 1 {
		t.Fatalf("duplicate append = %+v, %v", result, err)
	}

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectExec("INSERT INTO crdt_durable_groups").WithArgs(manifest.GroupID).WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery("SELECT high_water, event_count, used_bytes").WithArgs(manifest.GroupID).WillReturnRows(mock.NewRows([]string{"high_water", "event_count", "used_bytes"}).AddRow(int64(1), int64(1), int64(len(encoded))))
	mock.ExpectQuery("SELECT sequence, digest").WithArgs(manifest.GroupID, change.Dot.Actor, int64(change.Dot.Counter)).WillReturnRows(mock.NewRows([]string{"sequence", "digest"}).AddRow(int64(1), []byte("wrong-digest")))
	if _, err := store.Append(manifest.GroupID, change); !errors.Is(err, durable.ErrConflictingDot) {
		t.Fatalf("conflicting append = %v", err)
	}

	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	mock.ExpectQuery("SELECT high_water FROM crdt_durable_groups").WithArgs(manifest.GroupID).WillReturnRows(mock.NewRows([]string{"high_water"}).AddRow(int64(1)))
	mock.ExpectQuery("SELECT sequence, envelope FROM crdt_durable_events").WithArgs(manifest.GroupID, int64(0)).WillReturnRows(mock.NewRows([]string{"sequence", "envelope"}).AddRow(int64(1), encoded))
	mock.ExpectCommit()
	events, highWater, err := store.Replay(manifest.GroupID, 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
	if err != nil || highWater != 1 || len(events) != 1 || events[0].Change.Dot != change.Dot {
		t.Fatalf("replay = high=%d events=%+v err=%v", highWater, events, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRejectsBoundsAndClosedUse(t *testing.T) {
	var nilPool Pool
	if _, err := New(nilPool, Config{MaxEvents: 1, MaxBytes: 1}); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("nil pool = %v", err)
	}
	mock := testPool(t)
	for _, config := range []Config{{}, {MaxEvents: uint64(math.MaxInt64) + 1, MaxBytes: 1}, {MaxEvents: 1, MaxBytes: uint64(math.MaxInt64) + 1}, {MaxEvents: 1, MaxBytes: 1, Timeout: -1}} {
		if _, err := New(mock, config); !errors.Is(err, durable.ErrInvalidConfig) {
			t.Fatalf("New(%+v) = %v", config, err)
		}
	}
	store := testStore(t, mock, 1, 1)
	manifest := testManifest(t)
	if _, err := store.Append(manifest.GroupID, testChange(t, manifest, "small", 1, 1)); !errors.Is(err, durable.ErrStoreFull) {
		t.Fatalf("oversized envelope = %v", err)
	}
	if _, err := store.Append(" ", testChange(t, manifest, "alice", 1, 1)); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("invalid group = %v", err)
	}
	if _, err := store.Append(manifest.GroupID, replica.Change{}); err == nil {
		t.Fatal("invalid change accepted")
	}
	if _, _, err := store.Replay(manifest.GroupID, 0, 0, 1, manifest, crdt.ProtocolPolicy{}, 1, 1); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("invalid replay = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); !errors.Is(err, durable.ErrClosed) {
		t.Fatalf("second close = %v", err)
	}
	if _, err := store.Append(manifest.GroupID, testChange(t, manifest, "alice", 1, 1)); !errors.Is(err, durable.ErrClosed) {
		t.Fatalf("closed append = %v", err)
	}
	if _, _, err := store.Replay(manifest.GroupID, 0, 1, 1, manifest, crdt.ProtocolPolicy{}, 1, 1); !errors.Is(err, durable.ErrClosed) {
		t.Fatalf("closed replay = %v", err)
	}
}

func TestStoreFailsClosedForIncompleteAndInvalidReplay(t *testing.T) {
	mock := testPool(t)
	store := testStore(t, mock, 8, 1<<20)
	manifest := testManifest(t)
	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	mock.ExpectQuery("SELECT high_water FROM crdt_durable_groups").WithArgs(manifest.GroupID).WillReturnRows(mock.NewRows([]string{"high_water"}).AddRow(int64(2)))
	mock.ExpectQuery("SELECT sequence, envelope FROM crdt_durable_events").WithArgs(manifest.GroupID, int64(0)).WillReturnRows(mock.NewRows([]string{"sequence", "envelope"}).AddRow(int64(2), []byte("bad")))
	if _, _, err := store.Replay(manifest.GroupID, 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, durable.ErrCorruptStore) {
		t.Fatalf("incomplete replay = %v", err)
	}
	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	mock.ExpectQuery("SELECT high_water FROM crdt_durable_groups").WithArgs("missing").WillReturnError(pgx.ErrNoRows)
	mock.ExpectCommit()
	if events, high, err := store.Replay("missing", 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); err != nil || high != 0 || len(events) != 0 {
		t.Fatalf("empty replay = high=%d events=%d err=%v", high, len(events), err)
	}
	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	mock.ExpectQuery("SELECT high_water FROM crdt_durable_groups").WithArgs(manifest.GroupID).WillReturnRows(mock.NewRows([]string{"high_water"}).AddRow(int64(1)))
	if _, _, err := store.Replay(manifest.GroupID, 2, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, durable.ErrReplayUnavailable) {
		t.Fatalf("future replay = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureSchema(t *testing.T) {
	mock := testPool(t)
	store := testStore(t, mock, 1, 1)
	mock.ExpectExec("CREATE TABLE IF NOT EXISTS crdt_durable_groups").WillReturnResult(pgxmock.NewResult("CREATE", 0))
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreDatabaseFailurePaths(t *testing.T) {
	manifest := testManifest(t)
	change := testChange(t, manifest, "alice", 1, 1)
	encoded, err := durable.EncodeChange(change)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("append begin", func(t *testing.T) {
		mock := testPool(t)
		mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted}).WillReturnError(errors.New("down"))
		if _, err := testStore(t, mock, 8, 1<<20).Append(manifest.GroupID, change); err == nil {
			t.Fatal("begin failure accepted")
		}
	})
	t.Run("append group insert", func(t *testing.T) {
		mock := testPool(t)
		mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		mock.ExpectExec("INSERT INTO crdt_durable_groups").WithArgs(manifest.GroupID).WillReturnError(errors.New("down"))
		if _, err := testStore(t, mock, 8, 1<<20).Append(manifest.GroupID, change); err == nil {
			t.Fatal("group failure accepted")
		}
	})
	t.Run("append store full", func(t *testing.T) {
		mock := testPool(t)
		mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		mock.ExpectExec("INSERT INTO crdt_durable_groups").WithArgs(manifest.GroupID).WillReturnResult(pgxmock.NewResult("INSERT", 0))
		mock.ExpectQuery("SELECT high_water, event_count, used_bytes").WithArgs(manifest.GroupID).WillReturnRows(mock.NewRows([]string{"high_water", "event_count", "used_bytes"}).AddRow(int64(1), int64(8), int64(len(encoded))))
		mock.ExpectQuery("SELECT sequence, digest").WithArgs(manifest.GroupID, change.Dot.Actor, int64(change.Dot.Counter)).WillReturnError(pgx.ErrNoRows)
		if _, err := testStore(t, mock, 8, 1<<20).Append(manifest.GroupID, change); !errors.Is(err, durable.ErrStoreFull) {
			t.Fatalf("full = %v", err)
		}
	})
	t.Run("append sequence range", func(t *testing.T) {
		mock := testPool(t)
		mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		mock.ExpectExec("INSERT INTO crdt_durable_groups").WithArgs(manifest.GroupID).WillReturnResult(pgxmock.NewResult("INSERT", 0))
		mock.ExpectQuery("SELECT high_water, event_count, used_bytes").WithArgs(manifest.GroupID).WillReturnRows(mock.NewRows([]string{"high_water", "event_count", "used_bytes"}).AddRow(int64(math.MaxInt64), int64(0), int64(0)))
		mock.ExpectQuery("SELECT sequence, digest").WithArgs(manifest.GroupID, change.Dot.Actor, int64(change.Dot.Counter)).WillReturnError(pgx.ErrNoRows)
		if _, err := testStore(t, mock, 8, 1<<20).Append(manifest.GroupID, change); !errors.Is(err, ErrSequenceRange) {
			t.Fatalf("range = %v", err)
		}
	})
	t.Run("append event insert", func(t *testing.T) {
		mock := testPool(t)
		mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		mock.ExpectExec("INSERT INTO crdt_durable_groups").WithArgs(manifest.GroupID).WillReturnResult(pgxmock.NewResult("INSERT", 1))
		mock.ExpectQuery("SELECT high_water, event_count, used_bytes").WithArgs(manifest.GroupID).WillReturnRows(mock.NewRows([]string{"high_water", "event_count", "used_bytes"}).AddRow(int64(0), int64(0), int64(0)))
		mock.ExpectQuery("SELECT sequence, digest").WithArgs(manifest.GroupID, change.Dot.Actor, int64(change.Dot.Counter)).WillReturnError(pgx.ErrNoRows)
		mock.ExpectExec("INSERT INTO crdt_durable_events").WithArgs(manifest.GroupID, int64(1), encoded).WillReturnError(errors.New("down"))
		if _, err := testStore(t, mock, 8, 1<<20).Append(manifest.GroupID, change); err == nil {
			t.Fatal("event insert failure accepted")
		}
	})
	t.Run("append dot lookup and insert", func(t *testing.T) {
		mock := testPool(t)
		mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		mock.ExpectExec("INSERT INTO crdt_durable_groups").WithArgs(manifest.GroupID).WillReturnResult(pgxmock.NewResult("INSERT", 1))
		mock.ExpectQuery("SELECT high_water, event_count, used_bytes").WithArgs(manifest.GroupID).WillReturnRows(mock.NewRows([]string{"high_water", "event_count", "used_bytes"}).AddRow(int64(0), int64(0), int64(0)))
		mock.ExpectQuery("SELECT sequence, digest").WithArgs(manifest.GroupID, change.Dot.Actor, int64(change.Dot.Counter)).WillReturnError(errors.New("down"))
		if _, err := testStore(t, mock, 8, 1<<20).Append(manifest.GroupID, change); err == nil {
			t.Fatal("dot lookup failure accepted")
		}
		mock = testPool(t)
		mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		mock.ExpectExec("INSERT INTO crdt_durable_groups").WithArgs(manifest.GroupID).WillReturnResult(pgxmock.NewResult("INSERT", 1))
		mock.ExpectQuery("SELECT high_water, event_count, used_bytes").WithArgs(manifest.GroupID).WillReturnRows(mock.NewRows([]string{"high_water", "event_count", "used_bytes"}).AddRow(int64(0), int64(0), int64(0)))
		mock.ExpectQuery("SELECT sequence, digest").WithArgs(manifest.GroupID, change.Dot.Actor, int64(change.Dot.Counter)).WillReturnError(pgx.ErrNoRows)
		mock.ExpectExec("INSERT INTO crdt_durable_events").WithArgs(manifest.GroupID, int64(1), encoded).WillReturnResult(pgxmock.NewResult("INSERT", 1))
		mock.ExpectExec("INSERT INTO crdt_durable_dots").WithArgs(manifest.GroupID, change.Dot.Actor, int64(change.Dot.Counter), int64(1), pgxmock.AnyArg()).WillReturnError(errors.New("down"))
		if _, err := testStore(t, mock, 8, 1<<20).Append(manifest.GroupID, change); err == nil {
			t.Fatal("dot insert failure accepted")
		}
	})
	t.Run("replay begin and query", func(t *testing.T) {
		mock := testPool(t)
		mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}).WillReturnError(errors.New("down"))
		if _, _, err := testStore(t, mock, 8, 1<<20).Replay(manifest.GroupID, 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); err == nil {
			t.Fatal("replay begin failure accepted")
		}
		mock = testPool(t)
		mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		mock.ExpectQuery("SELECT high_water FROM crdt_durable_groups").WithArgs(manifest.GroupID).WillReturnError(errors.New("down"))
		if _, _, err := testStore(t, mock, 8, 1<<20).Replay(manifest.GroupID, 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, durable.ErrCorruptStore) {
			t.Fatalf("replay query = %v", err)
		}
	})
	t.Run("replay caught up and events query", func(t *testing.T) {
		mock := testPool(t)
		mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		mock.ExpectQuery("SELECT high_water FROM crdt_durable_groups").WithArgs(manifest.GroupID).WillReturnRows(mock.NewRows([]string{"high_water"}).AddRow(int64(1)))
		mock.ExpectQuery("SELECT sequence, envelope FROM crdt_durable_events").WithArgs(manifest.GroupID, int64(1)).WillReturnRows(mock.NewRows([]string{"sequence", "envelope"}))
		mock.ExpectCommit()
		if events, high, err := testStore(t, mock, 8, 1<<20).Replay(manifest.GroupID, 1, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); err != nil || high != 1 || len(events) != 0 {
			t.Fatalf("caught up replay = high=%d events=%d err=%v", high, len(events), err)
		}
		mock = testPool(t)
		mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		mock.ExpectQuery("SELECT high_water FROM crdt_durable_groups").WithArgs(manifest.GroupID).WillReturnRows(mock.NewRows([]string{"high_water"}).AddRow(int64(1)))
		mock.ExpectQuery("SELECT sequence, envelope FROM crdt_durable_events").WithArgs(manifest.GroupID, int64(0)).WillReturnError(errors.New("down"))
		if _, _, err := testStore(t, mock, 8, 1<<20).Replay(manifest.GroupID, 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); err == nil {
			t.Fatal("events query failure accepted")
		}
	})
	t.Run("schema errors", func(t *testing.T) {
		mock := testPool(t)
		mock.ExpectExec("CREATE TABLE IF NOT EXISTS crdt_durable_groups").WillReturnError(errors.New("denied"))
		store := testStore(t, mock, 1, 1)
		if err := store.EnsureSchema(context.Background()); err == nil {
			t.Fatal("schema error accepted")
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if err := store.EnsureSchema(context.Background()); !errors.Is(err, durable.ErrClosed) {
			t.Fatalf("closed schema = %v", err)
		}
	})
}

func expectAppendStart(mock pgxmock.PgxPoolIface, groupID string, change replica.Change, encoded []byte, highWater, count, usedBytes int64) {
	mock.ExpectBeginTx(pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	mock.ExpectExec("INSERT INTO crdt_durable_groups").WithArgs(groupID).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectQuery("SELECT high_water, event_count, used_bytes").WithArgs(groupID).WillReturnRows(mock.NewRows([]string{"high_water", "event_count", "used_bytes"}).AddRow(highWater, count, usedBytes))
	mock.ExpectQuery("SELECT sequence, digest").WithArgs(groupID, change.Dot.Actor, int64(change.Dot.Counter)).WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec("INSERT INTO crdt_durable_events").WithArgs(groupID, int64(1), encoded).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("INSERT INTO crdt_durable_dots").WithArgs(groupID, change.Dot.Actor, int64(change.Dot.Counter), int64(1), pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec("UPDATE crdt_durable_groups").WithArgs(groupID, int64(1), int64(1), int64(len(encoded))).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
}

func testPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	return mock
}

func testStore(t *testing.T, pool Pool, maxEvents, maxBytes uint64) *Store {
	t.Helper()
	store, err := New(pool, Config{MaxEvents: maxEvents, MaxBytes: maxBytes})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testManifest(t *testing.T) replica.Manifest {
	t.Helper()
	manifest, err := replica.NewManifest("postgres-counter", "example.com/postgres-counter/v1", 1, replica.Protocol{StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func testChange(t *testing.T, manifest replica.Manifest, actor string, sequence, amount uint64) replica.Change {
	t.Helper()
	state, err := counter.NewGCounter(actor)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := state.Increment(amount)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	change, err := replica.NewChange(manifest, replica.Dot{Actor: actor, Counter: sequence}, encoded)
	if err != nil {
		t.Fatal(err)
	}
	return change
}
