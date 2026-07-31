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

// YJSRoomConfig configures one explicitly named, in-memory y-protocols room.
// A room retains complete Yjs update messages only to bootstrap later live
// peers. It cannot compact or validate Yjs document semantics; production
// hosts must replace it with a Yjs-aware durable store before its bounded
// history is exhausted.
type YJSRoomConfig struct {
	Name            string
	MaxUpdateBytes  int
	MaxHistoryBytes int
	MaxUpdates      int
}

// YJSRoom is a bounded, opaque Yjs update cache and live subscriber set.
// It is intentionally isolated from Group: y-protocols and this module's
// framed CRDT protocols have incompatible state and recovery semantics.
type YJSRoom struct {
	name            string
	maxUpdateBytes  int
	maxHistoryBytes int
	maxUpdates      int

	mu        sync.Mutex
	updates   [][]byte
	hashes    map[[sha256.Size]byte]struct{}
	history   int
	peers     map[*yjsSubscriber]Peer
	awareness map[uint64]yjsAwarenessState
}

type yjsAwarenessState struct {
	clock uint64
	state []byte // Canonical JSON text, including the literal null.
	peer  string
}

// NewYJSRoom creates one room. The zero value is deliberately not usable:
// room names and every retained-resource boundary must be selected by the
// embedding application.
func NewYJSRoom(config YJSRoomConfig) (*YJSRoom, error) {
	if strings.TrimSpace(config.Name) != config.Name || config.Name == "" || strings.Contains(config.Name, "/") ||
		config.MaxUpdateBytes <= 0 || config.MaxHistoryBytes < config.MaxUpdateBytes || config.MaxUpdates <= 0 {
		return nil, invalidConfig("extensions.new_yjs_room", ErrInvalidConfig)
	}
	return &YJSRoom{
		name:            config.Name,
		maxUpdateBytes:  config.MaxUpdateBytes,
		maxHistoryBytes: config.MaxHistoryBytes,
		maxUpdates:      config.MaxUpdates,
		peers:           make(map[*yjsSubscriber]Peer),
		hashes:          make(map[[sha256.Size]byte]struct{}),
		awareness:       make(map[uint64]yjsAwarenessState),
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
	WriteTimeout          time.Duration
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
	maxAwarenessClients := config.MaxAwarenessClients
	if maxAwarenessClients == 0 {
		maxAwarenessClients = 256
	}
	rooms := make(map[string]*YJSRoom, len(config.Rooms))
	for _, room := range config.Rooms {
		if room == nil || room.name == "" || room.maxUpdateBytes > limits.maxMessageBytes || room.maxHistoryBytes < room.maxUpdateBytes || room.maxUpdates <= 0 {
			return nil, invalidConfig("extensions.new_yjs_handler", ErrInvalidConfig)
		}
		if _, exists := rooms[room.name]; exists {
			return nil, invalidConfig("extensions.new_yjs_handler", ErrInvalidConfig)
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
	bootstrap := room.addAndBootstrap(subscriber, peer)
	defer func() {
		room.remove(subscriber, peer)
		subscriber.close()
	}()
	handshakeContext, cancel := context.WithTimeout(request.Context(), handler.handshakeTimeout)
	defer cancel()
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
				if !room.appendUpdate(message.payload) {
					return
				}
			case YJSAwareness:
				if !room.applyAwareness(peer, message.awareness, handler.maxAwarenessClients) {
					return
				}
			case yjsAwarenessQuery:
				for _, awareness := range room.awarenessMessages() {
					if !subscriber.enqueue(awareness) {
						return
					}
				}
			case yjsSyncStep1:
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

func (room *YJSRoom) remove(subscriber *yjsSubscriber, peer Peer) {
	room.mu.Lock()
	delete(room.peers, subscriber)
	removed := make([]yjsAwarenessEntry, 0)
	for clientID, state := range room.awareness {
		if state.peer != peer.ID {
			continue
		}
		delete(room.awareness, clientID)
		removed = append(removed, yjsAwarenessEntry{clientID: clientID, clock: state.clock + 1, state: []byte("null")})
	}
	room.mu.Unlock()
	for _, entry := range removed {
		room.broadcast(marshalYJSAwareness([]yjsAwarenessEntry{entry}))
	}
}

func (room *YJSRoom) appendUpdate(update []byte) bool {
	if len(update) == 0 || len(update) > room.maxUpdateBytes {
		return false
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

func (room *YJSRoom) applyAwareness(peer Peer, incoming []yjsAwarenessEntry, maxClients int) bool {
	room.mu.Lock()
	next := make(map[uint64]yjsAwarenessState, len(room.awareness)+len(incoming))
	for clientID, state := range room.awareness {
		next[clientID] = state
	}
	changed := make([]yjsAwarenessEntry, 0, len(incoming))
	for _, entry := range incoming {
		previous, exists := next[entry.clientID]
		if exists && entry.clock <= previous.clock {
			// y-websocket deliberately re-broadcasts received awareness
			// updates. An equal/older forwarded state is harmless and must
			// not be mistaken for a second connection taking ownership.
			continue
		}
		if exists && previous.peer != peer.ID {
			room.mu.Unlock()
			return false
		}
		if string(entry.state) == "null" {
			delete(next, entry.clientID)
		} else {
			if !exists && len(next) >= maxClients {
				room.mu.Unlock()
				return false
			}
			next[entry.clientID] = yjsAwarenessState{clock: entry.clock, state: append([]byte(nil), entry.state...), peer: peer.ID}
		}
		changed = append(changed, entry)
	}
	room.awareness = next
	room.mu.Unlock()
	for _, entry := range changed {
		room.broadcast(marshalYJSAwareness([]yjsAwarenessEntry{entry}))
	}
	return true
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
	maxYJSMessageBytes              = 64 << 20
	maxYJSAwarenessClients          = 1 << 16
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
				messages = append(messages, yjsIncoming{kind: yjsSyncStep1})
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
