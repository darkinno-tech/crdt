package durable

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/replica"
	"github.com/DarkInno/crdt/telemetry"
	"github.com/coder/websocket"
)

const (
	defaultMaxMessageBytes    = 1 << 20
	defaultMaxActorBytes      = 128
	defaultQueuedEvents       = 64
	defaultQueuedBytes        = 4 << 20
	defaultReplayEvents       = 4096
	defaultReplayBytes        = 32 << 20
	defaultStateVectorEntries = 256
	defaultHandshakeTimeout   = 10 * time.Second
	defaultWriteTimeout       = 10 * time.Second
	defaultPingInterval       = 30 * time.Second
	defaultPingTimeout        = 10 * time.Second
)

// Config configures an authenticated durable WebSocket relay. Store and all
// authorization callbacks are required; the handler never starts a listener.
type Config struct {
	Store                 Log
	Groups                []*Group
	Authenticate          Authenticate
	Authorize             Authorize
	AuthorizeSubscription AuthorizeSubscription
	// RevalidateSubscription is optional. When provided, it runs before every
	// heartbeat so the host can close revoked or expired long-lived sessions.
	RevalidateSubscription RevalidateSubscription
	OriginPatterns         []string
	MaxMessageBytes        int
	MaxActorBytes          int
	MaxQueuedEvents        int
	MaxQueuedBytes         int
	MaxReplayEvents        int
	MaxReplayBytes         int
	MaxStateVectorEntries  int
	HandshakeTimeout       time.Duration
	WriteTimeout           time.Duration
	PingInterval           time.Duration
	PingTimeout            time.Duration
	// Telemetry receives bounded, payload-free operational events for
	// handshake, replay, and append outcomes. A nil Reporter is the default
	// and adds no reporting work to relay paths.
	Telemetry *telemetry.Reporter
}

type limits struct {
	maxMessageBytes       int
	maxActorBytes         int
	maxQueuedEvents       int
	maxQueuedBytes        int
	maxReplayEvents       uint64
	maxReplayBytes        uint64
	maxStateVectorEntries int
	handshakeTimeout      time.Duration
	writeTimeout          time.Duration
	pingInterval          time.Duration
	pingTimeout           time.Duration
}

// Handler is safe to mount into an application-owned HTTP server. It exposes
// only GET /ws and requires Subprotocol on every accepted connection.
type Handler struct {
	store                  Log
	groups                 map[string]*Group
	authenticate           Authenticate
	authorize              Authorize
	authorizeSubscription  AuthorizeSubscription
	revalidateSubscription RevalidateSubscription
	origins                []string
	limits                 limits
	telemetry              *telemetry.Reporter
}

// Group owns the manifest, validation boundary, and live subscribers for one
// durable operation log. It does not own concrete application CRDT state.
type Group struct {
	manifest replica.Manifest
	policy   crdt.ProtocolPolicy
	validate Validate

	mu    sync.Mutex
	peers map[*serverPeer]struct{}
}

// NewGroup validates one immutable-by-convention manifest and requires a
// state-independent concrete CRDT validator.
func NewGroup(config GroupConfig) (*Group, error) {
	if config.Validate == nil || strings.TrimSpace(config.Manifest.GroupID) == "" {
		return nil, invalidConfig("durable.new_group", ErrInvalidConfig)
	}
	if _, err := replica.NewSessionWithPolicy(config.Manifest, config.Policy); err != nil {
		return nil, crdt.WrapError(crdt.ErrorCodeInvalidConfig, "durable.new_group", fmt.Errorf("validate durable manifest: %w", err))
	}
	return &Group{
		manifest: config.Manifest,
		policy:   config.Policy,
		validate: config.Validate,
		peers:    make(map[*serverPeer]struct{}),
	}, nil
}

// Manifest returns the group's immutable-by-convention manifest.
func (group *Group) Manifest() replica.Manifest {
	if group == nil {
		return replica.Manifest{}
	}
	return group.manifest
}

