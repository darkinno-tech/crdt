package sqlrelay

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/counter"
	"github.com/DarkInno/crdt/durable"
	"github.com/DarkInno/crdt/replica"
)

func TestStoreAppendsRetriesConflictsAndReplays(t *testing.T) {
	dialect := testDialect()
	manifest := testManifest(t)
	change := testChange(t, manifest, "alice", 1, 2)
	encoded, err := durable.EncodeChange(change)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	wrongDigest := append([]byte(nil), digest[:]...)
	wrongDigest[0] ^= 1
	pool := &scriptPool{transactions: []*scriptTransaction{
		appendScript(dialect, manifest.GroupID, change, encoded),
		duplicateScript(dialect, manifest.GroupID, change, 1, digest[:], true),
		duplicateScript(dialect, manifest.GroupID, change, 1, wrongDigest, false),
		replayScript(dialect, manifest.GroupID, 0, 1, [][]any{{int64(1), encoded}}),
	}}
	store := testStore(t, pool, dialect, 8, 1<<20)
	result, err := store.Append(manifest.GroupID, change)
	if err != nil || result.Duplicate || result.Event.Sequence != 1 {
		t.Fatalf("append = %+v, %v", result, err)
	}
	result, err = store.Append(manifest.GroupID, change)
	if err != nil || !result.Duplicate || result.Event.Sequence != 1 {
		t.Fatalf("duplicate append = %+v, %v", result, err)
	}
	if _, err := store.Append(manifest.GroupID, change); !errors.Is(err, durable.ErrConflictingDot) {
		t.Fatalf("conflicting append = %v", err)
	}
	events, highWater, err := store.Replay(manifest.GroupID, 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
	if err != nil || highWater != 1 || len(events) != 1 || events[0].Change.Dot != change.Dot {
		t.Fatalf("replay = high=%d events=%+v err=%v", highWater, events, err)
	}
	pool.verify(t, []sql.TxOptions{dialect.AppendOptions, dialect.AppendOptions, dialect.AppendOptions, dialect.ReplayOptions})
}

func TestStoreRejectsBoundsAndClosedUse(t *testing.T) {
	dialect := testDialect()
	if _, err := New(nil, Config{MaxEvents: 1, MaxBytes: 1}, dialect); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("nil database = %v", err)
	}
	pool := &scriptPool{}
	for _, config := range []Config{{}, {MaxEvents: uint64(math.MaxInt64) + 1, MaxBytes: 1}, {MaxEvents: 1, MaxBytes: uint64(math.MaxInt64) + 1}, {MaxEvents: 1, MaxBytes: 1, Timeout: -1}} {
		if _, err := newWithPool(pool, config, dialect); !errors.Is(err, durable.ErrInvalidConfig) {
			t.Fatalf("newWithPool(%+v) = %v", config, err)
		}
	}
	store := testStore(t, pool, dialect, 1, 1)
	manifest := testManifest(t)
	if _, err := store.Append(manifest.GroupID, testChange(t, manifest, "small", 1, 1)); !errors.Is(err, durable.ErrStoreFull) {
		t.Fatalf("oversized envelope = %v", err)
	}
	if _, err := store.Append(" ", testChange(t, manifest, "alice", 1, 1)); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("invalid group = %v", err)
	}
	if _, err := store.Append(strings.Repeat("g", dialect.MaxGroupIDBytes+1), testChange(t, manifest, "alice", 1, 1)); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("oversized group = %v", err)
	}
	if _, err := store.Append(manifest.GroupID, testChange(t, manifest, strings.Repeat("a", dialect.MaxActorIDBytes+1), 1, 1)); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("oversized actor = %v", err)
	}
	if _, err := store.Append(manifest.GroupID, testChange(t, manifest, "alice", uint64(math.MaxInt64)+1, 1)); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("oversized counter = %v", err)
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

