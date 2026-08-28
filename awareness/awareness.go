// Package awareness implements bounded, ephemeral presence state for a
// collaboration group.
//
// Awareness is deliberately separate from durable CRDT state: it must never
// be checkpointed, included in a replica.Frontier, or used as an authorization
// decision. Each actor owns a monotonically increasing clock. A removal keeps
// its clock as an in-memory tombstone so a delayed older update cannot bring a
// disconnected actor back online.
package awareness

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	frame "github.com/darkinno-tech/crdt/encoding"
)

var (
	ErrInvalidOptions = errors.New("awareness: invalid options")
	ErrInvalidActor   = errors.New("awareness: invalid actor")
	ErrInvalidState   = errors.New("awareness: invalid state")
	ErrInvalidUpdate  = errors.New("awareness: invalid update")
	ErrResourceLimit  = errors.New("awareness: resource limit exceeded")
	ErrStateConflict  = errors.New("awareness: conflicting update clock")
	ErrClockExhausted = errors.New("awareness: actor clock exhausted")
	ErrOfflineActor   = errors.New("awareness: actor has no online state")
)

const protocolVersion byte = 1

// Options sets the resource and liveness boundaries for one awareness group.
// MaxStateBytes covers a single JSON object, not the aggregate application
// session. Timeout is evaluated by ActiveAt; applications should publish a
// strictly newer heartbeat before it elapses.
type Options struct {
	MaxActors     int
	MaxActorBytes int
	MaxStateBytes int
	// MaxSubscribers bounds local UI observers. It has no wire effect and
	// defaults to 1,024 when omitted from an otherwise valid Options value.
	MaxSubscribers int
	Timeout        time.Duration
}

// DefaultOptions returns conservative limits for UI presence such as names,
// colours, selections, and small cursor metadata.
func DefaultOptions() Options {
	return Options{
		MaxActors:      1 << 14,
		MaxActorBytes:  128,
		MaxStateBytes:  16 << 10,
		MaxSubscribers: 1 << 10,
		Timeout:        30 * time.Second,
	}
}

func (options Options) valid() bool {
	return options.MaxActors > 0 && options.MaxActorBytes > 0 && options.MaxStateBytes > 0 && options.MaxSubscribers > 0 && options.Timeout > 0
}

func normalizeOptions(options Options) Options {
	if options.MaxSubscribers == 0 {
		options.MaxSubscribers = DefaultOptions().MaxSubscribers
	}
	return options
}

// Update is one actor's complete ephemeral state at Clock. A nil State is a
// removal. State is an opaque canonical JSON object so the owning application
// can define fields without making presence a durable document schema.
type Update struct {
	Actor string
	Clock uint64
	State []byte
}

// Online reports whether update carries a presence object rather than a
// removal tombstone.
func (update Update) Online() bool { return update.State != nil }

// MarshalBinary serializes a self-contained awareness-v1 update using bounded
// canonical varints. It authenticates nothing; transports must authorize the
// actor against their authenticated peer before relaying it.
func (update Update) MarshalBinary() ([]byte, error) {
	return update.MarshalBinaryWithOptions(DefaultOptions())
}

// MarshalBinaryWithOptions serializes update after validating and canonicalizing
// its actor and optional JSON object.
func (update Update) MarshalBinaryWithOptions(options Options) ([]byte, error) {
	normalized, err := Normalize(update, options)
	if err != nil {
		return nil, err
	}
	encoded := make([]byte, 0, 2+len(normalized.Actor)+len(normalized.State)+30)
	encoded = append(encoded, protocolVersion)
	encoded = frame.AppendUvarint(encoded, uint64(len(normalized.Actor)))
	encoded = append(encoded, normalized.Actor...)
	encoded = frame.AppendUvarint(encoded, normalized.Clock)
	if !normalized.Online() {
		return append(encoded, 0), nil
	}
	encoded = append(encoded, 1)
	encoded = frame.AppendUvarint(encoded, uint64(len(normalized.State)))
	return append(encoded, normalized.State...), nil
}