// NewHandler validates a complete, bounded durable-relay configuration.
func NewHandler(config Config) (*Handler, error) {
	if config.Store == nil || config.Store.Closed() || config.Authenticate == nil || config.Authorize == nil || config.AuthorizeSubscription == nil || len(config.Groups) == 0 {
		return nil, invalidConfig("durable.new_handler", ErrInvalidConfig)
	}
	limits, err := normalizeLimits(config)
	if err != nil {
		return nil, invalidConfig("durable.new_handler", err)
	}
	if err := validateOriginPatterns(config.OriginPatterns); err != nil {
		return nil, invalidConfig("durable.new_handler", err)
	}
	groups := make(map[string]*Group, len(config.Groups))
	for _, group := range config.Groups {
		if group == nil || strings.TrimSpace(group.manifest.GroupID) == "" {
			return nil, invalidConfig("durable.new_handler", ErrInvalidConfig)
		}
		if _, exists := groups[group.manifest.GroupID]; exists {
			return nil, invalidConfig("durable.new_handler", ErrInvalidConfig)
		}
		if hello, err := marshalHello(group.manifest, 0); err != nil || len(hello) > controlLimit(limits.maxMessageBytes) {
			return nil, invalidConfig("durable.new_handler", ErrInvalidConfig)
		}
		groups[group.manifest.GroupID] = group
	}
	return &Handler{
		store:                  config.Store,
		groups:                 groups,
		authenticate:           config.Authenticate,
		authorize:              config.Authorize,
		authorizeSubscription:  config.AuthorizeSubscription,
		revalidateSubscription: config.RevalidateSubscription,
		origins:                append([]string(nil), config.OriginPatterns...),
		limits:                 limits,
		telemetry:              config.Telemetry,
	}, nil
}

