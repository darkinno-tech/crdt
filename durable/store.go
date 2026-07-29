package durable

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/replica"
	bolt "go.etcd.io/bbolt"
)

var (
	bucketGroups = []byte("groups")
	bucketEvents = []byte("events")
	bucketDots   = []byte("dots")
	bucketMeta   = []byte("meta")

	keyHighWater = []byte("high-water")
	keyCount     = []byte("count")
	keyBytes     = []byte("bytes")
)

// StoreConfig bounds retained canonical event data per replication group. Both
// limits are required: durable replay must apply an explicit overload policy
// rather than retaining unbounded history.
type StoreConfig struct {
	MaxEvents   uint64
	MaxBytes    uint64
	OpenTimeout time.Duration
}

// Store is a bbolt-backed, single-writer operation log. bbolt enforces an
// exclusive file lock; deployments must still schedule only one active relay
// process for a data file.
type Store struct {
	db        *bolt.DB
	maxEvents uint64
	maxBytes  uint64
	closed    atomic.Bool
}

// OpenStore opens or creates a durable operation log at path with mode 0600.
// The parent directory must already be owned and protected by the host.
func OpenStore(path string, config StoreConfig) (*Store, error) {
	if path == "" || config.MaxEvents == 0 || config.MaxBytes == 0 {
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
		return nil, fmt.Errorf("durable store directory: %w", err)
	}
	if !info.IsDir() {
		return nil, ErrInvalidConfig
	}
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, ErrInvalidConfig
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect durable store: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: config.OpenTimeout})
	if err != nil {
		return nil, fmt.Errorf("open durable store: %w", err)
	}
	store := &Store{db: db, maxEvents: config.MaxEvents, maxBytes: config.MaxBytes}
	if err := db.Update(func(transaction *bolt.Tx) error {
		_, err := transaction.CreateBucketIfNotExists(bucketGroups)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize durable store: %w", err)
	}
	return store, nil
}

// Close releases the database lock. Calls after Close fail with ErrClosed.
func (store *Store) Close() error {
	if store == nil || store.db == nil || !store.closed.CompareAndSwap(false, true) {
		return ErrClosed
	}
	if err := store.db.Close(); err != nil {
		return fmt.Errorf("close durable store: %w", err)
	}
	return nil
}

type appendResult struct {
	Event     Event
	Duplicate bool
}

// Append transactionally binds a Dot to its canonical envelope and allocates
// the next group-local sequence for new data. The caller must validate the
// concrete CRDT delta before invoking Append.
func (store *Store) Append(groupID string, change replica.Change) (appendResult, error) {
	if store == nil || store.db == nil || groupID == "" {
		return appendResult{}, ErrInvalidConfig
	}
	if store.closed.Load() {
		return appendResult{}, ErrClosed
	}
	encoded, err := marshalChange(change)
	if err != nil {
		return appendResult{}, err
	}
	if uint64(len(encoded)) > store.maxBytes {
		return appendResult{}, ErrStoreFull
	}
	digest := sha256.Sum256(encoded)
	dotKey := makeDotKey(change.Dot)
	var result appendResult
	err = store.db.Update(func(transaction *bolt.Tx) error {
		group, err := store.groupBucket(transaction, groupID, true)
		if err != nil {
			return err
		}
		events := group.Bucket(bucketEvents)
		dots := group.Bucket(bucketDots)
		meta := group.Bucket(bucketMeta)
		if events == nil || dots == nil || meta == nil {
			return ErrCorruptStore
		}
		if existing := dots.Get(dotKey); existing != nil {
			sequence, existingDigest, err := parseDotBinding(existing)
			if err != nil {
				return err
			}
			if existingDigest != digest {
				return ErrConflictingDot
			}
			result = appendResult{Event: Event{Sequence: sequence, Change: change}, Duplicate: true}
			return nil
		}
		highWater, count, usedBytes, err := readMeta(meta)
		if err != nil {
			return err
		}
		if count >= store.maxEvents || usedBytes > store.maxBytes-uint64(len(encoded)) || highWater == math.MaxUint64 {
			return ErrStoreFull
		}
		sequence := highWater + 1
		if err := events.Put(sequenceKey(sequence), encoded); err != nil {
			return err
		}
		if err := dots.Put(dotKey, appendDotBinding(sequence, digest)); err != nil {
			return err
		}
		if err := writeMeta(meta, sequence, count+1, usedBytes+uint64(len(encoded))); err != nil {
			return err
		}
		result = appendResult{Event: Event{Sequence: sequence, Change: change}}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrConflictingDot) || errors.Is(err, ErrStoreFull) || errors.Is(err, ErrCorruptStore) {
			return appendResult{}, err
		}
		return appendResult{}, fmt.Errorf("append durable event: %w", err)
	}
	return result, nil
}

