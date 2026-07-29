// Package provider is a WebSocket CRDT transport reference implementation.
//
// It authenticates the HTTP upgrade, compares an exact replica.Manifest before
// accepting binary changes, bounds messages and queued writes, and uses a
// replica.Inbox to tolerate duplicate and out-of-order delivery. It deliberately
// does not add TLS, durable storage, replay/outbox handling, membership, or a
// tombstone-GC policy. Applications must provide those boundaries themselves.
package provider

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/replica"
	"github.com/coder/websocket"
)

const (
	// Subprotocol identifies the reference provider's control and binary-change
	// envelope. It is not a CRDT frame format version.
	Subprotocol = "crdt-sync-v1"

	defaultMaxMessageBytes   = 1 << 20
	defaultMaxActorBytes     = 128
	defaultMaxQueuedMessages = 64
	defaultHandshakeTimeout  = 10 * time.Second
	defaultWriteTimeout      = 10 * time.Second
)

var (
	// ErrInvalidConfig reports a missing or unsafe provider configuration.
	ErrInvalidConfig = errors.New("websocket provider: invalid configuration")
	// ErrUnauthorized reports authentication or actor authorization failure.
	ErrUnauthorized = errors.New("websocket provider: unauthorized")
	// ErrClosed reports use of a closed client.
	ErrClosed = errors.New("websocket provider: client is closed")
)

// Peer is the authenticated identity returned by Authenticate. ID should be a
// stable application identity, not an unauthenticated user-supplied actor ID.
type Peer struct {
	ID string
}

// Authenticate authenticates one HTTP upgrade request before it becomes a
// WebSocket connection. Returning an error rejects the request with HTTP 401.
type Authenticate func(*http.Request) (Peer, error)

// Authorize binds a proposed CRDT change to an authenticated peer and its
// negotiated manifest. At minimum it should prevent one peer from publishing
// under another logical replica actor.
type Authorize func(Peer, replica.Manifest, replica.Dot) error

// GroupConfig describes one in-memory replication group at the reference
// provider. Frontier must come from the same durable transaction as the
// application CRDT state when a production application restores a group.
type GroupConfig struct {
	Manifest          replica.Manifest
	Frontier          replica.Frontier
	Policy            crdt.ProtocolPolicy
	MaxPendingChanges int
	MaxPendingBytes   int
	Apply             replica.ApplyDelta
}

// Group owns a manifest-bound, bounded replica inbox and the live peers for
// that group. It has no operation log or snapshot store.
type Group struct {
	manifest replica.Manifest
	policy   crdt.ProtocolPolicy
	inbox    *replica.Inbox

	receiveMu sync.Mutex
	peersMu   sync.Mutex
	peers     map[*connection]struct{}
}

// NewGroup creates one manifest-bound in-memory receiver. Apply must use the
// concrete CRDT decoder with limits appropriate to the application, then apply
// the decoded delta without a partial update on error.
func NewGroup(config GroupConfig) (*Group, error) {
	if config.MaxPendingChanges <= 0 || config.MaxPendingBytes <= 0 || config.Apply == nil {
		return nil, ErrInvalidConfig
	}
	hello, err := marshalHello(config.Manifest)
	if err != nil || len(hello) > maxControlBytes {
		return nil, ErrInvalidConfig
	}
	inbox, err := replica.NewInboxWithPolicy(
		config.Manifest,
		config.Frontier,
		config.MaxPendingChanges,
		config.MaxPendingBytes,
		config.Apply,
		config.Policy,
	)
	if err != nil {
		return nil, fmt.Errorf("create replica inbox: %w", err)
	}
	return &Group{
		manifest: config.Manifest,
		policy:   config.Policy,
		inbox:    inbox,
		peers:    make(map[*connection]struct{}),
	}, nil
}

// Manifest returns the immutable-by-convention manifest negotiated by g.
func (g *Group) Manifest() replica.Manifest {
	if g == nil {
		return replica.Manifest{}
	}
	return g.manifest
}

// Frontier returns a copy of g's installed contiguous delivery frontier.
func (g *Group) Frontier() replica.Frontier {
	if g == nil {
		return replica.Frontier{}
	}
	return g.inbox.Frontier()
}

// Pending reports the number and bytes of out-of-order changes retained by g.
func (g *Group) Pending() (changes, bytes int) {
	if g == nil {
		return 0, 0
	}
	return g.inbox.Pending()
}

// Config configures a Handler. Authentication and authorization are required;
// callers must not rely on the CRDT frame checksum as an identity check.
type Config struct {
	Groups            []*Group
	Authenticate      Authenticate
	Authorize         Authorize
	OriginPatterns    []string
	MaxMessageBytes   int
	MaxActorBytes     int
	MaxQueuedMessages int
	HandshakeTimeout  time.Duration
	WriteTimeout      time.Duration
}