// ServeHTTP exposes a single durable WebSocket endpoint at /ws.
func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handshakeStarted := handler.started()
	if handler == nil || handler.store == nil || handler.store.Closed() {
		handler.record("handshake", handshakeStarted, ErrClosed)
		http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if request.URL.Path != "/ws" {
		handler.record("handshake", handshakeStarted, errInvalidWire)
		http.NotFound(writer, request)
		return
	}
	if request.Method != http.MethodGet {
		handler.record("handshake", handshakeStarted, errInvalidWire)
		writer.Header().Set("Allow", http.MethodGet)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !handler.originAllowed(request) {
		handler.record("handshake", handshakeStarted, ErrUnauthorized)
		http.Error(writer, "forbidden origin", http.StatusForbidden)
		return
	}
	peer, err := handler.authenticate(request)
	if err != nil || strings.TrimSpace(peer.ID) == "" {
		handler.record("handshake", handshakeStarted, ErrUnauthorized)
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	protocols := []string{Subprotocol}
	if _, supportsStateVector := handler.store.(StateVectorLog); supportsStateVector {
		protocols = []string{StateVectorSubprotocol, Subprotocol}
	}
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		Subprotocols:    protocols,
		OriginPatterns:  handler.origins,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		handler.record("handshake", handshakeStarted, err)
		return
	}
	if connection.Subprotocol() != Subprotocol && connection.Subprotocol() != StateVectorSubprotocol {
		handler.record("handshake", handshakeStarted, errInvalidWire)
		_ = connection.CloseNow()
		return
	}
	connection.SetReadLimit(int64(controlLimit(handler.limits.maxMessageBytes)))
	handshakeContext, cancelHandshake := context.WithTimeout(context.Background(), handler.limits.handshakeTimeout)
	defer cancelHandshake()
	group, subscription, err := handler.serverHandshake(handshakeContext, connection, peer)
	if err != nil {
		handler.record("handshake", handshakeStarted, err)
		_ = connection.CloseNow()
		return
	}
	client := newServerPeer(connection, handler.limits.maxQueuedEvents, handler.limits.maxQueuedBytes, handler.limits.writeTimeout)
	handler.record("handshake", handshakeStarted, nil)
	replayStarted := handler.started()
	replay, highWater, err := group.subscribe(handler.store, client, subscription, handler.limits)
	if err != nil {
		handler.record("replay", replayStarted, err)
		if errors.Is(err, ErrReplayUnavailable) {
			if message, marshalErr := marshalError("replay_unavailable"); marshalErr == nil {
				_ = connection.Write(handshakeContext, websocket.MessageText, message)
			}
		}
		_ = connection.CloseNow()
		return
	}
	handler.record("replay", replayStarted, nil)
	defer group.remove(client)
	defer client.close()
	welcome, err := marshalWelcomeForSubprotocol(connection.Subprotocol(), group.manifest, highWater)
	if err != nil || connection.Write(handshakeContext, websocket.MessageText, welcome) != nil {
		return
	}
	for _, event := range replay {
		if !client.writeEvent(handshakeContext, event) {
			return
		}
	}
	if connection.Subprotocol() == StateVectorSubprotocol {
		complete, err := marshalCatchUpComplete(highWater)
		if err != nil || connection.Write(handshakeContext, websocket.MessageText, complete) != nil {
			return
		}
	}
	if client.isClosed() {
		return
	}
	cancelHandshake()
	connection.SetReadLimit(int64(handler.limits.maxMessageBytes))
	go client.writeLoop()
	go handler.heartbeat(client, peer, group)
	client.readLoop(peer, group, handler)
}

type subscriptionRequest struct {
	resume uint64
	vector *replica.Frontier
}

func (handler *Handler) serverHandshake(ctx context.Context, connection *websocket.Conn, peer Peer) (*Group, subscriptionRequest, error) {
	messageType, data, err := connection.Read(ctx)
	if err != nil || messageType != websocket.MessageText {
		return nil, subscriptionRequest{}, errInvalidWire
	}
	var remote replica.Manifest
	request := subscriptionRequest{}
	if connection.Subprotocol() == StateVectorSubprotocol {
		vector, parsedRemote, parseErr := stateVectorHandshake(data, handler.limits)
		if parseErr != nil {
			return nil, subscriptionRequest{}, parseErr
		}
		remote = parsedRemote
		request.vector = &vector
	} else {
		var resume uint64
		remote, resume, err = unmarshalHello(data)
		if err != nil {
			return nil, subscriptionRequest{}, err
		}
		request.resume = resume
	}
	group, exists := handler.groups[remote.GroupID]
	if !exists || group.manifest.Compatible(remote) != nil {
		return nil, subscriptionRequest{}, ErrUnauthorized
	}
	if err := handler.authorizeSubscription(peer, group.manifest); err != nil {
		return nil, subscriptionRequest{}, ErrUnauthorized
	}
	return group, request, nil
}

func (group *Group) subscribe(store Log, peer *serverPeer, request subscriptionRequest, limits limits) ([]Event, uint64, error) {
	group.mu.Lock()
	defer group.mu.Unlock()
	var (
		events    []Event
		highWater uint64
		err       error
	)
	if request.vector != nil {
		stateVectorStore, ok := store.(StateVectorLog)
		if !ok {
			return nil, 0, ErrStateVectorUnavailable
		}
		events, highWater, err = stateVectorStore.CatchUp(group.manifest.GroupID, *request.vector, limits.maxReplayEvents, limits.maxReplayBytes, group.manifest, group.policy, limits.maxMessageBytes, limits.maxActorBytes)
	} else {
		events, highWater, err = store.Replay(group.manifest.GroupID, request.resume, limits.maxReplayEvents, limits.maxReplayBytes, group.manifest, group.policy, limits.maxMessageBytes, limits.maxActorBytes)
	}
	if err != nil {
		return nil, 0, err
	}
	group.peers[peer] = struct{}{}
	return events, highWater, nil
}

func stateVectorHandshake(data []byte, limits limits) (replica.Frontier, replica.Manifest, error) {
	manifest, vector, err := unmarshalStateVectorHello(data, limits.maxStateVectorEntries, limits.maxActorBytes)
	return vector, manifest, err
}

func marshalWelcomeForSubprotocol(subprotocol string, manifest replica.Manifest, highWater uint64) ([]byte, error) {
	if subprotocol == StateVectorSubprotocol {
		return marshalStateVectorWelcome(manifest, highWater)
	}
	if subprotocol == Subprotocol {
		return marshalWelcome(manifest, highWater)
	}
	return nil, errInvalidWire
}

func (handler *Handler) heartbeat(peer *serverPeer, identity Peer, group *Group) {
	if handler == nil || peer == nil || group == nil {
		return
	}
	ticker := time.NewTicker(handler.limits.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-peer.context.Done():
			return
		case <-ticker.C:
			started := handler.started()
			if handler.revalidateSubscription != nil {
				if err := handler.revalidateSubscription(identity, group.manifest); err != nil {
					handler.record("heartbeat", started, ErrUnauthorized)
					peer.close()
					return
				}
			}
			pingContext, cancel := context.WithTimeout(peer.context, handler.limits.pingTimeout)
			err := peer.connection.Ping(pingContext)
			cancel()
			handler.record("heartbeat", started, err)
			if err != nil {
				peer.close()
				return
			}
		}
	}
}

func (group *Group) remove(peer *serverPeer) {
	if group == nil || peer == nil {
		return
	}
	group.mu.Lock()
	delete(group.peers, peer)
	group.mu.Unlock()
}

