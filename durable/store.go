package durable

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/merkle"
	"github.com/DarkInno/crdt/replica"
	bolt "go.etcd.io/bbolt"
)

var (
	bucketGroups = []byte("groups")
	bucketEvents = []byte("events")
	bucketDots   = []byte("dots")
	bucketActors = []byte("actors")
	bucketHLCs   = []byte("hlcs")
	bucketHLCIdx = []byte("hlc-index")
	bucketMeta   = []byte("meta")

	keyHighWater  = []byte("high-water")
	keyCount      = []byte("count")
	keyBytes      = []byte("bytes")
	keyActorIndex = []byte("actor-index-v1")
	keyHLCState   = []byte("hlc-state-v1")
)

// StoreConfig bounds retained canonical event data per replication group. Both
// limits are required: durable replay must apply an explicit overload policy
// rather than retaining unbounded history.
type StoreConfig struct {
	MaxEvents   uint64
	MaxBytes    uint64
	OpenTimeout time.Duration
	// HLCReplicaID enables the no-state-vector HLC/Merkle anti-entropy
	// capability. The store persists this relay-local clock in the same bbolt
	// transaction as every newly committed event. Empty preserves the legacy
	// cursor/state-vector-only behavior.
	HLCReplicaID string
}

// Store is a bbolt-backed, single-writer operation log. bbolt enforces an
// exclusive file lock; deployments must still schedule only one active relay
// process for a data file.
type Store struct {
	db           *bolt.DB
	maxEvents    uint64
	maxBytes     uint64
	hlcReplicaID string
	closed       atomic.Bool
}

// OpenStore opens or creates a durable operation log at path with mode 0600.
// The parent directory must already be owned and protected by the host.
func OpenStore(path string, config StoreConfig) (*Store, error) {
	if path == "" || config.MaxEvents == 0 || config.MaxBytes == 0 {
		return nil, invalidConfig("durable.open_store", ErrInvalidConfig)
	}
	if config.OpenTimeout == 0 {
		config.OpenTimeout = 5 * time.Second
	}
	if config.OpenTimeout < 0 {
		return nil, invalidConfig("durable.open_store", ErrInvalidConfig)
	}
	if config.HLCReplicaID != "" {
		if _, err := clock.NewHLC(config.HLCReplicaID); err != nil {
			return nil, invalidConfig("durable.open_store", ErrInvalidConfig)
		}
	}
	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err != nil {
		return nil, fmt.Errorf("durable store directory: %w", err)
	}
	if !info.IsDir() {
		return nil, invalidConfig("durable.open_store", ErrInvalidConfig)
	}
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, invalidConfig("durable.open_store", ErrInvalidConfig)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect durable store: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: config.OpenTimeout})
	if err != nil {
		return nil, fmt.Errorf("open durable store: %w", err)
	}
	store := &Store{db: db, maxEvents: config.MaxEvents, maxBytes: config.MaxBytes, hlcReplicaID: config.HLCReplicaID}
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

// Closed reports whether Close has completed or is in progress. It allows a
// Handler to fail closed without taking ownership of the store's lifetime.
func (store *Store) Closed() bool {
	return store == nil || store.db == nil || store.closed.Load()
}

// MerkleEnabled reports whether this store was explicitly configured to
// persist a relay HLC with each event. A disabled store remains a valid v1/v2
// Log but must not advertise the v3 anti-entropy protocol.
func (store *Store) MerkleEnabled() bool {
	return store != nil && store.db != nil && !store.closed.Load() && store.hlcReplicaID != ""
}

