// Package mysql provides a MySQL-backed durable relay operation log.
//
// It imports no MySQL driver. Applications construct their own *sql.DB with a
// driver, TLS, credentials, pool policy, and explicit migration lifecycle.
package mysql

import (
	"database/sql"

	"github.com/darkinno-tech/crdt/providers/internal/sqlrelay"
)

const (
	// MaxGroupIDBytes and MaxActorIDBytes fit the MySQL InnoDB primary keys in
	// this provider's fixed schema. Keep them aligned with the DDL below.
	MaxGroupIDBytes         = 1024
	MaxActorIDBytes         = 255
	maxEnvelopeBytes uint64 = 1<<32 - 1 // MySQL LONGBLOB maximum length.
)

// Config bounds one retained operation log per group.
type Config = sqlrelay.Config

// Store implements durable.Log. It does not close the supplied *sql.DB.
type Store = sqlrelay.Store

// ErrSequenceRange reports that a sequence or dot counter exceeds MySQL's
// signed BIGINT range.
var ErrSequenceRange = sqlrelay.ErrSequenceRange

// New validates provider limits around an application-owned database/sql pool.
// Call EnsureSchema with a migration role before accepting traffic.
func New(database *sql.DB, config Config) (*Store, error) {
	return sqlrelay.New(database, config, dialect)
}

var dialect = sqlrelay.Dialect{
	Name:             "MySQL",
	MaxGroupIDBytes:  MaxGroupIDBytes,
	MaxActorIDBytes:  MaxActorIDBytes,
	MaxEnvelopeBytes: maxEnvelopeBytes,
	Schema: []string{
		`CREATE TABLE IF NOT EXISTS crdt_durable_mysql_groups (
  group_id VARBINARY(1024) NOT NULL,
  high_water BIGINT NOT NULL DEFAULT 0,
  event_count BIGINT NOT NULL DEFAULT 0,
  used_bytes BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (group_id),
  CHECK (high_water >= 0),
  CHECK (event_count >= 0),
  CHECK (used_bytes >= 0)
) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS crdt_durable_mysql_events (
  group_id VARBINARY(1024) NOT NULL,
  sequence BIGINT NOT NULL,
  envelope LONGBLOB NOT NULL,
  PRIMARY KEY (group_id, sequence),
  CONSTRAINT crdt_durable_mysql_events_group_fk FOREIGN KEY (group_id)
    REFERENCES crdt_durable_mysql_groups(group_id) ON DELETE RESTRICT,
  CHECK (sequence > 0)
) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS crdt_durable_mysql_dots (
  group_id VARBINARY(1024) NOT NULL,
  actor VARBINARY(255) NOT NULL,
  counter BIGINT NOT NULL,
  sequence BIGINT NOT NULL,
  digest BINARY(32) NOT NULL,
  PRIMARY KEY (group_id, actor, counter),
  CONSTRAINT crdt_durable_mysql_dots_group_fk FOREIGN KEY (group_id)
    REFERENCES crdt_durable_mysql_groups(group_id) ON DELETE RESTRICT,
  CHECK (counter > 0),
  CHECK (sequence > 0)
) ENGINE=InnoDB`,
	},
	AppendOptions: sql.TxOptions{Isolation: sql.LevelReadCommitted},
	ReplayOptions: sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true},
	InsertGroup:   `INSERT IGNORE INTO crdt_durable_mysql_groups (group_id) VALUES (?)`,
	LockGroup:     `SELECT high_water, event_count, used_bytes FROM crdt_durable_mysql_groups WHERE group_id = ? FOR UPDATE`,
	ReadDot:       `SELECT sequence, digest FROM crdt_durable_mysql_dots WHERE group_id = ? AND actor = ? AND counter = ? FOR UPDATE`,
	InsertEvent:   `INSERT INTO crdt_durable_mysql_events (group_id, sequence, envelope) VALUES (?, ?, ?)`,
	InsertDot:     `INSERT INTO crdt_durable_mysql_dots (group_id, actor, counter, sequence, digest) VALUES (?, ?, ?, ?, ?)`,
	UpdateGroup:   `UPDATE crdt_durable_mysql_groups SET high_water = ?, event_count = ?, used_bytes = ? WHERE group_id = ?`,
	ReadHighWater: `SELECT high_water FROM crdt_durable_mysql_groups WHERE group_id = ?`,
	ReadEvents:    `SELECT sequence, envelope FROM crdt_durable_mysql_events WHERE group_id = ? AND sequence > ? ORDER BY sequence`,
}
