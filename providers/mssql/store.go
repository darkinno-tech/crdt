// Package mssql provides a Microsoft SQL Server-backed durable relay operation
// log. It imports no SQL Server driver: applications own their selected
// database/sql driver, credentials, TLS, pool policy, and migration lifecycle.
package mssql

import (
	"database/sql"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/im10furry/crdt/providers/internal/sqlrelay"
)

const (
	// MaxGroupIDUTF16Units and MaxActorIDUTF16Units keep the clustered
	// crdt_durable_mssql_dots primary key below SQL Server's 900-byte limit:
	// 350*2 + 90*2 + 8-byte counter = 888 bytes.
	MaxGroupIDUTF16Units = 350
	MaxActorIDUTF16Units = 90

	// MaxGroupIDBytes and MaxActorIDBytes are conservative UTF-8 allocation
	// guards. The stricter UTF-16 limits above are also validated before a
	// database call, so Unicode input cannot overflow an indexed NVARCHAR key.
	MaxGroupIDBytes = MaxGroupIDUTF16Units * utf8.UTFMax
	MaxActorIDBytes = MaxActorIDUTF16Units * utf8.UTFMax

	// SQL Server varbinary(max) accepts at most 2^31-1 bytes.
	maxEnvelopeBytes uint64 = 1<<31 - 1
)

// Config bounds one retained operation log per group.
type Config = sqlrelay.Config

// Store implements durable.Log. It does not close the supplied *sql.DB.
type Store = sqlrelay.Store

// ErrSequenceRange reports that a sequence or dot counter exceeds SQL
// Server's signed BIGINT range.
var ErrSequenceRange = sqlrelay.ErrSequenceRange

// New validates provider limits around an application-owned database/sql pool.
// Call EnsureSchema with a migration role before accepting traffic.
func New(database *sql.DB, config Config) (*Store, error) {
	return sqlrelay.New(database, config, dialect)
}

var dialect = sqlrelay.Dialect{
	Name:             "Microsoft SQL Server",
	MaxGroupIDBytes:  MaxGroupIDBytes,
	MaxActorIDBytes:  MaxActorIDBytes,
	MaxEnvelopeBytes: maxEnvelopeBytes,
	ValidateGroupID: func(value string) bool {
		return validIdentifier(value, MaxGroupIDUTF16Units)
	},
	ValidateActorID: func(value string) bool {
		return validIdentifier(value, MaxActorIDUTF16Units)
	},
	Schema: []string{
		`IF OBJECT_ID(N'dbo.crdt_durable_mssql_groups', N'U') IS NULL
BEGIN
  CREATE TABLE dbo.crdt_durable_mssql_groups (
    group_id NVARCHAR(350) COLLATE Latin1_General_100_BIN2 NOT NULL,
    high_water BIGINT NOT NULL CONSTRAINT crdt_durable_mssql_groups_high_water_nonnegative CHECK (high_water >= 0) DEFAULT 0,
    event_count BIGINT NOT NULL CONSTRAINT crdt_durable_mssql_groups_event_count_nonnegative CHECK (event_count >= 0) DEFAULT 0,
    used_bytes BIGINT NOT NULL CONSTRAINT crdt_durable_mssql_groups_used_bytes_nonnegative CHECK (used_bytes >= 0) DEFAULT 0,
    CONSTRAINT crdt_durable_mssql_groups_pk PRIMARY KEY CLUSTERED (group_id)
  );
END`,
		`IF OBJECT_ID(N'dbo.crdt_durable_mssql_events', N'U') IS NULL
BEGIN
  CREATE TABLE dbo.crdt_durable_mssql_events (
    group_id NVARCHAR(350) COLLATE Latin1_General_100_BIN2 NOT NULL,
    sequence BIGINT NOT NULL CONSTRAINT crdt_durable_mssql_events_sequence_positive CHECK (sequence > 0),
    envelope VARBINARY(MAX) NOT NULL,
    CONSTRAINT crdt_durable_mssql_events_pk PRIMARY KEY CLUSTERED (group_id, sequence),
    CONSTRAINT crdt_durable_mssql_events_group_fk FOREIGN KEY (group_id)
      REFERENCES dbo.crdt_durable_mssql_groups (group_id) ON DELETE NO ACTION
  );
END`,
		`IF OBJECT_ID(N'dbo.crdt_durable_mssql_dots', N'U') IS NULL
BEGIN
  CREATE TABLE dbo.crdt_durable_mssql_dots (
    group_id NVARCHAR(350) COLLATE Latin1_General_100_BIN2 NOT NULL,
    actor NVARCHAR(90) COLLATE Latin1_General_100_BIN2 NOT NULL,
    counter BIGINT NOT NULL CONSTRAINT crdt_durable_mssql_dots_counter_positive CHECK (counter > 0),
    sequence BIGINT NOT NULL CONSTRAINT crdt_durable_mssql_dots_sequence_positive CHECK (sequence > 0),
    digest BINARY(32) NOT NULL,
    CONSTRAINT crdt_durable_mssql_dots_pk PRIMARY KEY CLUSTERED (group_id, actor, counter),
    CONSTRAINT crdt_durable_mssql_dots_group_fk FOREIGN KEY (group_id)
      REFERENCES dbo.crdt_durable_mssql_groups (group_id) ON DELETE NO ACTION
  );
END`,
	},
	AppendOptions: sql.TxOptions{Isolation: sql.LevelSerializable},
	// go-mssqldb rejects database/sql's read-only transaction option. Replay
	// remains logically read-only because its fixed statement set contains no
	// writes; keep only the serializable isolation request at this boundary.
	ReplayOptions: sql.TxOptions{Isolation: sql.LevelSerializable},
	InsertGroup: `INSERT INTO dbo.crdt_durable_mssql_groups (group_id)
SELECT @p1
WHERE NOT EXISTS (
  SELECT 1
  FROM dbo.crdt_durable_mssql_groups WITH (UPDLOCK, HOLDLOCK)
  WHERE group_id = @p1
)`,
	LockGroup: `SELECT high_water, event_count, used_bytes
FROM dbo.crdt_durable_mssql_groups WITH (UPDLOCK, HOLDLOCK)
WHERE group_id = @p1`,
	ReadDot: `SELECT sequence, digest
FROM dbo.crdt_durable_mssql_dots WITH (UPDLOCK, HOLDLOCK)
WHERE group_id = @p1 AND actor = @p2 AND counter = @p3`,
	InsertEvent: `INSERT INTO dbo.crdt_durable_mssql_events (group_id, sequence, envelope) VALUES (@p1, @p2, @p3)`,
	InsertDot:   `INSERT INTO dbo.crdt_durable_mssql_dots (group_id, actor, counter, sequence, digest) VALUES (@p1, @p2, @p3, @p4, @p5)`,
	UpdateGroup: `UPDATE dbo.crdt_durable_mssql_groups SET high_water = @p1, event_count = @p2, used_bytes = @p3 WHERE group_id = @p4`,
	ReadHighWater: `SELECT high_water
FROM dbo.crdt_durable_mssql_groups
WHERE group_id = @p1`,
	ReadEvents: `SELECT sequence, envelope
FROM dbo.crdt_durable_mssql_events
WHERE group_id = @p1 AND sequence > @p2
ORDER BY sequence`,
}

func validIdentifier(value string, maxUTF16Units int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsRune(value, 0) && len(utf16.Encode([]rune(value))) <= maxUTF16Units
}
