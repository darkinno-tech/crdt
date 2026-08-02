// Package sqlrelay contains the standard-library SQL implementation shared by
// durable SQL providers. Database drivers remain application dependencies.
package sqlrelay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/durable"
	"github.com/DarkInno/crdt/replica"
)

var ErrSequenceRange = errors.New("crdt SQL durable provider: sequence exceeds signed bigint range")

// Config bounds one retained operation log per group. Timeout applies to each
// Append and Replay call because durable.Log intentionally has no context
// parameter; connection and pool lifetime remain application-owned.
type Config struct {
	MaxEvents uint64
	MaxBytes  uint64
	Timeout   time.Duration
}

// Dialect supplies fixed, audited SQL for one database family. All statements
// must use database/sql placeholders and must preserve the durable.Log
// transaction contract documented by the owning provider package.
type Dialect struct {
	Name             string
	MaxGroupIDBytes  int
	MaxActorIDBytes  int
	MaxEnvelopeBytes uint64
	Schema           []string
	AppendOptions    sql.TxOptions
	ReplayOptions    sql.TxOptions
	InsertGroup      string
	LockGroup        string
	ReadDot          string
	InsertEvent      string
	InsertDot        string
	UpdateGroup      string
	ReadHighWater    string
	ReadEvents       string
}

type pool interface {
	BeginTx(context.Context, *sql.TxOptions) (transaction, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type transaction interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) row
	QueryContext(context.Context, string, ...any) (rows, error)
	Commit() error
	Rollback() error
}

type row interface {
	Scan(...any) error
}

type rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}

type sqlPool struct{ database *sql.DB }

func (pool sqlPool) BeginTx(ctx context.Context, options *sql.TxOptions) (transaction, error) {
	transaction, err := pool.database.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return sqlTransaction{transaction}, nil
}

func (pool sqlPool) ExecContext(ctx context.Context, query string, arguments ...any) (sql.Result, error) {
	return pool.database.ExecContext(ctx, query, arguments...)
}

type sqlTransaction struct{ transaction *sql.Tx }

func (transaction sqlTransaction) ExecContext(ctx context.Context, query string, arguments ...any) (sql.Result, error) {
	return transaction.transaction.ExecContext(ctx, query, arguments...)
}

func (transaction sqlTransaction) QueryRowContext(ctx context.Context, query string, arguments ...any) row {
	return sqlRow{transaction.transaction.QueryRowContext(ctx, query, arguments...)}
}

func (transaction sqlTransaction) QueryContext(ctx context.Context, query string, arguments ...any) (rows, error) {
	result, err := transaction.transaction.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	return sqlRows{result}, nil
}

func (transaction sqlTransaction) Commit() error   { return transaction.transaction.Commit() }
func (transaction sqlTransaction) Rollback() error { return transaction.transaction.Rollback() }

type sqlRow struct{ row *sql.Row }

func (row sqlRow) Scan(destinations ...any) error { return row.row.Scan(destinations...) }

type sqlRows struct{ rows *sql.Rows }

func (rows sqlRows) Next() bool                     { return rows.rows.Next() }
func (rows sqlRows) Scan(destinations ...any) error { return rows.rows.Scan(destinations...) }
func (rows sqlRows) Err() error                     { return rows.rows.Err() }
func (rows sqlRows) Close() error                   { return rows.rows.Close() }

// Store implements durable.Log with a provider-specific transaction dialect.
// It does not close the supplied *sql.DB, which may be shared by handlers.
type Store struct {
	pool      pool
	dialect   Dialect
	maxEvents uint64
	maxBytes  uint64
	timeout   time.Duration
	closed    atomic.Bool
}

// New validates provider limits around an application-owned database/sql pool.
// It does not import or register a database driver.
func New(database *sql.DB, config Config, dialect Dialect) (*Store, error) {
	if database == nil {
		return nil, durable.ErrInvalidConfig
	}
	return newWithPool(sqlPool{database: database}, config, dialect)
}

func newWithPool(database pool, config Config, dialect Dialect) (*Store, error) {
	if database == nil || config.MaxEvents == 0 || config.MaxBytes == 0 || config.MaxEvents > math.MaxInt64 || config.MaxBytes > math.MaxInt64 || config.Timeout < 0 || !validDialect(dialect) {
		return nil, durable.ErrInvalidConfig
	}
	if config.Timeout == 0 {
		config.Timeout = 5 * time.Second
	}
	return &Store{pool: database, dialect: cloneDialect(dialect), maxEvents: config.MaxEvents, maxBytes: config.MaxBytes, timeout: config.Timeout}, nil
}

