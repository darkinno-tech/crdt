// Package postgres provides a PostgreSQL-backed durable relay operation log.
// It is intentionally separate from durable so applications opt into the
// database driver and own pool construction, credentials, TLS, and migrations.
package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/durable"
	"github.com/im10furry/crdt/replica"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrSequenceRange = errors.New("crdt postgres provider: sequence exceeds PostgreSQL bigint range")

// Config bounds one retained operation log per group. Timeout applies to each
// Append and Replay call because durable.Log intentionally has no context
// parameter; connection and pool lifetime remain application-owned.
type Config struct {
	MaxEvents uint64
	MaxBytes  uint64
	Timeout   time.Duration
}

// Pool is the minimum pgx pool surface used by Store. *pgxpool.Pool satisfies
// it directly; keeping the port narrow also permits deterministic SQL-path
// tests without a running database.
type Pool interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// Store implements durable.Log with one row lock per group. It does not close
// the supplied pool, which may be shared by multiple relay handlers.
type Store struct {
	pool      Pool
	maxEvents uint64
	maxBytes  uint64
	timeout   time.Duration
	closed    atomic.Bool
}

// New validates provider limits. Call EnsureSchema with a migration role
// before accepting traffic; runtime applications should grant only the needed
// table privileges to the relay role.
func New(pool Pool, config Config) (*Store, error) {
	if pool == nil || config.MaxEvents == 0 || config.MaxBytes == 0 || config.MaxEvents > math.MaxInt64 || config.MaxBytes > math.MaxInt64 || config.Timeout < 0 {
		return nil, durable.ErrInvalidConfig
	}
	if config.Timeout == 0 {
		config.Timeout = 5 * time.Second
	}
	return &Store{pool: pool, maxEvents: config.MaxEvents, maxBytes: config.MaxBytes, timeout: config.Timeout}, nil
}

// EnsureSchema creates the fixed provider tables. It is intentionally an
// explicit migration step rather than hidden startup DDL.
func (store *Store) EnsureSchema(ctx context.Context) error {
	if store == nil || store.pool == nil || store.Closed() {
		return durable.ErrClosed
	}
	_, err := store.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS crdt_durable_groups (
  group_id TEXT PRIMARY KEY,
  high_water BIGINT NOT NULL DEFAULT 0 CHECK (high_water >= 0),
  event_count BIGINT NOT NULL DEFAULT 0 CHECK (event_count >= 0),
  used_bytes BIGINT NOT NULL DEFAULT 0 CHECK (used_bytes >= 0)
);
CREATE TABLE IF NOT EXISTS crdt_durable_events (
  group_id TEXT NOT NULL REFERENCES crdt_durable_groups(group_id) ON DELETE RESTRICT,
  sequence BIGINT NOT NULL CHECK (sequence > 0),
  envelope BYTEA NOT NULL,
  PRIMARY KEY (group_id, sequence)
);
CREATE TABLE IF NOT EXISTS crdt_durable_dots (
  group_id TEXT NOT NULL REFERENCES crdt_durable_groups(group_id) ON DELETE RESTRICT,
  actor TEXT NOT NULL,
  counter BIGINT NOT NULL CHECK (counter > 0),
  sequence BIGINT NOT NULL CHECK (sequence > 0),
  digest BYTEA NOT NULL CHECK (octet_length(digest) = 32),
  PRIMARY KEY (group_id, actor, counter)
);`)
	if err != nil {
		return fmt.Errorf("create durable provider schema: %w", err)
	}
	return nil
}

// Close marks this Store unavailable. It deliberately does not close pool.
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

// Append atomically verifies a retry's payload digest, reserves a sequence,
// persists its envelope, and updates capacity accounting under a group row
// lock. PostgreSQL BIGINT intentionally limits provider sequences and dots to
// MaxInt64; applications must rotate/bootstrap long before that boundary.
func (store *Store) Append(groupID string, change replica.Change) (durable.AppendResult, error) {
	if store.Closed() {
		return durable.AppendResult{}, durable.ErrClosed
	}
	if strings.TrimSpace(groupID) == "" || change.Dot.Counter > math.MaxInt64 {
		return durable.AppendResult{}, durable.ErrInvalidConfig
	}
	encoded, err := durable.EncodeChange(change)
	if err != nil {
		return durable.AppendResult{}, err
	}
	if uint64(len(encoded)) > store.maxBytes {
		return durable.AppendResult{}, durable.ErrStoreFull
	}
	ctx, cancel := context.WithTimeout(context.Background(), store.timeout)
	defer cancel()
	transaction, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return durable.AppendResult{}, fmt.Errorf("begin durable append: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	if _, err := transaction.Exec(ctx, `INSERT INTO crdt_durable_groups (group_id) VALUES ($1) ON CONFLICT DO NOTHING`, groupID); err != nil {
		return durable.AppendResult{}, fmt.Errorf("create durable group: %w", err)
	}
	var highWater, count, usedBytes int64
	if err := transaction.QueryRow(ctx, `SELECT high_water, event_count, used_bytes FROM crdt_durable_groups WHERE group_id = $1 FOR UPDATE`, groupID).Scan(&highWater, &count, &usedBytes); err != nil {
		return durable.AppendResult{}, fmt.Errorf("lock durable group: %w", err)
	}
	digest := sha256.Sum256(encoded)
	var sequence int64
	var existingDigest []byte
	err = transaction.QueryRow(ctx, `SELECT sequence, digest FROM crdt_durable_dots WHERE group_id = $1 AND actor = $2 AND counter = $3`, groupID, change.Dot.Actor, int64(change.Dot.Counter)).Scan(&sequence, &existingDigest)
	if err == nil {
		if !bytes.Equal(existingDigest, digest[:]) {
			return durable.AppendResult{}, durable.ErrConflictingDot
		}
		if err := transaction.Commit(ctx); err != nil {
			return durable.AppendResult{}, fmt.Errorf("commit duplicate durable append: %w", err)
		}
		return durable.AppendResult{Event: durable.Event{Sequence: uint64(sequence), Change: change}, Duplicate: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return durable.AppendResult{}, fmt.Errorf("read durable dot binding: %w", err)
	}
	if highWater < 0 || count < 0 || usedBytes < 0 || highWater < count || uint64(count) >= store.maxEvents || uint64(usedBytes) > store.maxBytes-uint64(len(encoded)) {
		return durable.AppendResult{}, durable.ErrStoreFull
	}
	if highWater == math.MaxInt64 {
		return durable.AppendResult{}, ErrSequenceRange
	}
	sequence = highWater + 1
	if _, err := transaction.Exec(ctx, `INSERT INTO crdt_durable_events (group_id, sequence, envelope) VALUES ($1, $2, $3)`, groupID, sequence, encoded); err != nil {
		return durable.AppendResult{}, fmt.Errorf("insert durable event: %w", err)
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO crdt_durable_dots (group_id, actor, counter, sequence, digest) VALUES ($1, $2, $3, $4, $5)`, groupID, change.Dot.Actor, int64(change.Dot.Counter), sequence, digest[:]); err != nil {
		return durable.AppendResult{}, fmt.Errorf("insert durable dot binding: %w", err)
	}
	if _, err := transaction.Exec(ctx, `UPDATE crdt_durable_groups SET high_water = $2, event_count = $3, used_bytes = $4 WHERE group_id = $1`, groupID, sequence, count+1, usedBytes+int64(len(encoded))); err != nil {
		return durable.AppendResult{}, fmt.Errorf("advance durable group: %w", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return durable.AppendResult{}, fmt.Errorf("commit durable append: %w", err)
	}
	return durable.AppendResult{Event: durable.Event{Sequence: uint64(sequence), Change: change}}, nil
}

