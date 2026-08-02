package extensions

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// YJSMessageKind identifies the message class presented to YJSAuthorize. It
// intentionally does not expose document or awareness payloads to an
// authorization callback.
type YJSMessageKind uint8

const (
	// YJSUpdate is a Yjs sync-step-2 or update payload.
	YJSUpdate YJSMessageKind = iota + 1
	// YJSAwareness is an ephemeral y-protocols awareness payload.
	YJSAwareness
	yjsAwarenessQuery
	yjsSyncStep1
)

// YJSAuthorize authorizes publication to one configured room. Yjs update
// bytes are opaque to this relay: authorization belongs to the room and
// authenticated identity, never to a client-selected Yjs client ID.
type YJSAuthorize func(Peer, string, YJSMessageKind) error

// YJSAuthorizeSubscription authorizes access to a configured room separately
// from publication.
type YJSAuthorizeSubscription func(Peer, string) error

// YJSRoomConfig configures one explicitly named y-protocols room. Without
// Store, a room retains complete opaque updates only to bootstrap later live
// peers. With Store, it becomes a Level 1 Yjs room: the store owns semantic
// update admission, state-vector recovery, and durable snapshots while this
// handler remains responsible for client authentication and authorization.
type YJSRoomConfig struct {
	Name            string
	MaxUpdateBytes  int
	MaxHistoryBytes int
	MaxUpdates      int
	// MaxAwarenessTombstones bounds clock-only metadata retained after a
	// client becomes offline. Zero uses a conservative default. It never
	// retains awareness JSON and prevents delayed state resurrection.
	MaxAwarenessTombstones int
	// MaxStateVectorBytes and MaxSyncBytes are required with Store. They bound
	// the semantic handshake independently from one client-authored update.
	MaxStateVectorBytes int
	MaxSyncBytes        int
	// Store is optional. When set, Document must be a valid immutable identity
	// for this configured room, and MaxHistoryBytes/MaxUpdates are ignored: no
	// opaque update history is retained in the Go process.
	Store    YJSStore
	Document YJSDocument
}

// YJSRoom is a bounded, opaque Yjs update cache and live subscriber set.
// It is intentionally isolated from Group: y-protocols and this module's
// framed CRDT protocols have incompatible state and recovery semantics.
type YJSRoom struct {
	name                   string
	maxUpdateBytes         int
	maxHistoryBytes        int
	maxUpdates             int
	maxStateVectorBytes    int
	maxSyncBytes           int
	maxAwarenessTombstones int
	store                  YJSStore
	document               YJSDocument

	// storeMu makes a state-vector bootstrap atomic with respect to an Apply.
	// It never holds mu while a sidecar call is in flight, except for the short
	// peer-map mutation after the semantic state has been observed.
	storeMu   sync.Mutex
	mu        sync.Mutex
	updates   [][]byte
	hashes    map[[sha256.Size]byte]struct{}
	history   int
	peers     map[*yjsSubscriber]Peer
	awareness map[uint64]yjsAwarenessState
	// awarenessTombstones retains just enough protocol metadata to reject a
	// delayed pre-removal awareness state. It never retains awareness JSON and
	// is capped independently of the live-state limit.
	awarenessTombstones map[uint64]yjsAwarenessTombstone
}

type yjsAwarenessState struct {
	clock uint64
	state []byte // Canonical non-null JSON object text.
	owner yjsAwarenessOwner
}

// yjsAwarenessOwner binds a client-selected Yjs client ID to one WebSocket,
// rather than to a reusable authenticated principal. One user can have two
// browser tabs; closing either one must not remove the other tab's presence.
// Direct room tests have no subscriber and deliberately use peer as a
// deterministic fallback owner.
type yjsAwarenessOwner struct {
	subscriber *yjsSubscriber
	peer       string
}

type yjsAwarenessTombstone struct {
	clock     uint64
	owner     yjsAwarenessOwner
	removedAt time.Time
}