func validDialect(dialect Dialect) bool {
	if strings.TrimSpace(dialect.Name) == "" || dialect.MaxGroupIDBytes <= 0 || dialect.MaxActorIDBytes <= 0 || len(dialect.Schema) == 0 {
		return false
	}
	for _, statement := range append([]string(nil), dialect.Schema...) {
		if strings.TrimSpace(statement) == "" {
			return false
		}
	}
	for _, statement := range []string{dialect.InsertGroup, dialect.LockGroup, dialect.ReadDot, dialect.InsertEvent, dialect.InsertDot, dialect.UpdateGroup, dialect.ReadHighWater, dialect.ReadEvents} {
		if strings.TrimSpace(statement) == "" {
			return false
		}
	}
	return true
}

func cloneDialect(dialect Dialect) Dialect {
	dialect.Schema = append([]string(nil), dialect.Schema...)
	return dialect
}

// EnsureSchema creates fixed provider tables. It is intentionally explicit so
// applications can use a migration role instead of granting runtime DDL.
func (store *Store) EnsureSchema(ctx context.Context) error {
	if store == nil || store.pool == nil || store.Closed() {
		return durable.ErrClosed
	}
	for _, statement := range store.dialect.Schema {
		if _, err := store.pool.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create %s durable provider schema: %w", store.dialect.Name, err)
		}
	}
	return nil
}

// Close marks Store unavailable. It deliberately does not close the pool.
func (store *Store) Close() error {
	if store == nil || store.pool == nil || !store.closed.CompareAndSwap(false, true) {
		return durable.ErrClosed
	}
	return nil
}

// Closed implements durable.Log.
func (store *Store) Closed() bool {
	return store == nil || store.pool == nil || store.closed.Load()
}