func TestStoreAppliesProviderIdentifierValidators(t *testing.T) {
	dialect := testDialect()
	dialect.MaxGroupIDBytes = 64
	dialect.MaxActorIDBytes = 64
	var groups, actors int
	dialect.ValidateGroupID = func(value string) bool {
		groups++
		return value == "valid-group"
	}
	dialect.ValidateActorID = func(value string) bool {
		actors++
		return value == "valid-actor"
	}
	store := testStore(t, &scriptPool{}, dialect, 8, 1<<20)
	manifest := testManifest(t)
	if _, err := store.Append("invalid-group", testChange(t, manifest, "valid-actor", 1, 1)); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("invalid provider group = %v", err)
	}
	if _, err := store.Append("valid-group", testChange(t, manifest, "invalid-actor", 1, 1)); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("invalid provider actor = %v", err)
	}
	if _, _, err := store.Replay("invalid-group", 0, 1, 1, manifest, crdt.ProtocolPolicy{}, 1, 1); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("invalid provider replay group = %v", err)
	}
	if groups != 3 || actors != 1 {
		t.Fatalf("validator calls: groups=%d actors=%d", groups, actors)
	}
}

func TestStoreFailsClosedForCorruptionAndUnavailableReplay(t *testing.T) {
	dialect := testDialect()
	manifest := testManifest(t)
	change := testChange(t, manifest, "alice", 1, 1)
	pool := &scriptPool{transactions: []*scriptTransaction{
		replayScript(dialect, manifest.GroupID, 0, 2, [][]any{{int64(2), []byte("bad")}}),
		emptyReplayScript(dialect, "missing"),
		replayScript(dialect, manifest.GroupID, 2, 1, nil),
		corruptMetadataScript(dialect, manifest.GroupID, change),
		duplicateScript(dialect, manifest.GroupID, change, 1, []byte("short"), false),
	}}
	store := testStore(t, pool, dialect, 8, 1<<20)
	if _, _, err := store.Replay(manifest.GroupID, 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, durable.ErrCorruptStore) {
		t.Fatalf("incomplete replay = %v", err)
	}
	if events, high, err := store.Replay("missing", 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); err != nil || high != 0 || len(events) != 0 {
		t.Fatalf("empty replay = high=%d events=%d err=%v", high, len(events), err)
	}
	if _, _, err := store.Replay(manifest.GroupID, 2, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, durable.ErrReplayUnavailable) {
		t.Fatalf("future replay = %v", err)
	}
	if _, err := store.Append(manifest.GroupID, change); !errors.Is(err, durable.ErrCorruptStore) {
		t.Fatalf("inconsistent metadata append = %v", err)
	}
	if _, err := store.Append(manifest.GroupID, change); !errors.Is(err, durable.ErrCorruptStore) {
		t.Fatalf("malformed dot binding append = %v", err)
	}
	pool.verify(t, []sql.TxOptions{dialect.ReplayOptions, dialect.ReplayOptions, dialect.ReplayOptions, dialect.AppendOptions, dialect.AppendOptions})
}

func TestStoreSchemaAndDatabaseFailures(t *testing.T) {
	dialect := testDialect()
	pool := &scriptPool{schema: []scriptStep{{kind: stepExec, query: "schema groups"}, {kind: stepExec, query: "schema events"}, {kind: stepExec, query: "schema dots"}}}
	store := testStore(t, pool, dialect, 8, 1<<20)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	manifest := testManifest(t)
	change := testChange(t, manifest, "alice", 1, 1)
	pool.beginErr = errors.New("down")
	if _, err := store.Append(manifest.GroupID, change); err == nil {
		t.Fatal("append begin failure accepted")
	}
	pool.beginErr = nil
	pool.transactions = append(pool.transactions,
		&scriptTransaction{steps: []scriptStep{
			{kind: stepExec, query: dialect.InsertGroup, args: []any{manifest.GroupID}},
			{kind: stepRow, query: dialect.LockGroup, args: []any{manifest.GroupID}, values: []any{int64(0), int64(0), int64(0)}},
			{kind: stepRow, query: dialect.ReadDot, args: []any{manifest.GroupID, change.Dot.Actor, int64(change.Dot.Counter)}, err: sql.ErrNoRows},
			{kind: stepExec, query: dialect.InsertEvent, args: []any{manifest.GroupID, int64(1), mustEncode(t, change)}, err: errors.New("down")},
		}, expectRollback: true},
		&scriptTransaction{steps: []scriptStep{{kind: stepRow, query: dialect.ReadHighWater, args: []any{manifest.GroupID}, err: errors.New("down")}}, expectRollback: true},
	)
	if _, err := store.Append(manifest.GroupID, change); err == nil {
		t.Fatal("event insert failure accepted")
	}
	if _, _, err := store.Replay(manifest.GroupID, 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); err == nil {
		t.Fatal("replay high-water failure accepted")
	}
	pool.verify(t, []sql.TxOptions{dialect.AppendOptions, dialect.AppendOptions, dialect.ReplayOptions})
}