// Handler implements an authenticated WebSocket endpoint for a fixed set of
// manifest-bound Groups.
type Handler struct {
	groups map[string]*Group

	authenticate Authenticate
	authorize    Authorize
	origins      []string

	maxMessageBytes   int
	maxActorBytes     int
	maxQueuedMessages int
	handshakeTimeout  time.Duration
	writeTimeout      time.Duration
}

// NewHandler validates a reference provider configuration. The returned
// handler rejects cross-origin requests unless OriginPatterns explicitly
// authorizes their origin host.
func NewHandler(config Config) (*Handler, error) {
	if config.Authenticate == nil || config.Authorize == nil || len(config.Groups) == 0 {
		return nil, ErrInvalidConfig
	}
	limits, err := normalizeLimits(
		config.MaxMessageBytes,
		config.MaxActorBytes,
		config.MaxQueuedMessages,
		config.HandshakeTimeout,
		config.WriteTimeout,
	)
	if err != nil {
		return nil, err
	}
	groups := make(map[string]*Group, len(config.Groups))
	for _, group := range config.Groups {
		if group == nil || strings.TrimSpace(group.manifest.GroupID) == "" {
			return nil, ErrInvalidConfig
		}
		if _, exists := groups[group.manifest.GroupID]; exists {
			return nil, ErrInvalidConfig
		}
		hello, err := marshalHello(group.manifest)
		if err != nil || len(hello) > controlLimit(limits.maxMessageBytes) {
			return nil, ErrInvalidConfig
		}
		groups[group.manifest.GroupID] = group
	}
	return &Handler{
		groups:            groups,
		authenticate:      config.Authenticate,
		authorize:         config.Authorize,
		origins:           append([]string(nil), config.OriginPatterns...),
		maxMessageBytes:   limits.maxMessageBytes,
		maxActorBytes:     limits.maxActorBytes,
		maxQueuedMessages: limits.maxQueuedMessages,
		handshakeTimeout:  limits.handshakeTimeout,
		writeTimeout:      limits.writeTimeout,
	}, nil
}

