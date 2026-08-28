package persistence

import (
	"errors"
	"time"

	"github.com/im10furry/crdt/snapshot"
)

var (
	// ErrInvalidConfig reports a missing or unsafe persistence configuration.
	ErrInvalidConfig = errors.New("crdt persistence: invalid configuration")
	// ErrInvalidCheckpoint reports a checkpoint that cannot safely be stored.
	ErrInvalidCheckpoint = errors.New("crdt persistence: invalid checkpoint")
	// ErrCorruptStore reports a damaged, unknown-version, or semantically
	// invalid record. Callers must restore from an independently verified backup
	// or checkpoint rather than accept a partial record.
	ErrCorruptStore = errors.New("crdt persistence: corrupt store")
	// ErrClosed reports use of a store after Close.
	ErrClosed = errors.New("crdt persistence: closed")
	// ErrMigration reports a record that is valid in its source format but
	// cannot be safely transformed to the configured format. The source bytes
	// are left untouched.
	ErrMigration = errors.New("crdt persistence: migration failed")
)

const (
	// RecordFormatV1 is the original checkpoint record envelope.
	RecordFormatV1 byte = 1
	// RecordFormatV2 is the current checkpoint record envelope. Its payload is
	// intentionally equivalent to v1, so applications can roll out the
	// versioned configuration before introducing a schema-specific transform.
	RecordFormatV2 byte = 2
	// CurrentRecordFormat is the format written when Config.Format.Version is
	// not set.
	CurrentRecordFormat = RecordFormatV2
)

// Compatibility controls which checkpoint record envelopes a store accepts.
// It affects only the local persistence envelope; CRDT frame, TypeID, codec,
// and Manifest compatibility remain separate negotiated contracts.
type Compatibility uint8

const (
	// CompatibilityDefault accepts the configured format and its immediately
	// preceding supported format. It is the safe upgrade default for v1 to v2.
	CompatibilityDefault Compatibility = iota
	// CompatibilityCurrentOnly rejects every record format other than Version.
	CompatibilityCurrentOnly
	// CompatibilityCurrentAndPrevious accepts Version and its immediately
	// preceding supported format.
	CompatibilityCurrentAndPrevious
)

// CheckpointMigration transforms a checkpoint that was decoded and validated
// with its source-format validator. It must return a complete replacement
// checkpoint suitable for validation by Config.Validate. It must not retain
// aliases to caller-owned bytes or maps.
type CheckpointMigration func(Checkpoint) (Checkpoint, error)

// Migration describes one accepted older record format. Validate is optional;
// when omitted, Config.Validate validates both source and target records.
// Transform is optional for envelope-only migrations such as v1 to v2.
type Migration struct {
	FromVersion byte
	Validate    snapshot.StateValidator
	Transform   CheckpointMigration
}

// FormatConfig keeps local-record version policy together. Version is the
// format written by Save; zero selects CurrentRecordFormat. Older records are
// read only when Compatibility permits them. When MigrateOnLoad is set, an
// accepted older record is transformed and atomically rewritten in Version.
type FormatConfig struct {
	Version       byte
	Compatibility Compatibility
	MigrateOnLoad bool
	Migrations    []Migration
}

// Config bounds every record before it is written or decoded. Limits are
// intentionally required rather than hidden defaults because a checkpoint's
// state, frontier, and outbox are application capacity decisions.
type Config struct {
	// Format controls local checkpoint envelope compatibility and optional
	// transactional migration. It does not change CRDT wire formats.
	Format FormatConfig
	// MaxRecordBytes bounds the complete encoded record, including metadata and
	// its checksum.
	MaxRecordBytes int
	// MaxStateBytes bounds the canonical CRDT state frame retained per record.
	MaxStateBytes int
	// MaxFrontierEntries bounds the number of replica tags retained with one
	// snapshot.
	MaxFrontierEntries int
	// MaxReplicaIDBytes bounds a frontier or HLC replica ID before allocating or
	// converting it to a string.
	MaxReplicaIDBytes int
	// MaxOutboxBytes bounds the application-owned opaque outbox retained with a
	// checkpoint. It is not a substitute for a bounded retry policy.
	MaxOutboxBytes int
	// MaxNameBytes bounds a checkpoint name. Names use a deliberately small
	// ASCII namespace so callers cannot accidentally treat them as file paths.
	MaxNameBytes int
	// OpenTimeout bounds waiting for bbolt's exclusive file lock. A zero value
	// uses five seconds.
	OpenTimeout time.Duration
	// Validate must perform concrete, bounded CRDT decoding for this store's
	// state type and codec. It runs on Save and Load.
	Validate snapshot.StateValidator
}