func (group *Group) publish(peer Peer, data []byte, handler *Handler) (err error) {
	started := handler.started()
	defer func() { handler.record("append", started, err) }()
	dot, delta, err := unmarshalChange(data, handler.limits.maxMessageBytes, handler.limits.maxActorBytes)
	if err != nil {
		return err
	}
	change, err := replica.NewChangeWithPolicy(group.manifest, dot, delta, group.policy)
	if err != nil {
		return fmt.Errorf("validate change: %w", err)
	}
	if err := handler.authorize(peer, group.manifest, change.Dot); err != nil {
		return ErrUnauthorized
	}
	if err := group.validate(change.Delta()); err != nil {
		return fmt.Errorf("validate concrete delta: %w", err)
	}
	group.mu.Lock()
	defer group.mu.Unlock()
	result, err := handler.store.Append(group.manifest.GroupID, change)
	if err != nil {
		return err
	}
	if result.Duplicate {
		return nil
	}
	for subscriber := range group.peers {
		if !subscriber.enqueue(result.Event) {
			subscriber.close()
			delete(group.peers, subscriber)
		}
	}
	return nil
}

func (handler *Handler) started() time.Time {
	if handler == nil || handler.telemetry == nil {
		return time.Time{}
	}
	return time.Now()
}

func (handler *Handler) record(operation string, started time.Time, err error) {
	if handler == nil || handler.telemetry == nil {
		return
	}
	outcome := telemetry.OutcomeSuccess
	code := crdt.ErrorCodeUnknown
	if err != nil {
		code = crdt.ErrorCodeOf(err)
		outcome = telemetry.OutcomeFailure
		switch {
		case errors.Is(err, ErrUnauthorized):
			outcome = telemetry.OutcomeRejected
			code = crdt.ErrorCodeUnauthorized
		case errors.Is(err, ErrStoreFull):
			outcome = telemetry.OutcomeRejected
			code = crdt.ErrorCodeResourceLimit
		case errors.Is(err, ErrClosed), errors.Is(err, ErrReplayUnavailable):
			code = crdt.ErrorCodeUnavailable
		case errors.Is(err, errInvalidWire):
			outcome = telemetry.OutcomeRejected
			code = crdt.ErrorCodeInvalidInput
		}
	}
	duration := time.Duration(0)
	if !started.IsZero() {
		duration = time.Since(started)
	}
	handler.telemetry.Record(telemetry.Event{
		Component: "durable",
		Operation: operation,
		Outcome:   outcome,
		Duration:  duration,
		ErrorCode: code,
	})
}

func (peer *serverPeer) readLoop(identity Peer, group *Group, handler *Handler) {
	for {
		messageType, data, err := peer.connection.Read(peer.context)
		if err != nil {
			return
		}
		if messageType != websocket.MessageBinary || group.publish(identity, data, handler) != nil {
			return
		}
	}
}

type serverPeer struct {
	connection   *websocket.Conn
	context      context.Context
	cancel       context.CancelFunc
	outbound     chan []byte
	writeTimeout time.Duration
	maxBytes     int

	mu          sync.Mutex
	queuedBytes int
	closed      bool
	closeOnce   sync.Once
}

func newServerPeer(connection *websocket.Conn, maxEvents, maxBytes int, writeTimeout time.Duration) *serverPeer {
	ctx, cancel := context.WithCancel(context.Background())
	return &serverPeer{
		connection:   connection,
		context:      ctx,
		cancel:       cancel,
		outbound:     make(chan []byte, maxEvents),
		writeTimeout: writeTimeout,
		maxBytes:     maxBytes,
	}
}