// UnmarshalUpdate decodes one exact, bounded awareness-v1 update. It performs
// all limits and JSON validation before allocating retained state.
func UnmarshalUpdate(data []byte, options Options) (Update, error) {
	options = normalizeOptions(options)
	if !options.valid() {
		return Update{}, ErrInvalidOptions
	}
	if len(data) < 4 || data[0] != protocolVersion {
		return Update{}, ErrInvalidUpdate
	}
	actor, position, ok := frame.ReadBytes(data, 1, options.MaxActorBytes)
	if !ok {
		return Update{}, ErrInvalidUpdate
	}
	clock, position, ok := frame.ReadUvarint(data, position)
	if !ok || clock == 0 || position >= len(data) {
		return Update{}, ErrInvalidUpdate
	}
	status := data[position]
	position++
	update := Update{Actor: string(actor), Clock: clock}
	switch status {
	case 0:
		if position != len(data) {
			return Update{}, ErrInvalidUpdate
		}
	case 1:
		state, next, ok := frame.ReadBytes(data, position, options.MaxStateBytes)
		if !ok || next != len(data) {
			return Update{}, ErrInvalidUpdate
		}
		update.State = append([]byte(nil), state...)
	default:
		return Update{}, ErrInvalidUpdate
	}
	return Normalize(update, options)
}

// Normalize returns a copied update whose online JSON state has a deterministic
// representation. It reserves nil for a removal and rejects every other JSON
// top-level value, keeping presence fields namespaced beneath an object.
func Normalize(update Update, options Options) (Update, error) {
	options = normalizeOptions(options)
	if !options.valid() {
		return Update{}, ErrInvalidOptions
	}
	if strings.TrimSpace(update.Actor) == "" || !utf8.ValidString(update.Actor) || len(update.Actor) > options.MaxActorBytes {
		return Update{}, ErrInvalidActor
	}
	if update.Clock == 0 {
		return Update{}, ErrInvalidUpdate
	}
	normalized := Update{Actor: update.Actor, Clock: update.Clock}
	if update.State == nil {
		return normalized, nil
	}
	if len(update.State) == 0 || len(update.State) > options.MaxStateBytes {
		return Update{}, ErrResourceLimit
	}
	state, err := canonicalState(update.State)
	if err != nil {
		return Update{}, err
	}
	if len(state) > options.MaxStateBytes {
		return Update{}, ErrResourceLimit
	}
	normalized.State = state
	return normalized, nil
}

func canonicalState(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, ErrInvalidState
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, ErrInvalidState
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidState
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, ErrInvalidState
	}
	return canonical, nil
}

type record struct {
	update   Update
	lastSeen time.Time
	expired  bool
}

// Store accepts locally created and remotely received updates. It is safe for
// concurrent UI, timer, and transport goroutines. Its state is intentionally
// process-local and must not be persisted with CRDT data.
type Store struct {
	mu      sync.RWMutex
	options Options
	records map[string]record
	version uint64
	hub     observationHub
}

// NewStore constructs an empty bounded awareness store.
func NewStore(options Options) (*Store, error) {
	options = normalizeOptions(options)
	if !options.valid() {
		return nil, ErrInvalidOptions
	}
	return &Store{
		options: options,
		records: make(map[string]record),
		hub: observationHub{
			subscribers: make(map[uint64]*subscriber),
			max:         options.MaxSubscribers,
		},
	}, nil
}

// Set creates and installs the next online update for actor. The caller should
// publish the returned update and periodically call Set again as a heartbeat.
func (store *Store) Set(actor string, state []byte, now time.Time) (Update, error) {
	return store.next(actor, state, now)
}

// Heartbeat creates and installs the next online update without re-parsing an
// unchanged state object. Call it only for an actor owned by this local
// application; transports must still authorize that actor before relaying the
// returned update. A removed or unknown actor must use Set to establish its
// state again.
func (store *Store) Heartbeat(actor string, now time.Time) (Update, error) {
	if store == nil {
		return Update{}, ErrInvalidOptions
	}
	if _, err := Normalize(Update{Actor: actor, Clock: 1}, store.options); err != nil {
		return Update{}, err
	}
	now = normalizeTime(now)
	store.mu.Lock()
	current, exists := store.records[actor]
	if !exists || !current.update.Online() {
		store.mu.Unlock()
		return Update{}, ErrOfflineActor
	}
	if current.update.Clock == math.MaxUint64 {
		store.mu.Unlock()
		return Update{}, ErrClockExhausted
	}
	update := store.heartbeatLocked(actor, current, now)
	store.mu.Unlock()
	return update, nil
}

// Remove creates and installs the next removal update for actor. It is useful
// on a graceful disconnect; abrupt disconnects are handled by ActiveAt's TTL.
func (store *Store) Remove(actor string, now time.Time) (Update, error) {
	return store.next(actor, nil, now)
}

