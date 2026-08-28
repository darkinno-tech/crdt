// Package sqlite provides an SQLite-backed durable relay operation log.
//
// It imports no SQLite driver. Applications own their selected driver, file
// permissions, busy timeout, encryption, and backup/restore policy.
package sqlite

import (
	"database/sql"

	"github.com/im10furry/crdt/providers/internal/sqlrelay"
)

const (
	// MaxGroupIDBytes and MaxActorIDBytes prevent unbounded primary-key work.
	MaxGroupIDBytes = 1024
	MaxActorIDBytes = 255
)

// Config bounds one retained operation log per group.
type Config = sqlrelay.Config

// Store implements durable.Log. It does not close the supplied *sql.DB.
type Store = sqlrelay.Store

// ErrSequenceRange reports that a sequence or dot counter exceeds SQLite's
// signed INTEGER range.
var ErrSequenceRange = sqlrelay.ErrSequenceRange

// New validates provider limits around an application-owned database/sql pool.
// Call EnsureSchema with a migration role before accepting traffic.
func New(database *sql.DB, config Config) (*Store, error) {
	return sqlrelay.New(database, config, dialect)
}

var dialect = sqlrelay.Dialect{
	Name:            "SQLite",
	MaxGroupIDBytes: MaxGroupIDBytes,
	MaxActorIDBytes: MaxActorIDBytes,
	Schema: []string{
		`CREATE TABLE IF NOT EXISTS crdt_durable_sqlite_groups (
  group_id BLOB PRIMARY KEY NOT NULL,
  high_water INTEGER NOT NULL DEFAULT 0 CHECK (high_water >= 0),
  event_count INTEGER NOT NULL DEFAULT 0 CHECK (event_count >= 0),
  used_bytes INTEGER NOT NULL DEFAULT 0 CHECK (used_bytes >= 0)
)`,
		`CREATE TABLE IF NOT EXISTS crdt_durable_sqlite_events (
  group_id BLOB NOT NULL REFERENCES crdt_durable_sqlite_groups(group_id) ON DELETE RESTRICT,
  sequence INTEGER NOT NULL CHECK (sequence > 0),
  envelope BLOB NOT NULL,
  PRIMARY KEY (group_id, sequence)
)`,
		`CREATE TABLE IF NOT EXISTS crdt_durable_sqlite_dots (
  group_id BLOB NOT NULL REFERENCES crdt_durable_sqlite_groups(group_id) ON DELETE RESTRICT,
  actor BLOB NOT NULL,
  counter INTEGER NOT NULL CHECK (counter > 0),
  sequence INTEGER NOT NULL CHECK (sequence > 0),
  digest BLOB NOT NULL CHECK (length(digest) = 32),
  PRIMARY KEY (group_id, actor, counter)
)`,
	},
	AppendOptions: sql.TxOptions{Isolation: sql.LevelSerializable},
	ReplayOptions: sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: true},
	InsertGroup:   `INSERT OR IGNORE INTO crdt_durable_sqlite_groups (group_id) VALUES (?)`,
	LockGroup:     `SELECT high_water, event_count, used_bytes FROM crdt_durable_sqlite_groups WHERE group_id = ?`,
	ReadDot:       `SELECT sequence, digest FROM crdt_durable_sqlite_dots WHERE group_id = ? AND actor = ? AND counter = ?`,
	InsertEvent:   `INSERT INTO crdt_durable_sqlite_events (group_id, sequence, envelope) VALUES (?, ?, ?)`,
	InsertDot:     `INSERT INTO crdt_durable_sqlite_dots (group_id, actor, counter, sequence, digest) VALUES (?, ?, ?, ?, ?)`,
	UpdateGroup:   `UPDATE crdt_durable_sqlite_groups SET high_water = ?, event_count = ?, used_bytes = ? WHERE group_id = ?`,
	ReadHighWater: `SELECT high_water FROM crdt_durable_sqlite_groups WHERE group_id = ?`,
	ReadEvents:    `SELECT sequence, envelope FROM crdt_durable_sqlite_events WHERE group_id = ? AND sequence > ? ORDER BY sequence`,
}