func TestStoreRejectsInvalidDialectAndSchemaFailure(t *testing.T) {
	valid := testDialect()
	invalidDialects := []Dialect{
		{},
		func() Dialect {
			dialect := valid
			dialect.Name = " "
			return dialect
		}(),
		func() Dialect {
			dialect := valid
			dialect.Schema = []string{" "}
			return dialect
		}(),
		func() Dialect {
			dialect := valid
			dialect.ReadEvents = " "
			return dialect
		}(),
	}
	for _, dialect := range invalidDialects {
		if _, err := newWithPool(&scriptPool{}, Config{MaxEvents: 1, MaxBytes: 1}, dialect); !errors.Is(err, durable.ErrInvalidConfig) {
			t.Fatalf("invalid dialect %+v = %v", dialect, err)
		}
	}
	pool := &scriptPool{schema: []scriptStep{{kind: stepExec, query: valid.Schema[0], err: errors.New("ddl unavailable")}}}
	store := testStore(t, pool, valid, 8, 1<<20)
	if err := store.EnsureSchema(context.Background()); err == nil {
		t.Fatal("schema failure accepted")
	}
	pool.verify(t, nil)
}

func TestStoreAppendFailurePaths(t *testing.T) {
	dialect := testDialect()
	manifest := testManifest(t)
	change := testChange(t, manifest, "alice", 1, 1)
	encoded := mustEncode(t, change)
	appendFailure := func(query string) *scriptTransaction {
		transaction := appendScript(dialect, manifest.GroupID, change, encoded)
		for index := range transaction.steps {
			if transaction.steps[index].query == query {
				transaction.steps[index].err = errors.New("database unavailable")
				transaction.steps = transaction.steps[:index+1]
				transaction.expectCommit = false
				transaction.expectRollback = true
				return transaction
			}
		}
		t.Fatalf("missing append statement %q", query)
		return nil
	}
	cases := []struct {
		name        string
		transaction *scriptTransaction
		maxEvents   uint64
		want        error
	}{
		{name: "insert group", transaction: appendFailure(dialect.InsertGroup)},
		{name: "insert event", transaction: appendFailure(dialect.InsertEvent)},
		{name: "insert dot", transaction: appendFailure(dialect.InsertDot)},
		{name: "update group", transaction: appendFailure(dialect.UpdateGroup)},
		{name: "lock group", transaction: &scriptTransaction{steps: []scriptStep{
			{kind: stepExec, query: dialect.InsertGroup, args: []any{manifest.GroupID}},
			{kind: stepRow, query: dialect.LockGroup, args: []any{manifest.GroupID}, err: errors.New("lock unavailable")},
		}, expectRollback: true}},
		{name: "dot read", transaction: &scriptTransaction{steps: []scriptStep{
			{kind: stepExec, query: dialect.InsertGroup, args: []any{manifest.GroupID}},
			{kind: stepRow, query: dialect.LockGroup, args: []any{manifest.GroupID}, values: []any{int64(0), int64(0), int64(0)}},
			{kind: stepRow, query: dialect.ReadDot, args: []any{manifest.GroupID, change.Dot.Actor, int64(change.Dot.Counter)}, err: errors.New("read unavailable")},
		}, expectRollback: true}},
		{name: "capacity", transaction: &scriptTransaction{steps: []scriptStep{
			{kind: stepExec, query: dialect.InsertGroup, args: []any{manifest.GroupID}},
			{kind: stepRow, query: dialect.LockGroup, args: []any{manifest.GroupID}, values: []any{int64(1), int64(1), int64(0)}},
			{kind: stepRow, query: dialect.ReadDot, args: []any{manifest.GroupID, change.Dot.Actor, int64(change.Dot.Counter)}, err: sql.ErrNoRows},
		}, expectRollback: true}, maxEvents: 1, want: durable.ErrStoreFull},
		{name: "sequence range", transaction: &scriptTransaction{steps: []scriptStep{
			{kind: stepExec, query: dialect.InsertGroup, args: []any{manifest.GroupID}},
			{kind: stepRow, query: dialect.LockGroup, args: []any{manifest.GroupID}, values: []any{int64(math.MaxInt64), int64(math.MaxInt64), int64(0)}},
			{kind: stepRow, query: dialect.ReadDot, args: []any{manifest.GroupID, change.Dot.Actor, int64(change.Dot.Counter)}, err: sql.ErrNoRows},
		}, expectRollback: true}, maxEvents: math.MaxInt64, want: ErrSequenceRange},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			maxEvents := test.maxEvents
			if maxEvents == 0 {
				maxEvents = 8
			}
			pool := &scriptPool{transactions: []*scriptTransaction{test.transaction}}
			store := testStore(t, pool, dialect, maxEvents, 1<<20)
			_, err := store.Append(manifest.GroupID, change)
			if test.want != nil {
				if !errors.Is(err, test.want) {
					t.Fatalf("append error = %v, want %v", err, test.want)
				}
			} else if err == nil {
				t.Fatal("append failure accepted")
			}
			pool.verify(t, []sql.TxOptions{dialect.AppendOptions})
		})
	}
	transaction := appendScript(dialect, manifest.GroupID, change, encoded)
	transaction.commitErr = errors.New("commit unavailable")
	pool := &scriptPool{transactions: []*scriptTransaction{transaction}}
	store := testStore(t, pool, dialect, 8, 1<<20)
	if _, err := store.Append(manifest.GroupID, change); err == nil {
		t.Fatal("append commit failure accepted")
	}
	pool.verify(t, []sql.TxOptions{dialect.AppendOptions})
}