// NewYJSRoom creates one room. The zero value is deliberately not usable:
// room names and every retained-resource boundary must be selected by the
// embedding application.
func NewYJSRoom(config YJSRoomConfig) (*YJSRoom, error) {
	if strings.TrimSpace(config.Name) != config.Name || config.Name == "" || strings.Contains(config.Name, "/") || config.MaxUpdateBytes <= 0 ||
		config.MaxAwarenessTombstones < 0 || config.MaxAwarenessTombstones > maxYJSAwarenessClients {
		return nil, invalidConfig("extensions.new_yjs_room", ErrInvalidConfig)
	}
	if config.Store == nil && (config.MaxHistoryBytes < config.MaxUpdateBytes || config.MaxUpdates <= 0) {
		return nil, invalidConfig("extensions.new_yjs_room", ErrInvalidConfig)
	}
	if config.Store != nil && (config.MaxStateVectorBytes <= 0 || config.MaxSyncBytes < config.MaxUpdateBytes ||
		!validYJSStoreIdentifier(config.Document.Tenant) || !validYJSStoreIdentifier(config.Document.Room) || !validYJSStoreIdentifier(config.Document.Schema) ||
		(config.Document.Format != YJSStoreFormatV1 && config.Document.Format != YJSStoreFormatV2)) {
		return nil, invalidConfig("extensions.new_yjs_room", ErrInvalidConfig)
	}
	maxAwarenessTombstones := config.MaxAwarenessTombstones
	if maxAwarenessTombstones == 0 {
		maxAwarenessTombstones = defaultMaxYJSAwarenessTombstones
	}
	return &YJSRoom{
		name:                   config.Name,
		maxUpdateBytes:         config.MaxUpdateBytes,
		maxHistoryBytes:        config.MaxHistoryBytes,
		maxUpdates:             config.MaxUpdates,
		maxStateVectorBytes:    config.MaxStateVectorBytes,
		maxSyncBytes:           config.MaxSyncBytes,
		maxAwarenessTombstones: maxAwarenessTombstones,
		store:                  config.Store,
		document:               config.Document,
		peers:                  make(map[*yjsSubscriber]Peer),
		hashes:                 make(map[[sha256.Size]byte]struct{}),
		awareness:              make(map[uint64]yjsAwarenessState),
		awarenessTombstones:    make(map[uint64]yjsAwarenessTombstone),
	}, nil
}

// Name returns the configured immutable room name.
func (room *YJSRoom) Name() string {
	if room == nil {
		return ""
	}
	return room.name
}

// YJSConfig configures a y-websocket-compatible y-protocols relay. It has no
// default rooms and requires authentication plus independent read/write
// authorization. The transport is live-only except for each room's explicitly
// bounded in-memory update history.
type YJSConfig struct {
	Rooms                 []*YJSRoom
	Authenticate          Authenticate
	Authorize             YJSAuthorize
	AuthorizeSubscription YJSAuthorizeSubscription
	OriginPatterns        []string
	MaxMessageBytes       int
	MaxQueuedMessages     int
	MaxQueuedBytes        int
	MaxAwarenessClients   int
	HandshakeTimeout      time.Duration
	// StoreTimeout bounds one sidecar Apply or Diff after a WebSocket is live.
	// It is distinct from the initial WebSocket handshake timeout.
	StoreTimeout time.Duration
	WriteTimeout time.Duration
}

// YJSHandler serves y-websocket-compatible paths. Mount it below a prefix;
// the final single path element selects a configured room, e.g. /yjs/notes.
// It accepts the standard y-protocols sync (0), awareness (1), and awareness
// query (3) messages. Authentication and permissions are application-owned.
type YJSHandler struct {
	rooms                 map[string]*YJSRoom
	authenticate          Authenticate
	authorize             YJSAuthorize
	authorizeSubscription YJSAuthorizeSubscription
	origins               []string
	maxMessageBytes       int
	maxQueuedMessages     int
	maxQueuedBytes        int
	maxAwarenessClients   int
	handshakeTimeout      time.Duration
	storeTimeout          time.Duration
	writeTimeout          time.Duration
}