func (store *Store) next(actor string, state []byte, now time.Time) (Update, error) {
	if store == nil {
		return Update{}, ErrInvalidOptions
	}
	now = normalizeTime(now)
	store.mu.Lock()
	clock := uint64(1)
	if current, exists := store.records[actor]; exists {
		if current.update.Clock == math.MaxUint64 {
			store.mu.Unlock()
			return Update{}, ErrClockExhausted
		}
		if current.update.Online() && bytes.Equal(state, current.update.State) {
			update := store.heartbeatLocked(actor, current, now)
			store.mu.Unlock()
			return update, nil
		}
		clock = current.update.Clock + 1
	}
	update, err := Normalize(Update{Actor: actor, Clock: clock, State: state}, store.options)
	if err != nil {
		return Update{}, err
	}
	if _, exists := store.records[actor]; !exists && len(store.records) >= store.options.MaxActors {
		store.mu.Unlock()
		return Update{}, ErrResourceLimit
	}
	store.records[actor] = record{update: update, lastSeen: now}
	store.publishLocked(Local, update, now)
	store.mu.Unlock()
	return cloneUpdate(update), nil
}

// heartbeatLocked advances one retained online state without reparsing its
// canonical JSON. store.mu must be held. The stored state is private to Store,
// while the returned update is copied for the caller.
func (store *Store) heartbeatLocked(actor string, current record, now time.Time) Update {
	update := current.update
	update.Clock++
	store.records[actor] = record{update: update, lastSeen: now}
	store.publishLocked(Local, update, now)
	return cloneUpdate(update)
}

// Apply installs a newer remote update. Duplicates and stale updates are
// harmless and return changed=false. A different payload at an equal clock is
// rejected so arrival order cannot determine a user's displayed presence.
func (store *Store) Apply(update Update, now time.Time) (changed bool, err error) {
	if store == nil {
		return false, ErrInvalidOptions
	}
	normalized, err := Normalize(update, store.options)
	if err != nil {
		return false, err
	}
	now = normalizeTime(now)
	store.mu.Lock()
	current, exists := store.records[normalized.Actor]
	if !exists {
		if len(store.records) >= store.options.MaxActors {
			store.mu.Unlock()
			return false, ErrResourceLimit
		}
		store.records[normalized.Actor] = record{update: normalized, lastSeen: now}
		store.publishLocked(Remote, normalized, now)
		store.mu.Unlock()
		return true, nil
	}
	if normalized.Clock < current.update.Clock {
		store.mu.Unlock()
		return false, nil
	}
	if normalized.Clock == current.update.Clock {
		if !sameUpdate(normalized, current.update) {
			store.mu.Unlock()
			return false, ErrStateConflict
		}
		store.mu.Unlock()
		return false, nil
	}
	store.records[normalized.Actor] = record{update: normalized, lastSeen: now}
	store.publishLocked(Remote, normalized, now)
	store.mu.Unlock()
	return true, nil
}

// ActiveAt returns sorted, owned copies of the currently live presence states.
// Offline tombstones remain retained internally until the application drops
// the whole ephemeral Store, preventing old packets from reviving an actor.
func (store *Store) ActiveAt(now time.Time) []Update {
	if store == nil {
		return nil
	}
	now = normalizeTime(now)
	store.mu.RLock()
	updates := store.activeAtLocked(now)
	store.mu.RUnlock()
	return updates
}

// Options returns the immutable store configuration.
func (store *Store) Options() Options {
	if store == nil {
		return Options{}
	}
	return store.options
}

func (store *Store) activeAtLocked(now time.Time) []Update {
	updates := make([]Update, 0, len(store.records))
	for _, current := range store.records {
		if current.update.Online() && !current.expired && now.Sub(current.lastSeen) <= store.options.Timeout {
			updates = append(updates, cloneUpdate(current.update))
		}
	}
	sort.Slice(updates, func(left, right int) bool { return updates[left].Actor < updates[right].Actor })
	return updates
}

func normalizeTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now()
	}
	return value
}

func cloneUpdate(update Update) Update {
	return Update{Actor: update.Actor, Clock: update.Clock, State: append([]byte(nil), update.State...)}
}

func sameUpdate(left, right Update) bool {
	return left.Actor == right.Actor && left.Clock == right.Clock && bytes.Equal(left.State, right.State)
}