func TestStoreReplayFailurePaths(t *testing.T) {
	dialect := testDialect()
	manifest := testManifest(t)
	change := testChange(t, manifest, "alice", 1, 1)
	encoded := mustEncode(t, change)
	cases := []struct {
		name        string
		after       uint64
		transaction *scriptTransaction
		want        error
	}{
		{name: "missing history", after: 1, transaction: &scriptTransaction{steps: []scriptStep{{kind: stepRow, query: dialect.ReadHighWater, args: []any{manifest.GroupID}, err: sql.ErrNoRows}}, expectRollback: true}, want: durable.ErrReplayUnavailable},
		{name: "negative high water", transaction: &scriptTransaction{steps: []scriptStep{{kind: stepRow, query: dialect.ReadHighWater, args: []any{manifest.GroupID}, values: []any{int64(-1)}}}, expectRollback: true}, want: durable.ErrCorruptStore},
		{name: "event query", transaction: &scriptTransaction{steps: []scriptStep{
			{kind: stepRow, query: dialect.ReadHighWater, args: []any{manifest.GroupID}, values: []any{int64(1)}},
			{kind: stepRows, query: dialect.ReadEvents, args: []any{manifest.GroupID, int64(0)}, err: errors.New("query unavailable")},
		}, expectRollback: true}, want: nil},
		{name: "rows error", transaction: &scriptTransaction{steps: []scriptStep{
			{kind: stepRow, query: dialect.ReadHighWater, args: []any{manifest.GroupID}, values: []any{int64(1)}},
			{kind: stepRows, query: dialect.ReadEvents, args: []any{manifest.GroupID, int64(0)}, rows: [][]any{{int64(1), encoded}}, rowsErr: errors.New("rows unavailable")},
		}, expectRollback: true}, want: durable.ErrCorruptStore},
		{name: "rows close", transaction: &scriptTransaction{steps: []scriptStep{
			{kind: stepRow, query: dialect.ReadHighWater, args: []any{manifest.GroupID}, values: []any{int64(1)}},
			{kind: stepRows, query: dialect.ReadEvents, args: []any{manifest.GroupID, int64(0)}, rows: [][]any{{int64(1), encoded}}, closeErr: errors.New("close unavailable")},
		}, expectCommit: true}, want: durable.ErrCorruptStore},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			pool := &scriptPool{transactions: []*scriptTransaction{test.transaction}}
			store := testStore(t, pool, dialect, 8, 1<<20)
			_, _, err := store.Replay(manifest.GroupID, test.after, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
			if test.want != nil {
				if !errors.Is(err, test.want) {
					t.Fatalf("replay error = %v, want %v", err, test.want)
				}
			} else if err == nil {
				t.Fatal("replay failure accepted")
			}
			pool.verify(t, []sql.TxOptions{dialect.ReplayOptions})
		})
	}
	transaction := replayScript(dialect, manifest.GroupID, 0, 1, [][]any{{int64(1), encoded}})
	transaction.commitErr = errors.New("commit unavailable")
	pool := &scriptPool{transactions: []*scriptTransaction{transaction}}
	store := testStore(t, pool, dialect, 8, 1<<20)
	if _, _, err := store.Replay(manifest.GroupID, 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); err == nil {
		t.Fatal("replay commit failure accepted")
	}
	pool.verify(t, []sql.TxOptions{dialect.ReplayOptions})
	beginPool := &scriptPool{beginErr: errors.New("begin unavailable")}
	store = testStore(t, beginPool, dialect, 8, 1<<20)
	if _, _, err := store.Replay(manifest.GroupID, 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); err == nil {
		t.Fatal("replay begin failure accepted")
	}
	beginPool.verify(t, []sql.TxOptions{dialect.ReplayOptions})
}