// NewYJSHandler validates and constructs the opt-in compatibility relay.
func NewYJSHandler(config YJSConfig) (*YJSHandler, error) {
	if config.Authenticate == nil || config.Authorize == nil || config.AuthorizeSubscription == nil || len(config.Rooms) == 0 ||
		config.MaxAwarenessClients < 0 {
		return nil, invalidConfig("extensions.new_yjs_handler", ErrInvalidConfig)
	}
	limits, err := normalizeYJSLimits(config)
	if err != nil || validateOriginPatterns(config.OriginPatterns) != nil {
		return nil, invalidConfig("extensions.new_yjs_handler", ErrInvalidConfig)
	}
	storeTimeout := config.StoreTimeout
	if storeTimeout == 0 {
		storeTimeout = defaultHandshakeTimeout
	}
	if storeTimeout <= 0 {
		return nil, invalidConfig("extensions.new_yjs_handler", ErrInvalidConfig)
	}
	maxAwarenessClients := config.MaxAwarenessClients
	if maxAwarenessClients == 0 {
		maxAwarenessClients = 256
	}
	rooms := make(map[string]*YJSRoom, len(config.Rooms))
	storeDocuments := make(map[YJSDocument]struct{}, len(config.Rooms))
	for _, room := range config.Rooms {
		if room == nil || room.name == "" ||
			(room.store == nil && room.maxUpdateBytes > limits.maxMessageBytes) ||
			(room.store != nil && (room.maxUpdateBytes > limits.maxMessageBytes-maxYJSWireOverhead || room.maxStateVectorBytes > limits.maxMessageBytes-maxYJSWireOverhead || room.maxSyncBytes > limits.maxMessageBytes-maxYJSWireOverhead)) ||
			(room.store == nil && (room.maxHistoryBytes < room.maxUpdateBytes || room.maxUpdates <= 0)) {
			return nil, invalidConfig("extensions.new_yjs_handler", ErrInvalidConfig)
		}
		if _, exists := rooms[room.name]; exists {
			return nil, invalidConfig("extensions.new_yjs_handler", ErrInvalidConfig)
		}
		if room.store != nil {
			if _, exists := storeDocuments[room.document]; exists {
				return nil, invalidConfig("extensions.new_yjs_handler", ErrInvalidConfig)
			}
			storeDocuments[room.document] = struct{}{}
		}
		rooms[room.name] = room
	}
	return &YJSHandler{
		rooms:                 rooms,
		authenticate:          config.Authenticate,
		authorize:             config.Authorize,
		authorizeSubscription: config.AuthorizeSubscription,
		origins:               append([]string(nil), config.OriginPatterns...),
		maxMessageBytes:       limits.maxMessageBytes,
		maxQueuedMessages:     limits.maxQueuedMessages,
		maxQueuedBytes:        limits.maxQueuedBytes,
		maxAwarenessClients:   maxAwarenessClients,
		handshakeTimeout:      limits.handshakeTimeout,
		storeTimeout:          storeTimeout,
		writeTimeout:          limits.writeTimeout,
	}, nil
}

