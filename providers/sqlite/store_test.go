package sqlite

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/im10furry/crdt/durable"
)

func TestDialectAndConstructorBoundary(t *testing.T) {
	if _, err := New(nil, Config{MaxEvents: 1, MaxBytes: 1}); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("nil database = %v", err)
	}
	if dialect.MaxGroupIDBytes != MaxGroupIDBytes || dialect.MaxActorIDBytes != MaxActorIDBytes || dialect.MaxEnvelopeBytes != 0 {
		t.Fatalf("unexpected SQLite bounds: %+v", dialect)
	}
	if dialect.AppendOptions.Isolation != sql.LevelSerializable || dialect.ReplayOptions.Isolation != sql.LevelSerializable || !dialect.ReplayOptions.ReadOnly {
		t.Fatalf("unexpected SQLite transaction options: append=%+v replay=%+v", dialect.AppendOptions, dialect.ReplayOptions)
	}
	if len(dialect.Schema) != 3 || !strings.Contains(dialect.Schema[0], "BLOB") || !strings.Contains(dialect.InsertGroup, "INSERT OR IGNORE") || strings.Contains(dialect.LockGroup, "FOR UPDATE") {
		t.Fatalf("unexpected SQLite dialect: %+v", dialect)
	}
}
