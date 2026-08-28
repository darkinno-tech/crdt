// Package redis provides a Redis-backed durable relay operation log. Redis is
// only durable to the extent configured by the deployment (AOF/fsync, replica
// acknowledgement, TLS, ACLs, and backup policy are application concerns).
package redis

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/durable"
	"github.com/darkinno-tech/crdt/replica"
	redisclient "github.com/redis/go-redis/v9"
)

const maxLuaExactInteger uint64 = 1<<53 - 1

var ErrSequenceRange = errors.New("crdt redis provider: sequence exceeds Lua exact integer range")

// Config bounds retained data per group. Prefix must identify one application
// namespace. The provider hashes group IDs into a Redis Cluster hash tag so
// each group's atomic script keys share one slot.
type Config struct {
	Prefix    string
	MaxEvents uint64
	MaxBytes  uint64
	Timeout   time.Duration
}

// Store implements durable.Log. It never closes the supplied UniversalClient.
type Store struct {
	client    redisclient.UniversalClient
	prefix    string
	maxEvents uint64
	maxBytes  uint64
	timeout   time.Duration
	closed    atomic.Bool
}

func New(client redisclient.UniversalClient, config Config) (*Store, error) {
	if client == nil || strings.TrimSpace(config.Prefix) == "" || config.Prefix != strings.TrimSpace(config.Prefix) || config.MaxEvents == 0 || config.MaxBytes == 0 || config.MaxEvents > maxLuaExactInteger || config.MaxBytes > maxLuaExactInteger || config.Timeout < 0 {
		return nil, durable.ErrInvalidConfig
	}
	if config.Timeout == 0 {
		config.Timeout = 5 * time.Second
	}
	return &Store{client: client, prefix: config.Prefix, maxEvents: config.MaxEvents, maxBytes: config.MaxBytes, timeout: config.Timeout}, nil
}

func (store *Store) Close() error {
	if store == nil || store.client == nil || !store.closed.CompareAndSwap(false, true) {
		return durable.ErrClosed
	}
	return nil
}

func (store *Store) Closed() bool {
	return store == nil || store.client == nil || store.closed.Load()
}

// Append uses one bounded EVAL invocation so the Dot binding, capacity check,
// next sequence, envelope, and metadata change atomically. Redis Lua numbers
// cannot represent every uint64, hence the explicit 2^53-1 sequence ceiling.
func (store *Store) Append(groupID string, change replica.Change) (durable.AppendResult, error) {
	if store.Closed() {
		return durable.AppendResult{}, durable.ErrClosed
	}
	if strings.TrimSpace(groupID) == "" {
		return durable.AppendResult{}, durable.ErrInvalidConfig
	}
	encoded, err := durable.EncodeChange(change)
	if err != nil {
		return durable.AppendResult{}, err
	}
	if uint64(len(encoded)) > store.maxBytes {
		return durable.AppendResult{}, durable.ErrStoreFull
	}
	digest := sha256.Sum256(encoded)
	ctx, cancel := context.WithTimeout(context.Background(), store.timeout)
	defer cancel()
	result, err := appendScript.Run(ctx, store.client, store.keys(groupID), groupID, dotKey(change.Dot), fmt.Sprintf("%x", digest), encoded, strconv.FormatUint(store.maxEvents, 10), strconv.FormatUint(store.maxBytes, 10), strconv.FormatUint(maxLuaExactInteger, 10)).Result()
	if err != nil {
		return durable.AppendResult{}, fmt.Errorf("append Redis durable event: %w", err)
	}
	status, sequence, err := scriptResult(result)
	if err != nil {
		return durable.AppendResult{}, durable.ErrCorruptStore
	}
	switch status {
	case 0:
		return durable.AppendResult{Event: durable.Event{Sequence: sequence, Change: change}}, nil
	case 1:
		return durable.AppendResult{Event: durable.Event{Sequence: sequence, Change: change}, Duplicate: true}, nil
	case 2:
		return durable.AppendResult{}, durable.ErrConflictingDot
	case 3:
		return durable.AppendResult{}, durable.ErrStoreFull
	case 4:
		return durable.AppendResult{}, ErrSequenceRange
	default:
		return durable.AppendResult{}, durable.ErrCorruptStore
	}
}