// ServeHTTP authenticates the HTTP request, upgrades it, requires the provider
// subprotocol, and accepts changes only after an exact manifest handshake.
func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if h == nil {
		http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	peer, err := h.authenticate(request)
	if err != nil || strings.TrimSpace(peer.ID) == "" {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		Subprotocols:    []string{Subprotocol},
		OriginPatterns:  h.origins,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	if conn.Subprotocol() != Subprotocol {
		_ = conn.CloseNow()
		return
	}
	conn.SetReadLimit(int64(controlLimit(h.maxMessageBytes)))
	handshakeContext, cancelHandshake := context.WithTimeout(context.Background(), h.handshakeTimeout)
	defer cancelHandshake()
	group, response, err := h.serverHandshake(handshakeContext, conn)
	if err != nil {
		_ = conn.CloseNow()
		return
	}
	conn.SetReadLimit(int64(h.maxMessageBytes))
	connectionContext, cancelConnection := context.WithCancel(context.Background())
	peerConnection := &connection{
		context:      connectionContext,
		cancel:       cancelConnection,
		conn:         conn,
		outbound:     make(chan []byte, h.maxQueuedMessages),
		writeTimeout: h.writeTimeout,
	}
	// Register before confirming the handshake. Dial returns once it receives
	// response, so acknowledging first would allow an immediately published
	// change to miss this peer.
	group.add(peerConnection)
	defer group.remove(peerConnection)
	defer peerConnection.close()
	if err := conn.Write(handshakeContext, websocket.MessageText, response); err != nil {
		return
	}
	go peerConnection.writeLoop()
	peerConnection.readLoop(peer, group, h.authorize, h.maxMessageBytes, h.maxActorBytes)
}

func (h *Handler) serverHandshake(ctx context.Context, conn *websocket.Conn) (*Group, []byte, error) {
	messageType, data, err := conn.Read(ctx)
	if err != nil || messageType != websocket.MessageText {
		return nil, nil, errInvalidWireMessage
	}
	remote, err := unmarshalHello(data)
	if err != nil {
		return nil, nil, err
	}
	group, exists := h.groups[remote.GroupID]
	if !exists {
		return nil, nil, ErrUnauthorized
	}
	if err := group.manifest.Compatible(remote); err != nil {
		return nil, nil, err
	}
	response, err := marshalHello(group.manifest)
	if err != nil {
		return nil, nil, err
	}
	return group, response, nil
}

func (g *Group) add(connection *connection) {
	g.peersMu.Lock()
	defer g.peersMu.Unlock()
	g.peers[connection] = struct{}{}
}

func (g *Group) remove(connection *connection) {
	g.peersMu.Lock()
	defer g.peersMu.Unlock()
	delete(g.peers, connection)
}

func (g *Group) receive(peer Peer, authorize Authorize, data []byte, maxMessageBytes, maxActorBytes int) error {
	dot, delta, err := unmarshalChange(data, maxMessageBytes, maxActorBytes)
	if err != nil {
		return err
	}
	change, err := replica.NewChangeWithPolicy(g.manifest, dot, delta, g.policy)
	if err != nil {
		return fmt.Errorf("validate change: %w", err)
	}
	if err := authorize(peer, g.manifest, change.Dot); err != nil {
		return ErrUnauthorized
	}
	encoded, err := marshalChange(change)
	if err != nil {
		return err
	}
	g.receiveMu.Lock()
	defer g.receiveMu.Unlock()
	delivery, err := g.inbox.Receive(change)
	if err != nil {
		return fmt.Errorf("receive change: %w", err)
	}
	// A known dot is not broadcast again. Inbox deliberately does not retain
	// already installed payload bytes, so a relay cannot prove that a later
	// same-dot payload is identical. Suppressing it contains a conflicting retry
	// instead of exposing peers to a payload that may not be the original change.
	// Durable production relays must also persist the actor/counter-to-payload
	// binding with the CRDT state and frontier transaction.
	if delivery.Accepted() {
		g.broadcast(encoded)
	}
	return nil
}

func (g *Group) broadcast(data []byte) {
	g.peersMu.Lock()
	peers := make([]*connection, 0, len(g.peers))
	for peer := range g.peers {
		peers = append(peers, peer)
	}
	g.peersMu.Unlock()
	for _, peer := range peers {
		if !peer.enqueue(data) {
			peer.close()
		}
	}
}

type transportLimits struct {
	maxMessageBytes   int
	maxActorBytes     int
	maxQueuedMessages int
	handshakeTimeout  time.Duration
	writeTimeout      time.Duration
}

func normalizeLimits(messageBytes, actorBytes, queuedMessages int, handshakeTimeout, writeTimeout time.Duration) (transportLimits, error) {
	if messageBytes == 0 {
		messageBytes = defaultMaxMessageBytes
	}
	if actorBytes == 0 {
		actorBytes = defaultMaxActorBytes
	}
	if queuedMessages == 0 {
		queuedMessages = defaultMaxQueuedMessages
	}
	if handshakeTimeout == 0 {
		handshakeTimeout = defaultHandshakeTimeout
	}
	if writeTimeout == 0 {
		writeTimeout = defaultWriteTimeout
	}
	frameLimits := frame.DefaultLimits()
	if actorBytes <= 0 || actorBytes > frameLimits.MaxStringBytes {
		return transportLimits{}, ErrInvalidConfig
	}
	maxWireBytes := frameLimits.MaxFrameBytes + actorBytes + 1 + 3*binary.MaxVarintLen64
	if messageBytes < 1024 || messageBytes > maxWireBytes || queuedMessages <= 0 || handshakeTimeout <= 0 || writeTimeout <= 0 {
		return transportLimits{}, ErrInvalidConfig
	}
	return transportLimits{
		maxMessageBytes:   messageBytes,
		maxActorBytes:     actorBytes,
		maxQueuedMessages: queuedMessages,
		handshakeTimeout:  handshakeTimeout,
		writeTimeout:      writeTimeout,
	}, nil
}

type connection struct {
	context      context.Context
	cancel       context.CancelFunc
	conn         *websocket.Conn
	outbound     chan []byte
	writeTimeout time.Duration
	closeOnce    sync.Once
}

func (connection *connection) readLoop(peer Peer, group *Group, authorize Authorize, maxMessageBytes, maxActorBytes int) {
	for {
		messageType, data, err := connection.conn.Read(connection.context)
		if err != nil {
			return
		}
		if messageType != websocket.MessageBinary {
			return
		}
		if err := group.receive(peer, authorize, data, maxMessageBytes, maxActorBytes); err != nil {
			return
		}
	}
}

func (connection *connection) writeLoop() {
	for {
		select {
		case <-connection.context.Done():
			return
		case data := <-connection.outbound:
			writeContext, cancel := context.WithTimeout(connection.context, connection.writeTimeout)
			err := connection.conn.Write(writeContext, websocket.MessageBinary, data)
			cancel()
			if err != nil {
				connection.close()
				return
			}
		}
	}
}

func (connection *connection) enqueue(data []byte) bool {
	select {
	case <-connection.context.Done():
		return false
	default:
	}
	copyData := append([]byte(nil), data...)
	select {
	case connection.outbound <- copyData:
		return true
	case <-connection.context.Done():
		return false
	default:
		return false
	}
}

func (connection *connection) close() {
	connection.closeOnce.Do(func() {
		connection.cancel()
		_ = connection.conn.CloseNow()
	})
}
