package persistence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

var checkpointBucket = []byte("crdt-checkpoints-v1")

// Store owns a bbolt file containing checkpoints for one concrete CRDT state
// codec. bbolt serializes writes and permits concurrent read transactions;
// callers must still run one active process for a database path.
type Store struct {
	db     *bolt.DB
	config Config
	closed atomic.Bool
}

// Open opens or creates a checkpoint store at path with mode 0600. The parent
// directory must already exist and be protected by the host. A store is bound
// to Config.Validate, so a type or codec change must use an explicit migration
// rather than silently reinterpreting old bytes.
func Open(path string, config Config) (*Store, error) {
	if path == "" || !config.valid() {
		return nil, ErrInvalidConfig
	}
	if config.OpenTimeout == 0 {
		config.OpenTimeout = 5 * time.Second
	}
	if config.OpenTimeout < 0 {
		return nil, ErrInvalidConfig
	}
	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err != nil {
		return nil, fmt.Errorf("persistence store directory: %w", err)
	}
	if !info.IsDir() {
		return nil, ErrInvalidConfig
	}
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, ErrInvalidConfig
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect persistence store: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: config.OpenTimeout})
	if err != nil {
		return nil, fmt.Errorf("open persistence store: %w", err)
	}
	store := &Store{db: db, config: config}
	if err := db.Update(func(transaction *bolt.Tx) error {
		_, err := transaction.CreateBucketIfNotExists(checkpointBucket)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize persistence store: %w", err)
	}
	return store, nil
}

// Close releases the database file lock. Calls after Close return ErrClosed.
func (store *Store) Close() error {
	if store == nil || store.db == nil || !store.closed.CompareAndSwap(false, true) {
		return ErrClosed
	}
	if err := store.db.Close(); err != nil {
		return fmt.Errorf("close persistence store: %w", err)
	}
	return nil
}

// Save validates and atomically replaces name's complete checkpoint. The
// return from Save is the durable boundary for its snapshot, frontier, clock,
// cursor, and outbox; it does not acknowledge a remote peer or a separate
// database transaction.
func (store *Store) Save(name string, checkpoint Checkpoint) error {
	if store == nil || store.db == nil {
		return ErrClosed
	}
	if store.closed.Load() {
		return ErrClosed
	}
	if !store.validName(name) {
		return ErrInvalidCheckpoint
	}
	normalized, err := normalizeCheckpoint(checkpoint, store.config)
	if err != nil {
		return err
	}
	encoded, err := marshalCheckpoint(normalized, store.config)
	if err != nil {
		return err
	}
	if err := store.db.Update(func(transaction *bolt.Tx) error {
		bucket := transaction.Bucket(checkpointBucket)
		if bucket == nil {
			return ErrCorruptStore
		}
		return bucket.Put([]byte(name), encoded)
	}); err != nil {
		if errors.Is(err, ErrCorruptStore) {
			return err
		}
		if store.closed.Load() {
			return ErrClosed
		}
		return fmt.Errorf("save persistence checkpoint: %w", err)
	}
	return nil
}

// Load returns one validated checkpoint. found is false when name has not
// been saved. A malformed or semantically invalid stored value returns
// ErrCorruptStore and never returns a partial checkpoint.
func (store *Store) Load(name string) (checkpoint Checkpoint, found bool, err error) {
	if store == nil || store.db == nil || store.closed.Load() {
		return Checkpoint{}, false, ErrClosed
	}
	if !store.validName(name) {
		return Checkpoint{}, false, ErrInvalidCheckpoint
	}
	err = store.db.View(func(transaction *bolt.Tx) error {
		bucket := transaction.Bucket(checkpointBucket)
		if bucket == nil {
			return ErrCorruptStore
		}
		encoded := bucket.Get([]byte(name))
		if encoded == nil {
			return nil
		}
		decoded, err := unmarshalCheckpoint(encoded, store.config)
		if err != nil {
			return ErrCorruptStore
		}
		checkpoint = decoded
		found = true
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrCorruptStore) {
			return Checkpoint{}, false, ErrCorruptStore
		}
		if store.closed.Load() {
			return Checkpoint{}, false, ErrClosed
		}
		return Checkpoint{}, false, fmt.Errorf("load persistence checkpoint: %w", err)
	}
	return checkpoint, found, nil
}

func (config Config) valid() bool {
	return config.MaxRecordBytes > 0 && config.MaxStateBytes > 0 &&
		config.MaxFrontierEntries > 0 && config.MaxReplicaIDBytes > 0 &&
		config.MaxOutboxBytes >= 0 && config.MaxNameBytes > 0 &&
		config.Validate != nil
}

func (store *Store) validName(name string) bool {
	if len(name) == 0 || len(name) > store.config.MaxNameBytes {
		return false
	}
	for _, character := range name {
		if validNameCharacter(character) {
			continue
		}
		return false
	}
	return true
}

func validNameCharacter(character rune) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.'
}