// Replay atomically reads every event after after in sequence order. It fails
// rather than returning a prefix when the caller's explicit replay budget
// cannot cover the entire missed suffix.
func (store *Store) Replay(groupID string, after, maxEvents, maxBytes uint64, manifest replica.Manifest, policy crdt.ProtocolPolicy, maxMessageBytes, maxActorBytes int) ([]Event, uint64, error) {
	if store == nil || store.db == nil || groupID == "" || maxEvents == 0 || maxBytes == 0 || maxMessageBytes <= 0 || maxActorBytes <= 0 {
		return nil, 0, ErrInvalidConfig
	}
	if store.closed.Load() {
		return nil, 0, ErrClosed
	}
	var events []Event
	var highWater uint64
	err := store.db.View(func(transaction *bolt.Tx) error {
		group, err := store.groupBucket(transaction, groupID, false)
		if err != nil {
			return err
		}
		if group == nil {
			highWater = 0
			if after != 0 {
				return ErrReplayUnavailable
			}
			return nil
		}
		eventBucket := group.Bucket(bucketEvents)
		meta := group.Bucket(bucketMeta)
		if eventBucket == nil || meta == nil {
			return ErrCorruptStore
		}
		var errMeta error
		highWater, _, _, errMeta = readMeta(meta)
		if errMeta != nil {
			return errMeta
		}
		if after > highWater {
			return ErrReplayUnavailable
		}
		if after == highWater {
			return nil
		}
		if highWater-after > maxEvents {
			return ErrReplayUnavailable
		}
		cursor := eventBucket.Cursor()
		key, value := cursor.Seek(sequenceKey(after + 1))
		var usedBytes uint64
		for expected := after + 1; expected <= highWater; expected++ {
			if key == nil || bytesToSequence(key) != expected || value == nil {
				return ErrCorruptStore
			}
			if uint64(len(value)) > maxBytes-usedBytes || len(value) > maxMessageBytes {
				return ErrReplayUnavailable
			}
			dot, delta, err := unmarshalChange(value, maxMessageBytes, maxActorBytes)
			if err != nil {
				return ErrCorruptStore
			}
			change, err := replica.NewChangeWithPolicy(manifest, dot, delta, policy)
			if err != nil {
				return ErrCorruptStore
			}
			events = append(events, Event{Sequence: expected, Change: change})
			usedBytes += uint64(len(value))
			key, value = cursor.Next()
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrReplayUnavailable) || errors.Is(err, ErrCorruptStore) {
			return nil, 0, err
		}
		return nil, 0, fmt.Errorf("replay durable events: %w", err)
	}
	return events, highWater, nil
}

func (store *Store) groupBucket(transaction *bolt.Tx, groupID string, create bool) (*bolt.Bucket, error) {
	groups := transaction.Bucket(bucketGroups)
	if groups == nil {
		return nil, ErrCorruptStore
	}
	if !create {
		return groups.Bucket([]byte(groupID)), nil
	}
	group, err := groups.CreateBucketIfNotExists([]byte(groupID))
	if err != nil {
		return nil, err
	}
	for _, name := range [][]byte{bucketEvents, bucketDots, bucketMeta} {
		if _, err := group.CreateBucketIfNotExists(name); err != nil {
			return nil, err
		}
	}
	return group, nil
}

func makeDotKey(dot replica.Dot) []byte {
	key := make([]byte, 0, len(dot.Actor)+binary.MaxVarintLen64+8)
	key = frame.AppendUvarint(key, uint64(len(dot.Actor)))
	key = append(key, dot.Actor...)
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], dot.Counter)
	return append(key, counter[:]...)
}

func sequenceKey(sequence uint64) []byte {
	var key [8]byte
	binary.BigEndian.PutUint64(key[:], sequence)
	return key[:]
}

func bytesToSequence(key []byte) uint64 {
	if len(key) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(key)
}

func appendDotBinding(sequence uint64, digest [sha256.Size]byte) []byte {
	value := make([]byte, 8+sha256.Size)
	binary.BigEndian.PutUint64(value[:8], sequence)
	copy(value[8:], digest[:])
	return value
}

func parseDotBinding(value []byte) (uint64, [sha256.Size]byte, error) {
	if len(value) != 8+sha256.Size {
		return 0, [sha256.Size]byte{}, ErrCorruptStore
	}
	var digest [sha256.Size]byte
	copy(digest[:], value[8:])
	sequence := binary.BigEndian.Uint64(value[:8])
	if sequence == 0 {
		return 0, [sha256.Size]byte{}, ErrCorruptStore
	}
	return sequence, digest, nil
}

func readMeta(bucket *bolt.Bucket) (uint64, uint64, uint64, error) {
	highWater, err := readUint64(bucket.Get(keyHighWater))
	if err != nil {
		return 0, 0, 0, err
	}
	count, err := readUint64(bucket.Get(keyCount))
	if err != nil {
		return 0, 0, 0, err
	}
	usedBytes, err := readUint64(bucket.Get(keyBytes))
	if err != nil {
		return 0, 0, 0, err
	}
	if highWater < count {
		return 0, 0, 0, ErrCorruptStore
	}
	return highWater, count, usedBytes, nil
}

func writeMeta(bucket *bolt.Bucket, highWater, count, usedBytes uint64) error {
	for _, item := range []struct {
		key   []byte
		value uint64
	}{
		{keyHighWater, highWater},
		{keyCount, count},
		{keyBytes, usedBytes},
	} {
		if err := bucket.Put(item.key, sequenceKey(item.value)); err != nil {
			return err
		}
	}
	return nil
}

func readUint64(value []byte) (uint64, error) {
	if value == nil {
		return 0, nil
	}
	if len(value) != 8 {
		return 0, ErrCorruptStore
	}
	return binary.BigEndian.Uint64(value), nil
}