// Append atomically binds a dot to its canonical envelope, reserves the next
// group-local sequence, and updates capacity metadata.
func (store *Store) Append(groupID string, change replica.Change) (durable.AppendResult, error) {
	if store.Closed() {
		return durable.AppendResult{}, durable.ErrClosed
	}
	if !store.validGroupID(groupID) || !store.validActor(change.Dot.Actor) || change.Dot.Counter > math.MaxInt64 {
		return durable.AppendResult{}, durable.ErrInvalidConfig
	}
	encoded, err := durable.EncodeChange(change)
	if err != nil {
		return durable.AppendResult{}, err
	}
	if (store.dialect.MaxEnvelopeBytes != 0 && uint64(len(encoded)) > store.dialect.MaxEnvelopeBytes) || uint64(len(encoded)) > store.maxBytes {
		return durable.AppendResult{}, durable.ErrStoreFull
	}
	ctx, cancel := context.WithTimeout(context.Background(), store.timeout)
	defer cancel()
	transaction, err := store.pool.BeginTx(ctx, &store.dialect.AppendOptions)
	if err != nil {
		return durable.AppendResult{}, fmt.Errorf("begin %s durable append: %w", store.dialect.Name, err)
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, store.dialect.InsertGroup, groupID); err != nil {
		return durable.AppendResult{}, fmt.Errorf("create %s durable group: %w", store.dialect.Name, err)
	}
	var highWater, count, usedBytes int64
	if err := transaction.QueryRowContext(ctx, store.dialect.LockGroup, groupID).Scan(&highWater, &count, &usedBytes); err != nil {
		return durable.AppendResult{}, fmt.Errorf("lock %s durable group: %w", store.dialect.Name, err)
	}
	digest := sha256.Sum256(encoded)
	var sequence int64
	var existingDigest []byte
	err = transaction.QueryRowContext(ctx, store.dialect.ReadDot, groupID, change.Dot.Actor, int64(change.Dot.Counter)).Scan(&sequence, &existingDigest)
	if err == nil {
		if sequence <= 0 || len(existingDigest) != sha256.Size {
			return durable.AppendResult{}, durable.ErrCorruptStore
		}
		if !bytes.Equal(existingDigest, digest[:]) {
			return durable.AppendResult{}, durable.ErrConflictingDot
		}
		if err := transaction.Commit(); err != nil {
			return durable.AppendResult{}, fmt.Errorf("commit duplicate %s durable append: %w", store.dialect.Name, err)
		}
		return durable.AppendResult{Event: durable.Event{Sequence: uint64(sequence), Change: change}, Duplicate: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return durable.AppendResult{}, fmt.Errorf("read %s durable dot binding: %w", store.dialect.Name, err)
	}
	if highWater < 0 || count < 0 || usedBytes < 0 || highWater != count {
		return durable.AppendResult{}, durable.ErrCorruptStore
	}
	if highWater == math.MaxInt64 {
		return durable.AppendResult{}, ErrSequenceRange
	}
	if uint64(count) >= store.maxEvents || uint64(usedBytes) > store.maxBytes-uint64(len(encoded)) {
		return durable.AppendResult{}, durable.ErrStoreFull
	}
	sequence = highWater + 1
	if _, err := transaction.ExecContext(ctx, store.dialect.InsertEvent, groupID, sequence, encoded); err != nil {
		return durable.AppendResult{}, fmt.Errorf("insert %s durable event: %w", store.dialect.Name, err)
	}
	if _, err := transaction.ExecContext(ctx, store.dialect.InsertDot, groupID, change.Dot.Actor, int64(change.Dot.Counter), sequence, digest[:]); err != nil {
		return durable.AppendResult{}, fmt.Errorf("insert %s durable dot binding: %w", store.dialect.Name, err)
	}
	if _, err := transaction.ExecContext(ctx, store.dialect.UpdateGroup, sequence, count+1, usedBytes+int64(len(encoded)), groupID); err != nil {
		return durable.AppendResult{}, fmt.Errorf("advance %s durable group: %w", store.dialect.Name, err)
	}
	if err := transaction.Commit(); err != nil {
		return durable.AppendResult{}, fmt.Errorf("commit %s durable append: %w", store.dialect.Name, err)
	}
	return durable.AppendResult{Event: durable.Event{Sequence: uint64(sequence), Change: change}}, nil
}

// Replay reads one provider snapshot and refuses a partial, corrupt,
// over-budget, or manifest-incompatible suffix.
func (store *Store) Replay(groupID string, after, maxEvents, maxBytes uint64, manifest replica.Manifest, policy crdt.ProtocolPolicy, maxMessageBytes, maxActorBytes int) (events []durable.Event, highWater uint64, resultErr error) {
	if store.Closed() {
		return nil, 0, durable.ErrClosed
	}
	if !store.validGroupID(groupID) || maxEvents == 0 || maxBytes == 0 || maxEvents > math.MaxInt64 || after > math.MaxInt64 || maxMessageBytes <= 0 || maxActorBytes <= 0 {
		return nil, 0, durable.ErrInvalidConfig
	}
	ctx, cancel := context.WithTimeout(context.Background(), store.timeout)
	defer cancel()
	transaction, err := store.pool.BeginTx(ctx, &store.dialect.ReplayOptions)
	if err != nil {
		return nil, 0, fmt.Errorf("begin %s durable replay: %w", store.dialect.Name, err)
	}
	defer func() { _ = transaction.Rollback() }()
	var storedHighWater int64
	err = transaction.QueryRowContext(ctx, store.dialect.ReadHighWater, groupID).Scan(&storedHighWater)
	if errors.Is(err, sql.ErrNoRows) {
		if after != 0 {
			return nil, 0, durable.ErrReplayUnavailable
		}
		if err := transaction.Commit(); err != nil {
			return nil, 0, fmt.Errorf("commit empty %s durable replay: %w", store.dialect.Name, err)
		}
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read %s durable high water: %w", store.dialect.Name, err)
	}
	if storedHighWater < 0 {
		return nil, 0, durable.ErrCorruptStore
	}
	if after > uint64(storedHighWater) || uint64(storedHighWater)-after > maxEvents {
		return nil, 0, durable.ErrReplayUnavailable
	}
	rows, err := transaction.QueryContext(ctx, store.dialect.ReadEvents, groupID, int64(after))
	if err != nil {
		return nil, 0, fmt.Errorf("read %s durable replay: %w", store.dialect.Name, err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && resultErr == nil {
			events = nil
			highWater = 0
			resultErr = durable.ErrCorruptStore
		}
	}()
	events = make([]durable.Event, 0)
	var usedBytes uint64
	for expected := after + 1; rows.Next(); expected++ {
		var sequence int64
		var encoded []byte
		if err := rows.Scan(&sequence, &encoded); err != nil || sequence <= 0 || uint64(sequence) != expected || uint64(len(encoded)) > maxBytes-usedBytes || len(encoded) > maxMessageBytes {
			return nil, 0, durable.ErrCorruptStore
		}
		dot, delta, err := durable.DecodeChange(encoded, maxMessageBytes, maxActorBytes)
		if err != nil {
			return nil, 0, durable.ErrCorruptStore
		}
		change, err := replica.NewChangeWithPolicy(manifest, dot, delta, policy)
		if err != nil {
			return nil, 0, durable.ErrCorruptStore
		}
		events = append(events, durable.Event{Sequence: uint64(sequence), Change: change})
		usedBytes += uint64(len(encoded))
	}
	if err := rows.Err(); err != nil || uint64(len(events)) != uint64(storedHighWater)-after {
		return nil, 0, durable.ErrCorruptStore
	}
	if err := transaction.Commit(); err != nil {
		return nil, 0, fmt.Errorf("commit %s durable replay: %w", store.dialect.Name, err)
	}
	return events, uint64(storedHighWater), nil
}

func (store *Store) validGroupID(groupID string) bool {
	return strings.TrimSpace(groupID) != "" && len(groupID) <= store.dialect.MaxGroupIDBytes
}

func (store *Store) validActor(actor string) bool {
	return strings.TrimSpace(actor) != "" && len(actor) <= store.dialect.MaxActorIDBytes
}

var _ durable.Log = (*Store)(nil)