func TestSQLAdaptersUseDatabaseSQL(t *testing.T) {
	database, err := sql.Open(sqlAdapterDriverName, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	pool := sqlPool{database: database}
	if _, err := pool.ExecContext(ctx, "schema"); err != nil {
		t.Fatal(err)
	}
	transaction, err := pool.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.ExecContext(ctx, "insert"); err != nil {
		t.Fatal(err)
	}
	var first string
	if err := transaction.QueryRowContext(ctx, "row").Scan(&first); err != nil {
		t.Fatal(err)
	}
	if first != "value" {
		t.Fatalf("row value = %q", first)
	}
	resultRows, err := transaction.QueryContext(ctx, "rows")
	if err != nil {
		t.Fatal(err)
	}
	if !resultRows.Next() {
		t.Fatal("expected result row")
	}
	var second string
	if err := resultRows.Scan(&second); err != nil {
		t.Fatal(err)
	}
	if second != "value" {
		t.Fatalf("rows value = %q", second)
	}
	if resultRows.Next() {
		t.Fatal("unexpected second result row")
	}
	if err := resultRows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := resultRows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	transaction, err = pool.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatal(err)
	}
	store, err := New(database, Config{MaxEvents: 8, MaxBytes: 1 << 20}, testDialect())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkStoreAppendSimulated(b *testing.B) {
	dialect := testDialect()
	manifest := testManifest(b)
	change := testChange(b, manifest, "alice", 1, 1)
	encoded := mustEncode(b, change)
	pool := &scriptPool{
		transactions: make([]*scriptTransaction, b.N),
		options:      make([]sql.TxOptions, 0, b.N),
	}
	for index := range pool.transactions {
		pool.transactions[index] = appendScript(dialect, manifest.GroupID, change, encoded)
	}
	store := testStore(b, pool, dialect, uint64(b.N)+1, 1<<20)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := store.Append(manifest.GroupID, change); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if pool.failure != nil || len(pool.transactions) != 0 {
		b.Fatalf("simulated append state: failure=%v transactions=%d", pool.failure, len(pool.transactions))
	}
}

func testDialect() Dialect {
	return Dialect{
		Name:             "test SQL",
		MaxGroupIDBytes:  16,
		MaxActorIDBytes:  16,
		MaxEnvelopeBytes: 1024,
		Schema:           []string{"schema groups", "schema events", "schema dots"},
		AppendOptions:    sql.TxOptions{Isolation: sql.LevelReadCommitted},
		ReplayOptions:    sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true},
		InsertGroup:      "insert group",
		LockGroup:        "lock group",
		ReadDot:          "read dot",
		InsertEvent:      "insert event",
		InsertDot:        "insert dot",
		UpdateGroup:      "update group",
		ReadHighWater:    "read high water",
		ReadEvents:       "read events",
	}
}