// ServeHTTP serves one configured room selected by a single escaped path
// segment. Dynamic room creation is intentionally not supported: accepting an
// untrusted room name must not allocate retained server state.
func (handler *YJSHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if handler == nil {
		http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	room, ok := handler.roomForPath(request.URL.Path)
	if !ok {
		http.NotFound(writer, request)
		return
	}
	if !originAllowed(request, handler.origins) {
		http.Error(writer, "forbidden origin", http.StatusForbidden)
		return
	}
	peer, err := handler.authenticate(request)
	if err != nil || strings.TrimSpace(peer.ID) == "" {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := handler.authorizeSubscription(peer, room.name); err != nil {
		http.Error(writer, "forbidden", http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		OriginPatterns:  handler.origins,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(int64(handler.maxMessageBytes))
	subscriber := newYJSSubscriber(conn, handler.maxQueuedMessages, handler.maxQueuedBytes, handler.writeTimeout)
	handshakeContext, cancel := context.WithTimeout(request.Context(), handler.handshakeTimeout)
	defer cancel()
	bootstrap, err := room.bootstrap(handshakeContext, subscriber, peer)
	if err != nil {
		subscriber.close()
		return
	}
	defer func() {
		room.remove(subscriber, peer)
		subscriber.close()
	}()
	for _, message := range bootstrap {
		if err := conn.Write(handshakeContext, websocket.MessageBinary, message); err != nil {
			return
		}
	}
	go subscriber.writeLoop()
	handler.readLoop(subscriber, room, peer)
}

func (handler *YJSHandler) roomForPath(requestPath string) (*YJSRoom, bool) {
	if !strings.HasPrefix(requestPath, "/") || strings.Count(strings.TrimPrefix(requestPath, "/"), "/") != 0 {
		return nil, false
	}
	name, err := url.PathUnescape(strings.TrimPrefix(requestPath, "/"))
	if err != nil || name == "" || name != path.Base(name) {
		return nil, false
	}
	room, ok := handler.rooms[name]
	return room, ok
}

func (handler *YJSHandler) readLoop(subscriber *yjsSubscriber, room *YJSRoom, peer Peer) {
	for {
		messageType, data, err := subscriber.conn.Read(subscriber.context)
		if err != nil || messageType != websocket.MessageBinary {
			return
		}
		incoming, err := unmarshalYJSMessages(data, handler.maxMessageBytes, handler.maxAwarenessClients)
		if err != nil {
			return
		}
		for _, message := range incoming {
			if message.kind == YJSUpdate || message.kind == YJSAwareness {
				if err := handler.authorize(peer, room.name, message.kind); err != nil {
					return
				}
			}
		}
		for _, message := range incoming {
			switch message.kind {
			case YJSUpdate:
				storeContext, cancel := context.WithTimeout(subscriber.context, handler.storeTimeout)
				accepted := room.appendUpdateContext(storeContext, message.payload)
				cancel()
				if !accepted {
					return
				}
			case YJSAwareness:
				if !room.applyAwarenessFrom(subscriber, peer, message.awareness, handler.maxAwarenessClients) {
					return
				}
			case yjsAwarenessQuery:
				for _, awareness := range room.awarenessMessages() {
					if !subscriber.enqueue(awareness) {
						return
					}
				}
			case yjsSyncStep1:
				if room.store != nil {
					storeContext, cancel := context.WithTimeout(subscriber.context, handler.storeTimeout)
					accepted := room.replyDiff(storeContext, subscriber, message.payload)
					cancel()
					if !accepted {
						return
					}
					continue
				}
				// The room already sends every retained opaque update at connection
				// time. An empty Step 2 completes y-websocket's sync handshake.
				if !subscriber.enqueue(marshalYJSSync(yjsWireSyncStep2, yjsEmptyUpdate)) {
					return
				}
			}
		}
	}
}

func (room *YJSRoom) addAndBootstrap(subscriber *yjsSubscriber, peer Peer) [][]byte {
	room.mu.Lock()
	defer room.mu.Unlock()
	room.peers[subscriber] = peer
	bootstrap := make([][]byte, 0, len(room.updates)+len(room.awareness)+1)
	// Sending empty Step 2 first makes y-websocket complete its handshake;
	// retained updates that follow are ordinary idempotent y-protocol updates.
	bootstrap = append(bootstrap, marshalYJSSync(yjsWireSyncStep2, yjsEmptyUpdate))
	for _, update := range room.updates {
		bootstrap = append(bootstrap, marshalYJSSync(yjsWireUpdate, update))
	}
	for clientID, state := range room.awareness {
		bootstrap = append(bootstrap, marshalYJSAwareness([]yjsAwarenessEntry{{clientID: clientID, clock: state.clock, state: state.state}}))
	}
	return bootstrap
}

// bootstrap adds one peer and returns its first protocol messages. Store-backed
// rooms start the standard sync handshake with the durable state vector rather
// than replaying an in-process opaque history.
func (room *YJSRoom) bootstrap(ctx context.Context, subscriber *yjsSubscriber, peer Peer) ([][]byte, error) {
	if room.store == nil {
		return room.addAndBootstrap(subscriber, peer), nil
	}
	room.storeMu.Lock()
	defer room.storeMu.Unlock()
	vector, err := room.store.StateVector(ctx, room.document)
	if err != nil || len(vector) == 0 || len(vector) > room.maxStateVectorBytes {
		return nil, ErrYJSStoreUnavailable
	}
	room.mu.Lock()
	room.peers[subscriber] = peer
	bootstrap := make([][]byte, 0, len(room.awareness)+1)
	bootstrap = append(bootstrap, marshalYJSSync(yjsWireSyncStep1, vector))
	for clientID, state := range room.awareness {
		bootstrap = append(bootstrap, marshalYJSAwareness([]yjsAwarenessEntry{{clientID: clientID, clock: state.clock, state: state.state}}))
	}
	room.mu.Unlock()
	return bootstrap, nil
}

func (room *YJSRoom) remove(subscriber *yjsSubscriber, peer Peer) {
	room.mu.Lock()
	delete(room.peers, subscriber)
	removed := make([]yjsAwarenessEntry, 0)
	for clientID, state := range room.awareness {
		if !state.owner.matches(subscriber, peer) {
			continue
		}
		delete(room.awareness, clientID)
		room.rememberAwarenessTombstoneLocked(clientID, yjsAwarenessTombstone{clock: state.clock, owner: state.owner, removedAt: time.Now()})
		// y-protocols clears a remote awareness state with the same clock. A
		// larger clock is reserved for the originating client's own next state.
		removed = append(removed, yjsAwarenessEntry{clientID: clientID, clock: state.clock, state: []byte("null")})
	}
	room.mu.Unlock()
	for _, entry := range removed {
		room.broadcast(marshalYJSAwareness([]yjsAwarenessEntry{entry}))
	}
}

func (room *YJSRoom) appendUpdate(update []byte) bool {
	return room.appendUpdateContext(context.Background(), update)
}

func (room *YJSRoom) appendUpdateContext(ctx context.Context, update []byte) bool {
	if len(update) == 0 || len(update) > room.maxUpdateBytes {
		return false
	}
	if room.store != nil {
		room.storeMu.Lock()
		result, err := room.store.Apply(ctx, room.document, update)
		if err != nil {
			room.storeMu.Unlock()
			return false
		}
		if result.Applied {
			room.broadcast(marshalYJSSync(yjsWireUpdate, update))
		}
		room.storeMu.Unlock()
		return true
	}
	digest := sha256.Sum256(update)
	room.mu.Lock()
	if _, exists := room.hashes[digest]; exists {
		room.mu.Unlock()
		room.broadcast(marshalYJSSync(yjsWireUpdate, update))
		return true
	}
	if len(room.updates) >= room.maxUpdates || room.history > room.maxHistoryBytes-len(update) {
		room.mu.Unlock()
		return false
	}
	copied := append([]byte(nil), update...)
	room.updates = append(room.updates, copied)
	room.hashes[digest] = struct{}{}
	room.history += len(copied)
	room.mu.Unlock()
	room.broadcast(marshalYJSSync(yjsWireUpdate, copied))
	return true
}

func (room *YJSRoom) replyDiff(ctx context.Context, subscriber *yjsSubscriber, remoteVector []byte) bool {
	if room.store == nil || len(remoteVector) == 0 || len(remoteVector) > room.maxStateVectorBytes {
		return false
	}
	room.storeMu.Lock()
	defer room.storeMu.Unlock()
	delta, err := room.store.Diff(ctx, room.document, remoteVector)
	if err != nil || len(delta) == 0 || len(delta) > room.maxSyncBytes {
		return false
	}
	return subscriber.enqueue(marshalYJSSync(yjsWireSyncStep2, delta))
}

// applyAwareness retains the direct-room helper used by unit tests. A live
// WebSocket uses applyAwarenessFrom so ownership is scoped to its connection.
func (room *YJSRoom) applyAwareness(peer Peer, incoming []yjsAwarenessEntry, maxClients int) bool {
	return room.applyAwarenessFrom(nil, peer, incoming, maxClients)
}

func (room *YJSRoom) applyAwarenessFrom(subscriber *yjsSubscriber, peer Peer, incoming []yjsAwarenessEntry, maxClients int) bool {
	owner := yjsAwarenessOwner{subscriber: subscriber, peer: peer.ID}
	room.mu.Lock()
	next := make(map[uint64]yjsAwarenessState, len(room.awareness)+len(incoming))
	for clientID, state := range room.awareness {
		next[clientID] = state
	}
	nextTombstones := make(map[uint64]yjsAwarenessTombstone, len(room.awarenessTombstones)+len(incoming))
	for clientID, tombstone := range room.awarenessTombstones {
		nextTombstones[clientID] = tombstone
	}
	changed := make([]yjsAwarenessEntry, 0, len(incoming))
	for _, entry := range incoming {
		previous, active := next[entry.clientID]
		tombstone, removed := nextTombstones[entry.clientID]
		present := string(entry.state) != "null"
		knownClock := uint64(0)
		knownOwner := yjsAwarenessOwner{}
		if active {
			knownClock, knownOwner = previous.clock, previous.owner
		} else if removed {
			knownClock, knownOwner = tombstone.clock, tombstone.owner
		}

		// This is the y-protocols acceptance rule: an equal clock is an
		// idempotent state update, except that an equal-clock null clears an
		// active remote state. Checking staleness before ownership also lets a
		// harmless duplicate forwarded by y-websocket pass without taking over
		// the connection's record.
		if entry.clock < knownClock || (entry.clock == knownClock && present) || (!active && !removed && entry.clock == 0) {
			continue
		}
		if (active || removed) && !knownOwner.matches(subscriber, peer) {
			room.mu.Unlock()
			return false
		}

		if !present {
			if !active {
				// A later null can advance an existing tombstone for peers that
				// still have an older state. Unknown nulls allocate no metadata.
				if removed && entry.clock > tombstone.clock {
					nextTombstones[entry.clientID] = yjsAwarenessTombstone{clock: entry.clock, owner: knownOwner, removedAt: time.Now()}
					changed = append(changed, copyYJSAwarenessEntry(entry))
				}
				continue
			}
			delete(next, entry.clientID)
			rememberYJSAwarenessTombstone(nextTombstones, entry.clientID, yjsAwarenessTombstone{clock: entry.clock, owner: previous.owner, removedAt: time.Now()}, room.maxAwarenessTombstones)
			changed = append(changed, copyYJSAwarenessEntry(entry))
			continue
		}

		if !active && len(next) >= maxClients {
			room.mu.Unlock()
			return false
		}
		next[entry.clientID] = yjsAwarenessState{clock: entry.clock, state: append([]byte(nil), entry.state...), owner: owner}
		delete(nextTombstones, entry.clientID)
		changed = append(changed, copyYJSAwarenessEntry(entry))
	}
	room.awareness = next
	room.awarenessTombstones = nextTombstones
	room.mu.Unlock()
	for _, entry := range changed {
		room.broadcast(marshalYJSAwareness([]yjsAwarenessEntry{entry}))
	}
	return true
}

const defaultMaxYJSAwarenessTombstones = 256

func (owner yjsAwarenessOwner) matches(subscriber *yjsSubscriber, peer Peer) bool {
	if owner.subscriber != nil {
		return owner.subscriber == subscriber
	}
	return owner.peer == peer.ID
}

func copyYJSAwarenessEntry(entry yjsAwarenessEntry) yjsAwarenessEntry {
	return yjsAwarenessEntry{clientID: entry.clientID, clock: entry.clock, state: append([]byte(nil), entry.state...)}
}

func (room *YJSRoom) rememberAwarenessTombstoneLocked(clientID uint64, tombstone yjsAwarenessTombstone) {
	rememberYJSAwarenessTombstone(room.awarenessTombstones, clientID, tombstone, room.maxAwarenessTombstones)
}

func rememberYJSAwarenessTombstone(tombstones map[uint64]yjsAwarenessTombstone, clientID uint64, tombstone yjsAwarenessTombstone, maximum int) {
	if maximum <= 0 {
		return
	}
	if _, exists := tombstones[clientID]; !exists && len(tombstones) >= maximum {
		var oldestID uint64
		var oldest time.Time
		for candidateID, candidate := range tombstones {
			if oldest.IsZero() || candidate.removedAt.Before(oldest) {
				oldestID, oldest = candidateID, candidate.removedAt
			}
		}
		delete(tombstones, oldestID)
	}
	tombstones[clientID] = tombstone
}

func (room *YJSRoom) awarenessMessages() [][]byte {
	room.mu.Lock()
	defer room.mu.Unlock()
	messages := make([][]byte, 0, len(room.awareness))
	for clientID, state := range room.awareness {
		messages = append(messages, marshalYJSAwareness([]yjsAwarenessEntry{{clientID: clientID, clock: state.clock, state: state.state}}))
	}
	return messages
}

func (room *YJSRoom) broadcast(data []byte) {
	room.mu.Lock()
	peers := make([]*yjsSubscriber, 0, len(room.peers))
	for subscriber := range room.peers {
		peers = append(peers, subscriber)
	}
	room.mu.Unlock()
	for _, subscriber := range peers {
		if !subscriber.enqueue(data) {
			subscriber.close()
			room.mu.Lock()
			delete(room.peers, subscriber)
			room.mu.Unlock()
		}
	}
}

type yjsSubscriber struct {
	context      context.Context
	cancel       context.CancelFunc
	conn         *websocket.Conn
	queue        peerQueue
	writeTimeout time.Duration
	closeOnce    sync.Once
}

func newYJSSubscriber(conn *websocket.Conn, maxMessages, maxBytes int, writeTimeout time.Duration) *yjsSubscriber {
	// #nosec G118 -- cancel is retained by the subscriber and invoked by close.
	ctx, cancel := context.WithCancel(context.Background())
	return &yjsSubscriber{context: ctx, cancel: cancel, conn: conn, queue: newPeerQueue(maxMessages, maxBytes), writeTimeout: writeTimeout}
}

func (subscriber *yjsSubscriber) enqueue(data []byte) bool {
	return subscriber != nil && subscriber.queue.enqueue(data)
}

func (subscriber *yjsSubscriber) writeLoop() {
	for {
		data, ok := subscriber.queue.dequeueContext(subscriber.context)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(subscriber.context, subscriber.writeTimeout)
		err := subscriber.conn.Write(ctx, websocket.MessageBinary, data)
		cancel()
		if err != nil {
			subscriber.close()
			return
		}
	}
}

func (subscriber *yjsSubscriber) close() {
	if subscriber != nil {
		subscriber.closeOnce.Do(func() {
			subscriber.cancel()
			subscriber.queue.close()
			_ = subscriber.conn.CloseNow()
		})
	}
}

const (
	// maxYJSMessageBytes keeps decoder conversions and retained room state
	// bounded even when an embedding application supplies custom limits.
	maxYJSMessageBytes     = 64 << 20
	maxYJSAwarenessClients = 1 << 16
	// A sync message has a top-level type, sync type, and byte length varuint.
	// Reserve the worst-case header so a configured semantic payload can always
	// be queued and emitted within MaxMessageBytes.
	maxYJSWireOverhead              = 3 * binary.MaxVarintLen64
	yjsMessageSync           uint64 = 0
	yjsMessageAwareness      uint64 = 1
	yjsMessageAwarenessQuery uint64 = 3
	yjsWireSyncStep1         uint64 = 0
	yjsWireSyncStep2         uint64 = 1
	yjsWireUpdate            uint64 = 2
)

// yjsEmptyUpdate is Y.encodeStateAsUpdate(new Y.Doc()) for the stable Yjs
// v1 update encoding. It is deliberately not an empty byte string: Yjs rejects
// an empty update while [0, 0] is the canonical empty struct/delete-set pair.
var yjsEmptyUpdate = []byte{0, 0}

type yjsIncoming struct {
	kind      YJSMessageKind
	payload   []byte
	awareness []yjsAwarenessEntry
}

type yjsAwarenessEntry struct {
	clientID uint64
	clock    uint64
	state    []byte
}

func unmarshalYJSMessages(data []byte, maxMessageBytes, maxAwarenessClients int) ([]yjsIncoming, error) {
	if len(data) == 0 || len(data) > maxMessageBytes {
		return nil, errInvalidWireMessage
	}
	messages := make([]yjsIncoming, 0, 1)
	for position := 0; position < len(data); {
		messageType, next, ok := yjsReadUvarint(data, position)
		if !ok {
			return nil, errInvalidWireMessage
		}
		position = next
		switch messageType {
		case yjsMessageSync:
			syncType, next, ok := yjsReadUvarint(data, position)
			if !ok {
				return nil, errInvalidWireMessage
			}
			payload, next, ok := yjsReadBytes(data, next, maxMessageBytes)
			if !ok {
				return nil, errInvalidWireMessage
			}
			position = next
			switch syncType {
			case yjsWireSyncStep1:
				messages = append(messages, yjsIncoming{kind: yjsSyncStep1, payload: append([]byte(nil), payload...)})
			case yjsWireSyncStep2, yjsWireUpdate:
				if len(payload) == 0 {
					return nil, errInvalidWireMessage
				}
				messages = append(messages, yjsIncoming{kind: YJSUpdate, payload: append([]byte(nil), payload...)})
			default:
				return nil, errInvalidWireMessage
			}
		case yjsMessageAwareness:
			payload, next, ok := yjsReadBytes(data, position, maxMessageBytes)
			if !ok {
				return nil, errInvalidWireMessage
			}
			entries, ok := unmarshalYJSAwareness(payload, maxAwarenessClients)
			if !ok {
				return nil, errInvalidWireMessage
			}
			messages = append(messages, yjsIncoming{kind: YJSAwareness, awareness: entries})
			position = next
		case yjsMessageAwarenessQuery:
			messages = append(messages, yjsIncoming{kind: yjsAwarenessQuery})
		default:
			return nil, errInvalidWireMessage
		}
	}
	return messages, nil
}

func unmarshalYJSAwareness(data []byte, maxClients int) ([]yjsAwarenessEntry, bool) {
	count, position, ok := yjsReadUvarint(data, 0)
	if !ok || maxClients < 0 || maxClients > maxYJSAwarenessClients || count > maxYJSAwarenessClients {
		return nil, false
	}
	entries := make([]yjsAwarenessEntry, 0)
	for index := uint64(0); index < count; index++ {
		if len(entries) >= maxClients {
			return nil, false
		}
		clientID, next, ok := yjsReadUvarint(data, position)
		if !ok {
			return nil, false
		}
		clock, next, ok := yjsReadUvarint(data, next)
		if !ok {
			return nil, false
		}
		state, next, ok := yjsReadBytes(data, next, len(data)-next)
		if !ok || !json.Valid(state) || (string(state) != "null" && !jsonObject(state)) {
			return nil, false
		}
		entries = append(entries, yjsAwarenessEntry{clientID: clientID, clock: clock, state: append([]byte(nil), state...)})
		position = next
	}
	return entries, position == len(data)
}

func jsonObject(data []byte) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal(data, &value) == nil && value != nil
}

func marshalYJSSync(syncType uint64, payload []byte) []byte {
	encoded := make([]byte, 0, 2+binary.MaxVarintLen64+len(payload))
	encoded = appendUvarint(encoded, yjsMessageSync)
	encoded = appendUvarint(encoded, syncType)
	encoded = appendUvarint(encoded, uint64(len(payload)))
	return append(encoded, payload...)
}

func marshalYJSAwareness(entries []yjsAwarenessEntry) []byte {
	payload := make([]byte, 0, 1+len(entries)*16)
	payload = appendUvarint(payload, uint64(len(entries)))
	for _, entry := range entries {
		payload = appendUvarint(payload, entry.clientID)
		payload = appendUvarint(payload, entry.clock)
		payload = appendUvarint(payload, uint64(len(entry.state)))
		payload = append(payload, entry.state...)
	}
	encoded := make([]byte, 0, 1+binary.MaxVarintLen64+len(payload))
	encoded = appendUvarint(encoded, yjsMessageAwareness)
	encoded = appendUvarint(encoded, uint64(len(payload)))
	return append(encoded, payload...)
}

func yjsReadUvarint(data []byte, position int) (uint64, int, bool) {
	if position < 0 || position >= len(data) {
		return 0, position, false
	}
	value, size := binary.Uvarint(data[position:])
	if size <= 0 || size != uvarintSize(value) {
		return 0, position, false
	}
	return value, position + size, true
}

func yjsReadBytes(data []byte, position, maximum int) ([]byte, int, bool) {
	length, next, ok := yjsReadUvarint(data, position)
	if !ok || maximum < 0 || maximum > maxYJSMessageBytes || length > maxYJSMessageBytes {
		return nil, position, false
	}
	size := int(length)
	if size > maximum || size > len(data)-next {
		return nil, position, false
	}
	end := next + size
	return data[next:end], end, true
}

func appendUvarint(data []byte, value uint64) []byte { return binary.AppendUvarint(data, value) }

func uvarintSize(value uint64) int {
	size := 1
	for value >= 0x80 {
		value >>= 7
		size++
	}
	return size
}

func normalizeYJSLimits(config YJSConfig) (transportLimits, error) {
	if config.MaxMessageBytes == 0 {
		config.MaxMessageBytes = defaultMaxMessageBytes
	}
	if config.MaxQueuedMessages == 0 {
		config.MaxQueuedMessages = defaultMaxQueuedMessages
	}
	if config.MaxQueuedBytes == 0 {
		config.MaxQueuedBytes = defaultMaxQueuedBytes
		if config.MaxQueuedBytes < config.MaxMessageBytes {
			config.MaxQueuedBytes = config.MaxMessageBytes
		}
	}
	if config.HandshakeTimeout == 0 {
		config.HandshakeTimeout = defaultHandshakeTimeout
	}
	if config.WriteTimeout == 0 {
		config.WriteTimeout = defaultWriteTimeout
	}
	if config.MaxMessageBytes < 1024 || config.MaxMessageBytes > maxYJSMessageBytes || config.MaxQueuedMessages <= 0 || config.MaxQueuedBytes < config.MaxMessageBytes || config.HandshakeTimeout <= 0 || config.WriteTimeout <= 0 {
		return transportLimits{}, errors.New("invalid Yjs limits")
	}
	return transportLimits{maxMessageBytes: config.MaxMessageBytes, maxQueuedMessages: config.MaxQueuedMessages, maxQueuedBytes: config.MaxQueuedBytes, handshakeTimeout: config.HandshakeTimeout, writeTimeout: config.WriteTimeout}, nil
}