// Checkpoint is one atomically stored local recovery boundary. Snapshot
// already contains the canonical state, frontier, and (when required) HLC
// state. Cursor is normally the last durable-relay sequence whose effects are
// represented by Snapshot. Outbox remains opaque so the application can retain
// its canonical pending payloads in the same transaction without this package
// inventing transport or authorization semantics.
type Checkpoint struct {
	Snapshot snapshot.Snapshot
	Cursor   uint64
	Outbox   []byte
}

func (config Config) normalized() (Config, error) {
	if !config.validLimits() {
		return Config{}, ErrInvalidConfig
	}
	if config.OpenTimeout == 0 {
		config.OpenTimeout = 5 * time.Second
	}
	if config.OpenTimeout < 0 {
		return Config{}, ErrInvalidConfig
	}
	format, err := config.Format.normalized()
	if err != nil {
		return Config{}, err
	}
	config.Format = format
	return config, nil
}

func (config Config) validLimits() bool {
	return config.MaxRecordBytes > 0 && config.MaxStateBytes > 0 &&
		config.MaxFrontierEntries > 0 && config.MaxReplicaIDBytes > 0 &&
		config.MaxOutboxBytes >= 0 && config.MaxNameBytes > 0 &&
		config.Validate != nil
}

func (format FormatConfig) normalized() (FormatConfig, error) {
	if format.Version == 0 {
		format.Version = CurrentRecordFormat
	}
	if format.Version != RecordFormatV1 && format.Version != RecordFormatV2 {
		return FormatConfig{}, ErrInvalidConfig
	}
	if format.Compatibility == CompatibilityDefault {
		format.Compatibility = CompatibilityCurrentAndPrevious
	}
	if format.Compatibility != CompatibilityCurrentOnly && format.Compatibility != CompatibilityCurrentAndPrevious {
		return FormatConfig{}, ErrInvalidConfig
	}
	if format.Compatibility == CompatibilityCurrentAndPrevious && format.Version == RecordFormatV1 {
		return FormatConfig{}, ErrInvalidConfig
	}
	format.Migrations = append([]Migration(nil), format.Migrations...)
	seen := make(map[byte]struct{}, len(format.Migrations))
	for _, migration := range format.Migrations {
		if migration.FromVersion == format.Version || !format.accepts(migration.FromVersion) {
			return FormatConfig{}, ErrInvalidConfig
		}
		if _, ok := seen[migration.FromVersion]; ok {
			return FormatConfig{}, ErrInvalidConfig
		}
		seen[migration.FromVersion] = struct{}{}
	}
	return format, nil
}

func (format FormatConfig) accepts(version byte) bool {
	if version == format.Version {
		return true
	}
	return format.Compatibility == CompatibilityCurrentAndPrevious &&
		format.Version == RecordFormatV2 && version == RecordFormatV1
}

func (format FormatConfig) migration(version byte) (Migration, bool) {
	for _, migration := range format.Migrations {
		if migration.FromVersion == version {
			return migration, true
		}
	}
	return Migration{}, false
}

func (config Config) validatorFor(version byte) snapshot.StateValidator {
	if migration, ok := config.Format.migration(version); ok && migration.Validate != nil {
		return migration.Validate
	}
	return config.Validate
}

// Store is the local recovery-store contract. Save must atomically replace one
// complete checkpoint: its snapshot, frontier, HLC state (when required),
// relay cursor, and outbox either all become durable or none do. Load must
// validate stored data before returning it and must never return a partial
// checkpoint.
//
// Store deliberately does not model a distributed transaction, remote
// acknowledgement, identity, encryption, backup, or tombstone collection.
// Applications own its lifetime and must not use a closed Store.
type Store interface {
	Save(name string, checkpoint Checkpoint) error
	Load(name string) (checkpoint Checkpoint, found bool, err error)
	Delete(name string) (found bool, err error)
	Close() error
}
