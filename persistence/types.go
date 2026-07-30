package persistence

import (
	"errors"
	"time"

	"github.com/DarkInno/crdt/snapshot"
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
)

// Config bounds every record before it is written or decoded. Limits are
// intentionally required rather than hidden defaults because a checkpoint's
// state, frontier, and outbox are application capacity decisions.
type Config struct {
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
