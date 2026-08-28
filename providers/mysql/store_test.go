package mysql

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
	if dialect.MaxGroupIDBytes != MaxGroupIDBytes || dialect.MaxActorIDBytes != MaxActorIDBytes || dialect.MaxEnvelopeBytes != maxEnvelopeBytes {
		t.Fatalf("unexpected MySQL bounds: %+v", dialect)
	}
	if dialect.AppendOptions.Isolation != sql.LevelReadCommitted || dialect.ReplayOptions.Isolation != sql.LevelRepeatableRead || !dialect.ReplayOptions.ReadOnly {
		t.Fatalf("unexpected MySQL transaction options: append=%+v replay=%+v", dialect.AppendOptions, dialect.ReplayOptions)
	}
	if len(dialect.Schema) != 3 || !strings.Contains(dialect.Schema[1], "LONGBLOB") || !strings.Contains(dialect.LockGroup, "FOR UPDATE") || !strings.Contains(dialect.ReadDot, "FOR UPDATE") {
		t.Fatalf("unexpected MySQL dialect: %+v", dialect)
	}
}