// Append transactionally binds a Dot to its canonical envelope and allocates
// the next group-local sequence for new data. The caller must validate the
// concrete CRDT delta before invoking Append.
func (store *Store) Append(groupID string, change replica.Change) (AppendResult, error) {
	if store == nil || store.db == nil || groupID == "" {
		return AppendResult{}, ErrInvalidConfig
	}
	if store.closed.Load() {
		return AppendResult{}, ErrClosed
	}
	if err := store.ensureActorIndex(groupID); err != nil {
		return AppendResult{}, err
	}
	encoded, err := marshalChange(change)
	if err != nil {
		return AppendResult{}, err
	}
	if uint64(len(encoded)) > store.maxBytes {
		return AppendResult{}, ErrStoreFull
	}
	digest := sha256.Sum256(encoded)
	dotKey := makeDotKey(change.Dot)
	var result AppendResult
	err = store.db.Update(func(transaction *bolt.Tx) error {
		group, err := store.groupBucket(transaction, groupID, true)
		if err != nil {
			return err
		}
		events := group.Bucket(bucketEvents)
		dots := group.Bucket(bucketDots)
		actors := group.Bucket(bucketActors)
		hlcs := group.Bucket(bucketHLCs)
		hlcIndex := group.Bucket(bucketHLCIdx)
		meta := group.Bucket(bucketMeta)
		if events == nil || dots == nil || actors == nil || hlcs == nil || hlcIndex == nil || meta == nil {
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
			event := Event{Sequence: sequence, Change: change}
			if store.hlcReplicaID != "" {
				tag, err := relayHLCState(hlcs.Get(sequenceKey(sequence)), store.hlcReplicaID, frame.DefaultLimits().MaxStringBytes)
				if err != nil || hlcs.Get(sequenceKey(sequence)) == nil {
					return ErrAntiEntropyUnavailable
				}
				event.HLC = tag
			}
			result = AppendResult{Event: event, Duplicate: true}
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
		event := Event{Sequence: sequence, Change: change}
		var nextHLCState []byte
		if store.hlcReplicaID != "" {
			state, err := relayHLCState(meta.Get(keyHLCState), store.hlcReplicaID, frame.DefaultLimits().MaxStringBytes)
			if err != nil {
				return err
			}
			relay, err := clock.NewHLCFromState(clock.State{ReplicaID: state.ReplicaID, WallTime: state.WallTime, Logical: state.Logical})
			if err != nil {
				return ErrCorruptStore
			}
			event.HLC, err = relay.Now()
			if err != nil {
				return ErrCorruptStore
			}
			next := relay.Snapshot()
			nextHLCState = encodeRelayHLCState(crdt.Tag{ReplicaID: next.ReplicaID, WallTime: next.WallTime, Logical: next.Logical})
			if hlcIndex.Get([]byte(merkleLeafKey(event.HLC))) != nil {
				return ErrCorruptStore
			}
		}
		if err := events.Put(sequenceKey(sequence), encoded); err != nil {
			return err
		}
		if err := dots.Put(dotKey, appendDotBinding(sequence, digest)); err != nil {
			return err
		}
		actorKey := []byte(change.Dot.Actor)
		maximum, err := readUint64(actors.Get(actorKey))
		if err != nil {
			return err
		}
		if change.Dot.Counter > maximum {
			if err := actors.Put(actorKey, sequenceKey(change.Dot.Counter)); err != nil {
				return err
			}
		}
		if err := meta.Put(keyActorIndex, []byte{1}); err != nil {
			return err
		}
		if store.hlcReplicaID != "" {
			if err := hlcs.Put(sequenceKey(sequence), encodeRelayHLCState(event.HLC)); err != nil {
				return err
			}
			if err := hlcIndex.Put([]byte(merkleLeafKey(event.HLC)), sequenceKey(sequence)); err != nil {
				return err
			}
			if err := meta.Put(keyHLCState, nextHLCState); err != nil {
				return err
			}
		}
		if err := writeMeta(meta, sequence, count+1, usedBytes+uint64(len(encoded))); err != nil {
			return err
		}
		result = AppendResult{Event: event}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrConflictingDot) || errors.Is(err, ErrStoreFull) || errors.Is(err, ErrCorruptStore) {
			return AppendResult{}, err
		}
		return AppendResult{}, fmt.Errorf("append durable event: %w", err)
	}
	return result, nil
}

// CatchUp returns the complete bounded set of events not covered by vector.
// The vector represents only contiguous locally installed Dot prefixes. A
// vector ahead of retained log data, or a required suffix beyond the caller's
// explicit limits, fails closed rather than silently declaring a client synced.
func (store *Store) CatchUp(groupID string, vector replica.Frontier, maxEvents, maxBytes uint64, manifest replica.Manifest, policy crdt.ProtocolPolicy, maxMessageBytes, maxActorBytes int) ([]Event, uint64, error) {
	if store == nil || store.db == nil || groupID == "" || maxEvents == 0 || maxBytes == 0 || maxMessageBytes <= 0 || maxActorBytes <= 0 {
		return nil, 0, ErrInvalidConfig
	}
	if store.closed.Load() {
		return nil, 0, ErrClosed
	}
	validatedVector, err := replica.NewFrontier(vector.Entries())
	if err != nil {
		return nil, 0, ErrInvalidConfig
	}
	if err := store.ensureActorIndex(groupID); err != nil {
		return nil, 0, err
	}
	var events []Event
	var highWater uint64
	err = store.db.View(func(transaction *bolt.Tx) error {
		group, err := store.groupBucket(transaction, groupID, false)
		if err != nil {
			return err
		}
		if group == nil {
			if len(validatedVector.Entries()) != 0 {
				return ErrReplayUnavailable
			}
			return nil
		}
		eventBucket := group.Bucket(bucketEvents)
		dots := group.Bucket(bucketDots)
		actors := group.Bucket(bucketActors)
		meta := group.Bucket(bucketMeta)
		if eventBucket == nil || dots == nil || actors == nil || meta == nil || !bytes.Equal(meta.Get(keyActorIndex), []byte{1}) {
			return ErrCorruptStore
		}
		var errMeta error
		highWater, _, _, errMeta = readMeta(meta)
		if errMeta != nil {
			return errMeta
		}
		for actor, counter := range validatedVector.Entries() {
			maximum, err := readUint64(actors.Get([]byte(actor)))
			if err != nil || maximum < counter {
				return ErrReplayUnavailable
			}
		}
		var usedBytes uint64
		cursor := actors.Cursor()
		for actorKey, maximumValue := cursor.First(); actorKey != nil; actorKey, maximumValue = cursor.Next() {
			actor := string(actorKey)
			maximum, err := readUint64(maximumValue)
			if err != nil || maximum == 0 {
				return ErrCorruptStore
			}
			current := validatedVector.Counter(actor)
			if maximum <= current {
				continue
			}
			prefix := dotPrefix(actor)
			dotCursor := dots.Cursor()
			first := append(append([]byte(nil), prefix...), sequenceKey(current+1)...)
			for key, binding := dotCursor.Seek(first); key != nil && bytes.HasPrefix(key, prefix); key, binding = dotCursor.Next() {
				dot, err := dotFromKey(key)
				if err != nil || dot.Actor != actor || dot.Counter > maximum {
					return ErrCorruptStore
				}
				sequence, _, err := parseDotBinding(binding)
				if err != nil {
					return err
				}
				encoded := eventBucket.Get(sequenceKey(sequence))
				if encoded == nil || uint64(len(encoded)) > maxBytes-usedBytes || len(encoded) > maxMessageBytes || uint64(len(events)) >= maxEvents {
					return ErrReplayUnavailable
				}
				storedDot, delta, err := unmarshalChange(encoded, maxMessageBytes, maxActorBytes)
				if err != nil || storedDot != dot {
					return ErrCorruptStore
				}
				change, err := replica.NewChangeWithPolicy(manifest, storedDot, delta, policy)
				if err != nil {
					return ErrCorruptStore
				}
				events = append(events, Event{Sequence: sequence, Change: change})
				usedBytes += uint64(len(encoded))
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrReplayUnavailable) || errors.Is(err, ErrCorruptStore) || errors.Is(err, ErrInvalidConfig) {
			return nil, 0, err
		}
		return nil, 0, fmt.Errorf("state-vector catch-up: %w", err)
	}
	sort.Slice(events, func(left, right int) bool { return events[left].Sequence < events[right].Sequence })
	return events, highWater, nil
}

// MerkleSnapshot returns a complete bounded HLC/Merkle view of one durable
// group. It verifies every retained event before it contributes to the root;
// an absent HLC tag or index is unsafe because a receiver could otherwise
// accept a root that does not name all replayable events.
func (store *Store) MerkleSnapshot(groupID string, maxLeaves, maxBytes uint64, manifest replica.Manifest, policy crdt.ProtocolPolicy, maxMessageBytes, maxActorBytes int) (MerkleSnapshot, error) {
	if store == nil || store.db == nil || groupID == "" || maxLeaves == 0 || maxBytes == 0 || maxMessageBytes <= 0 || maxActorBytes <= 0 {
		return MerkleSnapshot{}, ErrInvalidConfig
	}
	if !store.MerkleEnabled() {
		return MerkleSnapshot{}, ErrAntiEntropyUnavailable
	}
	var snapshot MerkleSnapshot
	err := store.db.View(func(transaction *bolt.Tx) error {
		group, err := store.groupBucket(transaction, groupID, false)
		if err != nil {
			return err
		}
		tree := merkle.NewTree()
		if group == nil {
			snapshot.Root = tree.Root()
			return nil
		}
		events := group.Bucket(bucketEvents)
		hlcs := group.Bucket(bucketHLCs)
		hlcIndex := group.Bucket(bucketHLCIdx)
		meta := group.Bucket(bucketMeta)
		if events == nil || hlcs == nil || hlcIndex == nil || meta == nil {
			return ErrAntiEntropyUnavailable
		}
		highWater, _, _, err := readMeta(meta)
		if err != nil {
			return err
		}
		var usedBytes uint64
		for sequence := uint64(1); sequence <= highWater; sequence++ {
			encoded := events.Get(sequenceKey(sequence))
			tagData := hlcs.Get(sequenceKey(sequence))
			if encoded == nil || tagData == nil || uint64(len(snapshot.Leaves)) >= maxLeaves {
				return ErrAntiEntropyUnavailable
			}
			tag, err := relayHLCState(tagData, store.hlcReplicaID, maxActorBytes)
			if err != nil {
				return err
			}
			dot, delta, err := unmarshalChange(encoded, maxMessageBytes, maxActorBytes)
			if err != nil {
				return ErrCorruptStore
			}
			change, err := replica.NewChangeWithPolicy(manifest, dot, delta, policy)
			if err != nil {
				return ErrCorruptStore
			}
			event := Event{Sequence: sequence, HLC: tag, Change: change}
			leaf, err := merkleLeafForEvent(event)
			if err != nil || merkleLeafBytes(leaf) > maxBytes-usedBytes {
				return ErrAntiEntropyUnavailable
			}
			if indexed := hlcIndex.Get([]byte(merkleLeafKey(tag))); indexed == nil || bytesToSequence(indexed) != sequence {
				return ErrAntiEntropyUnavailable
			}
			value, err := merkleLeafValue(event)
			if err != nil {
				return ErrCorruptStore
			}
			tree.Insert(merkleLeafKey(tag), value)
			snapshot.Leaves = append(snapshot.Leaves, leaf)
			usedBytes += merkleLeafBytes(leaf)
			snapshot.HLC = tag
		}
		snapshot.HighWater = highWater
		snapshot.Root = tree.Root()
		sortMerkleLeaves(snapshot.Leaves)
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrInvalidConfig) || errors.Is(err, ErrCorruptStore) || errors.Is(err, ErrAntiEntropyUnavailable) || errors.Is(err, ErrClosed) {
			return MerkleSnapshot{}, err
		}
		return MerkleSnapshot{}, fmt.Errorf("read durable HLC/Merkle snapshot: %w", err)
	}
	return snapshot, nil
}

// MerkleEvents returns the complete requested event set in canonical HLC
// order. A request for an unknown HLC identity fails closed: partial repair
// would let a client attest to a root it cannot actually reconstruct.
func (store *Store) MerkleEvents(groupID string, identities []crdt.Tag, maxEvents, maxBytes uint64, manifest replica.Manifest, policy crdt.ProtocolPolicy, maxMessageBytes, maxActorBytes int) ([]Event, error) {
	if store == nil || store.db == nil || groupID == "" || maxEvents == 0 || maxBytes == 0 || maxMessageBytes <= 0 || maxActorBytes <= 0 {
		return nil, ErrInvalidConfig
	}
	if !store.MerkleEnabled() {
		return nil, ErrAntiEntropyUnavailable
	}
	if err := validateMerkleIdentityRequest(identities, maxEvents, maxBytes, maxActorBytes); err != nil {
		return nil, ErrInvalidConfig
	}
	if len(identities) == 0 {
		return nil, nil
	}
	var events []Event
	err := store.db.View(func(transaction *bolt.Tx) error {
		group, err := store.groupBucket(transaction, groupID, false)
		if err != nil {
			return err
		}
		if group == nil {
			return ErrAntiEntropyUnavailable
		}
		eventBucket := group.Bucket(bucketEvents)
		hlcs := group.Bucket(bucketHLCs)
		hlcIndex := group.Bucket(bucketHLCIdx)
		if eventBucket == nil || hlcs == nil || hlcIndex == nil {
			return ErrAntiEntropyUnavailable
		}
		var usedBytes uint64
		for _, identity := range identities {
			sequenceValue := hlcIndex.Get([]byte(merkleLeafKey(identity)))
			if sequenceValue == nil {
				return ErrAntiEntropyUnavailable
			}
			sequence := bytesToSequence(sequenceValue)
			encoded := eventBucket.Get(sequenceKey(sequence))
			tagData := hlcs.Get(sequenceKey(sequence))
			if sequence == 0 || encoded == nil || tagData == nil || uint64(len(encoded)) > maxBytes-usedBytes || len(encoded) > maxMessageBytes {
				return ErrAntiEntropyUnavailable
			}
			tag, err := relayHLCState(tagData, store.hlcReplicaID, maxActorBytes)
			if err != nil || tag.Compare(identity) != 0 {
				return ErrAntiEntropyUnavailable
			}
			dot, delta, err := unmarshalChange(encoded, maxMessageBytes, maxActorBytes)
			if err != nil {
				return ErrCorruptStore
			}
			change, err := replica.NewChangeWithPolicy(manifest, dot, delta, policy)
			if err != nil {
				return ErrCorruptStore
			}
			event := Event{Sequence: sequence, HLC: tag, Change: change}
			wire, err := marshalMerkleEvent(event)
			if err != nil || len(wire) > maxMessageBytes {
				return ErrAntiEntropyUnavailable
			}
			events = append(events, event)
			usedBytes += uint64(len(encoded))
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrInvalidConfig) || errors.Is(err, ErrCorruptStore) || errors.Is(err, ErrAntiEntropyUnavailable) || errors.Is(err, ErrClosed) {
			return nil, err
		}
		return nil, fmt.Errorf("read durable HLC/Merkle events: %w", err)
	}
	return events, nil
}

func (store *Store) ensureActorIndex(groupID string) error {
	if store == nil || store.db == nil || groupID == "" {
		return ErrInvalidConfig
	}
	if store.closed.Load() {
		return ErrClosed
	}
	err := store.db.Update(func(transaction *bolt.Tx) error {
		group, err := store.groupBucket(transaction, groupID, false)
		if err != nil || group == nil {
			return err
		}
		meta := group.Bucket(bucketMeta)
		events := group.Bucket(bucketEvents)
		if meta == nil || events == nil {
			return ErrCorruptStore
		}
		if bytes.Equal(meta.Get(keyActorIndex), []byte{1}) {
			return nil
		}
		if group.Bucket(bucketActors) != nil {
			if err := group.DeleteBucket(bucketActors); err != nil {
				return err
			}
		}
		actors, err := group.CreateBucket(bucketActors)
		if err != nil {
			return err
		}
		highWater, _, _, err := readMeta(meta)
		if err != nil {
			return err
		}
		cursor := events.Cursor()
		key, value := cursor.First()
		for expected := uint64(1); expected <= highWater; expected++ {
			if key == nil || bytesToSequence(key) != expected || value == nil {
				return ErrCorruptStore
			}
			dot, _, err := unmarshalChange(value, len(value), frame.DefaultLimits().MaxStringBytes)
			if err != nil {
				return ErrCorruptStore
			}
			maximum, err := readUint64(actors.Get([]byte(dot.Actor)))
			if err != nil {
				return err
			}
			if dot.Counter > maximum {
				if err := actors.Put([]byte(dot.Actor), sequenceKey(dot.Counter)); err != nil {
					return err
				}
			}
			key, value = cursor.Next()
		}
		return meta.Put(keyActorIndex, []byte{1})
	})
	if err != nil {
		if errors.Is(err, ErrCorruptStore) || errors.Is(err, ErrInvalidConfig) || errors.Is(err, ErrClosed) {
			return err
		}
		return fmt.Errorf("build durable actor index: %w", err)
	}
	return nil
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
	for _, name := range [][]byte{bucketEvents, bucketDots, bucketActors, bucketHLCs, bucketHLCIdx, bucketMeta} {
		if _, err := group.CreateBucketIfNotExists(name); err != nil {
			return nil, err
		}
	}
	return group, nil
}

func dotPrefix(actor string) []byte {
	prefix := frame.AppendUvarint(nil, uint64(len(actor)))
	return append(prefix, actor...)
}

func dotFromKey(key []byte) (replica.Dot, error) {
	actor, position, ok := frame.ReadBytes(key, 0, frame.DefaultLimits().MaxStringBytes)
	if !ok || len(key)-position != 8 {
		return replica.Dot{}, ErrCorruptStore
	}
	counter := binary.BigEndian.Uint64(key[position:])
	dot := replica.Dot{Actor: string(actor), Counter: counter}
	if strings.TrimSpace(dot.Actor) == "" || counter == 0 {
		return replica.Dot{}, ErrCorruptStore
	}
	return dot, nil
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