func appendScript(dialect Dialect, groupID string, change replica.Change, encoded []byte) *scriptTransaction {
	digest := sha256.Sum256(encoded)
	return &scriptTransaction{steps: []scriptStep{
		{kind: stepExec, query: dialect.InsertGroup, args: []any{groupID}},
		{kind: stepRow, query: dialect.LockGroup, args: []any{groupID}, values: []any{int64(0), int64(0), int64(0)}},
		{kind: stepRow, query: dialect.ReadDot, args: []any{groupID, change.Dot.Actor, int64(change.Dot.Counter)}, err: sql.ErrNoRows},
		{kind: stepExec, query: dialect.InsertEvent, args: []any{groupID, int64(1), encoded}},
		{kind: stepExec, query: dialect.InsertDot, args: []any{groupID, change.Dot.Actor, int64(change.Dot.Counter), int64(1), digest[:]}},
		{kind: stepExec, query: dialect.UpdateGroup, args: []any{int64(1), int64(1), int64(len(encoded)), groupID}},
	}, expectCommit: true}
}

func duplicateScript(dialect Dialect, groupID string, change replica.Change, sequence int64, digest []byte, commit bool) *scriptTransaction {
	return &scriptTransaction{steps: []scriptStep{
		{kind: stepExec, query: dialect.InsertGroup, args: []any{groupID}},
		{kind: stepRow, query: dialect.LockGroup, args: []any{groupID}, values: []any{int64(1), int64(1), int64(1)}},
		{kind: stepRow, query: dialect.ReadDot, args: []any{groupID, change.Dot.Actor, int64(change.Dot.Counter)}, values: []any{sequence, digest}},
	}, expectCommit: commit, expectRollback: !commit}
}

func replayScript(dialect Dialect, groupID string, after, highWater int64, resultRows [][]any) *scriptTransaction {
	steps := []scriptStep{{kind: stepRow, query: dialect.ReadHighWater, args: []any{groupID}, values: []any{highWater}}}
	if after <= highWater {
		steps = append(steps, scriptStep{kind: stepRows, query: dialect.ReadEvents, args: []any{groupID, after}, rows: resultRows})
	}
	return &scriptTransaction{steps: steps, expectCommit: after <= highWater && int64(len(resultRows)) == highWater-after, expectRollback: after > highWater || int64(len(resultRows)) != highWater-after}
}

func emptyReplayScript(dialect Dialect, groupID string) *scriptTransaction {
	return &scriptTransaction{steps: []scriptStep{{kind: stepRow, query: dialect.ReadHighWater, args: []any{groupID}, err: sql.ErrNoRows}}, expectCommit: true}
}

func corruptMetadataScript(dialect Dialect, groupID string, change replica.Change) *scriptTransaction {
	return &scriptTransaction{steps: []scriptStep{
		{kind: stepExec, query: dialect.InsertGroup, args: []any{groupID}},
		{kind: stepRow, query: dialect.LockGroup, args: []any{groupID}, values: []any{int64(2), int64(1), int64(0)}},
		{kind: stepRow, query: dialect.ReadDot, args: []any{groupID, change.Dot.Actor, int64(change.Dot.Counter)}, err: sql.ErrNoRows},
	}, expectRollback: true}
}