// Replay verifies the complete bounded suffix after reading the high-water
// mark. Events are append-only, so a concurrent later append cannot alter the
// suffix at or below that mark.
func (store *Store) Replay(groupID string, after, maxEvents, maxBytes uint64, manifest replica.Manifest, policy crdt.ProtocolPolicy, maxMessageBytes, maxActorBytes int) ([]durable.Event, uint64, error) {
	if store.Closed() {
		return nil, 0, durable.ErrClosed
	}
	if strings.TrimSpace(groupID) == "" || maxEvents == 0 || maxBytes == 0 || maxEvents > maxLuaExactInteger || maxBytes > maxLuaExactInteger || maxMessageBytes <= 0 || maxActorBytes <= 0 {
		return nil, 0, durable.ErrInvalidConfig
	}
	ctx, cancel := context.WithTimeout(context.Background(), store.timeout)
	defer cancel()
	keys := store.keys(groupID)
	metadata, err := store.client.HMGet(ctx, keys[0], "group_id", "high_water", "event_count", "used_bytes").Result()
	if err != nil {
		return nil, 0, fmt.Errorf("read Redis durable metadata: %w", err)
	}
	if allNil(metadata) {
		if after != 0 {
			return nil, 0, durable.ErrReplayUnavailable
		}
		return nil, 0, nil
	}
	if len(metadata) != 4 || stringValue(metadata[0]) != groupID {
		return nil, 0, durable.ErrCorruptStore
	}
	highWater, okHigh := uintValue(metadata[1])
	count, okCount := uintValue(metadata[2])
	used, okUsed := uintValue(metadata[3])
	if !okHigh || !okCount || !okUsed || highWater < count || highWater > maxLuaExactInteger || count > store.maxEvents || used > store.maxBytes || after > highWater || highWater-after > maxEvents {
		return nil, 0, durable.ErrReplayUnavailable
	}
	if after == highWater {
		return nil, highWater, nil
	}
	fields := make([]string, 0, highWater-after)
	for sequence := after + 1; sequence <= highWater; sequence++ {
		fields = append(fields, strconv.FormatUint(sequence, 10))
	}
	encodedEvents, err := store.client.HMGet(ctx, keys[2], fields...).Result()
	if err != nil || len(encodedEvents) != len(fields) {
		return nil, 0, durable.ErrCorruptStore
	}
	events := make([]durable.Event, 0, len(encodedEvents))
	var replayedBytes uint64
	for index, item := range encodedEvents {
		var encoded []byte
		switch value := item.(type) {
		case []byte:
			encoded = append([]byte(nil), value...)
		case string:
			encoded = []byte(value)
		default:
			return nil, 0, durable.ErrCorruptStore
		}
		ok := encoded != nil
		if !ok || uint64(len(encoded)) > maxBytes-replayedBytes || len(encoded) > maxMessageBytes {
			return nil, 0, durable.ErrCorruptStore
		}
		dot, delta, err := durable.DecodeChange(encoded, maxMessageBytes, maxActorBytes)
		if err != nil {
			return nil, 0, durable.ErrCorruptStore
		}
		change, err := replica.NewChangeWithPolicy(manifest, dot, delta, policy)
		if err != nil {
			return nil, 0, durable.ErrCorruptStore
		}
		events = append(events, durable.Event{Sequence: after + uint64(index) + 1, Change: change})
		replayedBytes += uint64(len(encoded))
	}
	return events, highWater, nil
}

func (store *Store) keys(groupID string) []string {
	sum := sha256.Sum256([]byte(groupID))
	base := store.prefix + ":{" + fmt.Sprintf("%x", sum[:]) + "}"
	return []string{base + ":meta", base + ":dots", base + ":events"}
}

func dotKey(dot replica.Dot) string {
	return dot.Actor + "\x00" + strconv.FormatUint(dot.Counter, 10)
}

func allNil(values []interface{}) bool {
	for _, value := range values {
		if value != nil {
			return false
		}
	}
	return true
}

func stringValue(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case int:
		return strconv.Itoa(typed)
	default:
		return ""
	}
}

func uintValue(value interface{}) (uint64, bool) {
	parsed, err := strconv.ParseUint(stringValue(value), 10, 64)
	return parsed, err == nil
}

func scriptResult(value interface{}) (int64, uint64, error) {
	items, ok := value.([]interface{})
	if !ok || len(items) != 2 {
		return 0, 0, errors.New("invalid Redis script response")
	}
	status, err := strconv.ParseInt(stringValue(items[0]), 10, 64)
	if err != nil {
		return 0, 0, err
	}
	sequence, ok := uintValue(items[1])
	if !ok {
		return 0, 0, errors.New("invalid Redis sequence")
	}
	return status, sequence, nil
}

var appendScript = redisclient.NewScript(`
local meta, dots, events = KEYS[1], KEYS[2], KEYS[3]
local group, dot, digest, envelope = ARGV[1], ARGV[2], ARGV[3], ARGV[4]
local max_events, max_bytes, max_sequence = tonumber(ARGV[5]), tonumber(ARGV[6]), ARGV[7]
local bound_group = redis.call('HGET', meta, 'group_id')
if bound_group and bound_group ~= group then return {5, '0'} end
local existing = redis.call('HGET', dots, dot)
if existing then
  local divider = string.find(existing, ':', 1, true)
  if not divider then return {5, '0'} end
  local sequence = string.sub(existing, 1, divider - 1)
  local existing_digest = string.sub(existing, divider + 1)
  if existing_digest == digest then return {1, sequence} end
  return {2, '0'}
end
local high = redis.call('HGET', meta, 'high_water') or '0'
local count = tonumber(redis.call('HGET', meta, 'event_count') or '0')
local used = tonumber(redis.call('HGET', meta, 'used_bytes') or '0')
if high == max_sequence then return {4, '0'} end
if count >= max_events or used > max_bytes - string.len(envelope) then return {3, '0'} end
local sequence = redis.call('HINCRBY', meta, 'high_water', 1)
redis.call('HSET', meta, 'group_id', group, 'event_count', count + 1, 'used_bytes', used + string.len(envelope))
redis.call('HSET', dots, dot, tostring(sequence) .. ':' .. digest)
redis.call('HSET', events, tostring(sequence), envelope)
return {0, tostring(sequence)}
`)

var _ durable.Log = (*Store)(nil)
