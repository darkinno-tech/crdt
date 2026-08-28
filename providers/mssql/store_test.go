package mssql

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/im10furry/crdt/durable"
)

func TestDialectAndConstructorBoundary(t *testing.T) {
	if _, err := New(nil, Config{MaxEvents: 1, MaxBytes: 1}); !errors.Is(err, durable.ErrInvalidConfig) {
		t.Fatalf("nil database = %v", err)
	}
	if dialect.MaxGroupIDBytes != MaxGroupIDBytes || dialect.MaxActorIDBytes != MaxActorIDBytes || dialect.MaxEnvelopeBytes != maxEnvelopeBytes {
		t.Fatalf("unexpected SQL Server bounds: %+v", dialect)
	}
	if dialect.AppendOptions.Isolation != sql.LevelSerializable || dialect.ReplayOptions.Isolation != sql.LevelSerializable || dialect.ReplayOptions.ReadOnly {
		t.Fatalf("unexpected SQL Server transaction options: append=%+v replay=%+v", dialect.AppendOptions, dialect.ReplayOptions)
	}
	if len(dialect.Schema) != 3 || !strings.Contains(dialect.Schema[1], "VARBINARY(MAX)") || !strings.Contains(dialect.Schema[2], "PRIMARY KEY CLUSTERED") || !strings.Contains(dialect.InsertGroup, "UPDLOCK, HOLDLOCK") || !strings.Contains(dialect.LockGroup, "UPDLOCK, HOLDLOCK") || !strings.Contains(dialect.ReadDot, "UPDLOCK, HOLDLOCK") {
		t.Fatalf("unexpected SQL Server dialect: %+v", dialect)
	}
}

func TestIdentifierBoundsPreserveSQLServerKeyLimits(t *testing.T) {
	validGroups := []string{
		strings.Repeat("a", MaxGroupIDUTF16Units),
		strings.Repeat("😀", MaxGroupIDUTF16Units/2),
		strings.Repeat("é", MaxGroupIDUTF16Units),
	}
	for _, value := range validGroups {
		if !dialect.ValidateGroupID(value) {
			t.Fatalf("valid group identifier rejected: %q", value)
		}
	}
	validActors := []string{
		strings.Repeat("a", MaxActorIDUTF16Units),
		strings.Repeat("😀", MaxActorIDUTF16Units/2),
	}
	for _, value := range validActors {
		if !dialect.ValidateActorID(value) {
			t.Fatalf("valid actor identifier rejected: %q", value)
		}
	}
	invalidGroups := []string{
		strings.Repeat("a", MaxGroupIDUTF16Units+1),
		strings.Repeat("😀", MaxGroupIDUTF16Units/2+1),
		" group",
		"group ",
		"group\x00id",
		string([]byte{0xff}),
	}
	for _, value := range invalidGroups {
		if dialect.ValidateGroupID(value) {
			t.Fatalf("invalid group identifier accepted: %q", value)
		}
	}
	if dialect.ValidateActorID(strings.Repeat("a", MaxActorIDUTF16Units+1)) {
		t.Fatal("oversized actor identifier accepted")
	}
}

func FuzzIdentifierValidation(f *testing.F) {
	for _, seed := range []string{"group", "😀", " group", "group ", "group\x00id", string([]byte{0xff})} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		for _, validator := range []struct {
			maxUnits int
			validate func(string) bool
		}{
			{MaxGroupIDUTF16Units, dialect.ValidateGroupID},
			{MaxActorIDUTF16Units, dialect.ValidateActorID},
		} {
			if !validator.validate(value) {
				continue
			}
			if !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.ContainsRune(value, 0) || len(utf16.Encode([]rune(value))) > validator.maxUnits {
				t.Fatalf("validator accepted an unsafe identifier %q", value)
			}
		}
	})
}