func testStore(t testing.TB, pool pool, dialect Dialect, maxEvents, maxBytes uint64) *Store {
	t.Helper()
	store, err := newWithPool(pool, Config{MaxEvents: maxEvents, MaxBytes: maxBytes}, dialect)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testManifest(t testing.TB) replica.Manifest {
	t.Helper()
	manifest, err := replica.NewManifest("sql-counter", "example.com/sql-counter/v1", 1, replica.Protocol{StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func testChange(t testing.TB, manifest replica.Manifest, actor string, sequence, amount uint64) replica.Change {
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

func mustEncode(t testing.TB, change replica.Change) []byte {
	t.Helper()
	encoded, err := durable.EncodeChange(change)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type stepKind uint8

const (
	stepExec stepKind = iota + 1
	stepRow
	stepRows
)

type scriptStep struct {
	kind     stepKind
	query    string
	args     []any
	values   []any
	rows     [][]any
	err      error
	rowsErr  error
	closeErr error
}

type scriptPool struct {
	transactions []*scriptTransaction
	schema       []scriptStep
	options      []sql.TxOptions
	beginErr     error
	failure      error
}

func (pool *scriptPool) BeginTx(_ context.Context, options *sql.TxOptions) (transaction, error) {
	if options == nil {
		pool.fail(errors.New("missing transaction options"))
		return nil, pool.failure
	}
	pool.options = append(pool.options, *options)
	if pool.beginErr != nil {
		return nil, pool.beginErr
	}
	if len(pool.transactions) == 0 {
		pool.fail(errors.New("unexpected transaction"))
		return nil, pool.failure
	}
	transaction := pool.transactions[0]
	pool.transactions = pool.transactions[1:]
	transaction.pool = pool
	return transaction, nil
}

func (pool *scriptPool) ExecContext(_ context.Context, query string, arguments ...any) (sql.Result, error) {
	if len(pool.schema) == 0 {
		pool.fail(fmt.Errorf("unexpected schema statement %q", query))
		return testResult{}, pool.failure
	}
	step := pool.schema[0]
	pool.schema = pool.schema[1:]
	if err := pool.match(stepExec, step, query, arguments); err != nil {
		return testResult{}, err
	}
	return testResult{}, step.err
}

func (pool *scriptPool) fail(err error) {
	if pool.failure == nil {
		pool.failure = err
	}
}

func (pool *scriptPool) match(kind stepKind, step scriptStep, query string, arguments []any) error {
	if step.kind != kind || step.query != query || !reflect.DeepEqual(step.args, arguments) {
		err := fmt.Errorf("unexpected SQL kind=%d query=%q args=%#v, want kind=%d query=%q args=%#v", kind, query, arguments, step.kind, step.query, step.args)
		pool.fail(err)
		return err
	}
	return nil
}

func (pool *scriptPool) verify(t testing.TB, options []sql.TxOptions) {
	t.Helper()
	if pool.failure != nil {
		t.Fatal(pool.failure)
	}
	if len(pool.schema) != 0 || len(pool.transactions) != 0 {
		t.Fatalf("unconsumed scripts: schema=%d transactions=%d", len(pool.schema), len(pool.transactions))
	}
	if !reflect.DeepEqual(pool.options, options) {
		t.Fatalf("transaction options = %#v, want %#v", pool.options, options)
	}
}

type scriptTransaction struct {
	pool           *scriptPool
	steps          []scriptStep
	expectCommit   bool
	expectRollback bool
	finished       bool
	commitErr      error
}

func (transaction *scriptTransaction) ExecContext(_ context.Context, query string, arguments ...any) (sql.Result, error) {
	step, err := transaction.pop(stepExec, query, arguments)
	return testResult{}, firstError(err, step.err)
}

func (transaction *scriptTransaction) QueryRowContext(_ context.Context, query string, arguments ...any) row {
	step, err := transaction.pop(stepRow, query, arguments)
	return scriptRow{step: step, err: err}
}

func (transaction *scriptTransaction) QueryContext(_ context.Context, query string, arguments ...any) (rows, error) {
	step, err := transaction.pop(stepRows, query, arguments)
	if err != nil || step.err != nil {
		return nil, firstError(err, step.err)
	}
	return &scriptRows{rows: step.rows, err: step.rowsErr, closeErr: step.closeErr}, nil
}

func (transaction *scriptTransaction) Commit() error {
	if transaction.finished {
		return sql.ErrTxDone
	}
	transaction.finished = true
	if !transaction.expectCommit || len(transaction.steps) != 0 {
		transaction.pool.fail(errors.New("unexpected transaction commit"))
		return transaction.pool.failure
	}
	return transaction.commitErr
}

func (transaction *scriptTransaction) Rollback() error {
	if transaction.finished {
		return sql.ErrTxDone
	}
	transaction.finished = true
	if !transaction.expectRollback || len(transaction.steps) != 0 {
		transaction.pool.fail(errors.New("unexpected transaction rollback"))
		return transaction.pool.failure
	}
	return nil
}

func (transaction *scriptTransaction) pop(kind stepKind, query string, arguments []any) (scriptStep, error) {
	if len(transaction.steps) == 0 {
		err := fmt.Errorf("unexpected SQL kind=%d query=%q", kind, query)
		transaction.pool.fail(err)
		return scriptStep{}, err
	}
	step := transaction.steps[0]
	transaction.steps = transaction.steps[1:]
	if err := transaction.pool.match(kind, step, query, arguments); err != nil {
		return scriptStep{}, err
	}
	return step, nil
}

type scriptRow struct {
	step scriptStep
	err  error
}

func (row scriptRow) Scan(destinations ...any) error {
	if row.err != nil || row.step.err != nil {
		return firstError(row.err, row.step.err)
	}
	return scan(destinations, row.step.values)
}

type scriptRows struct {
	rows     [][]any
	index    int
	err      error
	closeErr error
}

func (rows *scriptRows) Next() bool {
	if rows.index >= len(rows.rows) {
		return false
	}
	rows.index++
	return true
}

func (rows *scriptRows) Scan(destinations ...any) error {
	if rows.index == 0 || rows.index > len(rows.rows) {
		return errors.New("scan without a current row")
	}
	return scan(destinations, rows.rows[rows.index-1])
}

func (rows *scriptRows) Err() error   { return rows.err }
func (rows *scriptRows) Close() error { return rows.closeErr }

func scan(destinations, values []any) error {
	if len(destinations) != len(values) {
		return errors.New("scan width mismatch")
	}
	for index, destination := range destinations {
		switch destination := destination.(type) {
		case *int64:
			value, ok := values[index].(int64)
			if !ok {
				return fmt.Errorf("cannot scan %T into int64", values[index])
			}
			*destination = value
		case *[]byte:
			value, ok := values[index].([]byte)
			if !ok {
				return fmt.Errorf("cannot scan %T into bytes", values[index])
			}
			*destination = append((*destination)[:0], value...)
		default:
			return fmt.Errorf("unsupported scan destination %T", destination)
		}
	}
	return nil
}

func firstError(first, second error) error {
	if first != nil {
		return first
	}
	return second
}

type testResult struct{}

func (testResult) LastInsertId() (int64, error) { return 0, nil }
func (testResult) RowsAffected() (int64, error) { return 1, nil }

const sqlAdapterDriverName = "darkinno-crdt-sqlrelay-test"

func init() {
	sql.Register(sqlAdapterDriverName, sqlAdapterDriver{})
}

type sqlAdapterDriver struct{}

func (sqlAdapterDriver) Open(string) (driver.Conn, error) { return sqlAdapterConnection{}, nil }

type sqlAdapterConnection struct{}

func (sqlAdapterConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements are not used by the adapter test")
}

func (sqlAdapterConnection) Close() error { return nil }

func (sqlAdapterConnection) Begin() (driver.Tx, error) { return sqlAdapterTransaction{}, nil }

func (sqlAdapterConnection) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return sqlAdapterTransaction{}, nil
}

func (sqlAdapterConnection) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return testResult{}, nil
}

func (sqlAdapterConnection) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &sqlAdapterRows{}, nil
}

type sqlAdapterTransaction struct{}

func (sqlAdapterTransaction) Commit() error   { return nil }
func (sqlAdapterTransaction) Rollback() error { return nil }

type sqlAdapterRows struct{ read bool }

func (rows *sqlAdapterRows) Columns() []string { return []string{"value"} }
func (rows *sqlAdapterRows) Close() error      { return nil }

func (rows *sqlAdapterRows) Next(destinations []driver.Value) error {
	if rows.read {
		return io.EOF
	}
	rows.read = true
	destinations[0] = "value"
	return nil
}