func (peer *serverPeer) enqueue(event Event) bool {
	encoded, err := marshalEvent(event)
	if err != nil {
		return false
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.closed || len(encoded) > peer.maxBytes || peer.queuedBytes > peer.maxBytes-len(encoded) {
		return false
	}
	select {
	case peer.outbound <- encoded:
		peer.queuedBytes += len(encoded)
		return true
	default:
		return false
	}
}

func (peer *serverPeer) writeEvent(parent context.Context, event Event) bool {
	encoded, err := marshalEvent(event)
	if err != nil {
		return false
	}
	writeContext, cancel := context.WithTimeout(parent, peer.writeTimeout)
	err = peer.connection.Write(writeContext, websocket.MessageBinary, encoded)
	cancel()
	return err == nil
}

func (peer *serverPeer) writeLoop() {
	for {
		select {
		case <-peer.context.Done():
			return
		case encoded := <-peer.outbound:
			peer.mu.Lock()
			peer.queuedBytes -= len(encoded)
			peer.mu.Unlock()
			writeContext, cancel := context.WithTimeout(peer.context, peer.writeTimeout)
			err := peer.connection.Write(writeContext, websocket.MessageBinary, encoded)
			cancel()
			if err != nil {
				peer.close()
				return
			}
		}
	}
}

func (peer *serverPeer) close() {
	if peer == nil {
		return
	}
	peer.closeOnce.Do(func() {
		peer.mu.Lock()
		peer.closed = true
		peer.mu.Unlock()
		peer.cancel()
		if peer.connection != nil {
			_ = peer.connection.CloseNow()
		}
	})
}

func (peer *serverPeer) isClosed() bool {
	if peer == nil {
		return true
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	return peer.closed
}

func normalizeLimits(config Config) (limits, error) {
	result := limits{
		maxMessageBytes:       config.MaxMessageBytes,
		maxActorBytes:         config.MaxActorBytes,
		maxQueuedEvents:       config.MaxQueuedEvents,
		maxQueuedBytes:        config.MaxQueuedBytes,
		maxReplayEvents:       uint64(config.MaxReplayEvents),
		maxReplayBytes:        uint64(config.MaxReplayBytes),
		maxStateVectorEntries: config.MaxStateVectorEntries,
		handshakeTimeout:      config.HandshakeTimeout,
		writeTimeout:          config.WriteTimeout,
		pingInterval:          config.PingInterval,
		pingTimeout:           config.PingTimeout,
	}
	if result.maxMessageBytes == 0 {
		result.maxMessageBytes = defaultMaxMessageBytes
	}
	if result.maxActorBytes == 0 {
		result.maxActorBytes = defaultMaxActorBytes
	}
	if result.maxQueuedEvents == 0 {
		result.maxQueuedEvents = defaultQueuedEvents
	}
	if result.maxQueuedBytes == 0 {
		result.maxQueuedBytes = defaultQueuedBytes
	}
	if result.maxReplayEvents == 0 {
		result.maxReplayEvents = defaultReplayEvents
	}
	if result.maxReplayBytes == 0 {
		result.maxReplayBytes = defaultReplayBytes
	}
	if result.maxStateVectorEntries == 0 {
		result.maxStateVectorEntries = defaultStateVectorEntries
	}
	if result.handshakeTimeout == 0 {
		result.handshakeTimeout = defaultHandshakeTimeout
	}
	if result.writeTimeout == 0 {
		result.writeTimeout = defaultWriteTimeout
	}
	if result.pingInterval == 0 {
		result.pingInterval = defaultPingInterval
	}
	if result.pingTimeout == 0 {
		result.pingTimeout = defaultPingTimeout
	}
	frameLimits := frame.DefaultLimits()
	maxWireBytes := frameLimits.MaxFrameBytes + result.maxActorBytes + 1 + 3*binary.MaxVarintLen64
	if result.maxMessageBytes < 1024 || result.maxMessageBytes > maxWireBytes || result.maxActorBytes <= 0 || result.maxActorBytes > frameLimits.MaxStringBytes || result.maxQueuedEvents <= 0 || result.maxQueuedBytes < result.maxMessageBytes || result.maxReplayEvents == 0 || result.maxReplayBytes < uint64(result.maxMessageBytes) || result.maxStateVectorEntries <= 0 || result.handshakeTimeout <= 0 || result.writeTimeout <= 0 || result.pingInterval <= 0 || result.pingTimeout <= 0 {
		return limits{}, ErrInvalidConfig
	}
	return result, nil
}

func validateOriginPatterns(patterns []string) error {
	for _, pattern := range patterns {
		if strings.TrimSpace(pattern) != pattern || pattern == "" || pattern == "*" || strings.ContainsAny(pattern, "/?#@") {
			return ErrInvalidConfig
		}
		if _, err := path.Match(strings.ToLower(pattern), ""); err != nil {
			return ErrInvalidConfig
		}
	}
	return nil
}

func (handler *Handler) originAllowed(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	if strings.EqualFold(parsed.Host, request.Host) {
		return true
	}
	for _, pattern := range handler.origins {
		matched, err := path.Match(strings.ToLower(pattern), strings.ToLower(parsed.Host))
		if err == nil && matched {
			return true
		}
	}
	return false
}