// Replay reads one repeatable PostgreSQL snapshot and refuses a partial,
// corrupt, over-budget, or manifest-incompatible suffix.
func (store *Store) Replay(groupID string, after, maxEvents, maxBytes uint64, manifest replica.Manifest, policy crdt.ProtocolPolicy, maxMessageBytes, maxActorBytes int) ([]durable.Event, uint64, error) {
	if store.Closed() {
		return nil, 0, durable.ErrClosed
	}
	if strings.TrimSpace(groupID) == "" || maxEvents == 0 || maxBytes == 0 || maxEvents > math.MaxInt64 || after > math.MaxInt64 || maxMessageBytes <= 0 || maxActorBytes <= 0 {
		return nil, 0, durable.ErrInvalidConfig
	}
	ctx, cancel := context.WithTimeout(context.Background(), store.timeout)
	defer cancel()
	transaction, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, fmt.Errorf("begin durable replay: %w", err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	var highWater int64
	err = transaction.QueryRow(ctx, `SELECT high_water FROM crdt_durable_groups WHERE group_id = $1`, groupID).Scan(&highWater)
	if errors.Is(err, pgx.ErrNoRows) {
		if after != 0 {
			return nil, 0, durable.ErrReplayUnavailable
		}
		if err := transaction.Commit(ctx); err != nil {
			return nil, 0, fmt.Errorf("commit empty durable replay: %w", err)
		}
		return nil, 0, nil
	}
	if err != nil || highWater < 0 {
		return nil, 0, durable.ErrCorruptStore
	}
	if after > uint64(highWater) || uint64(highWater)-after > maxEvents {
		return nil, 0, durable.ErrReplayUnavailable
	}
	rows, err := transaction.Query(ctx, `SELECT sequence, envelope FROM crdt_durable_events WHERE group_id = $1 AND sequence > $2 ORDER BY sequence`, groupID, int64(after))
	if err != nil {
		return nil, 0, fmt.Errorf("read durable replay: %w", err)
	}
	defer rows.Close()
	events := make([]durable.Event, 0, uint64(highWater)-after)
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
	if err := rows.Err(); err != nil || uint64(len(events)) != uint64(highWater)-after {
		return nil, 0, durable.ErrCorruptStore
	}
	if err := transaction.Commit(ctx); err != nil {
		return nil, 0, fmt.Errorf("commit durable replay: %w", err)
	}
	return events, uint64(highWater), nil
}

var _ durable.Log = (*Store)(nil)
