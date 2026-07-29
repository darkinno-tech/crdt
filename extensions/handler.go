package extensions

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	frame "github.com/DarkInno/crdt/encoding"
	"github.com/coder/websocket"
)

const (
	defaultMaxMessageBytes   = 1 << 20
	defaultMaxActorBytes     = 128
	defaultMaxQueuedMessages = 16
	defaultMaxQueuedBytes    = 4 << 20
	defaultHandshakeTimeout  = 10 * time.Second
	defaultWriteTimeout      = 10 * time.Second
)

// Config configures one optional transport handler. Features is default-off;
// no endpoint is active unless FeatureWebSocket or FeatureHTTP is selected.
// Authentication and separate read/write authorization are required whenever a
// feature is active.
type Config struct {
	Features              Feature
	Groups                []*Group
	Authenticate          Authenticate
	Authorize             Authorize
	AuthorizeSubscription AuthorizeSubscription
	// OriginPatterns lists case-insensitive host patterns permitted to make
	// browser-originated cross-origin requests, for example "app.example" or
	// "*.example.internal". It uses path.Match syntax, so "*" is rejected.
	// The request host is always permitted. These are host patterns rather than
	// URL strings so HTTP and WebSocket apply the same rule.
	OriginPatterns    []string
	MaxMessageBytes   int
	MaxActorBytes     int
	MaxQueuedMessages int
	MaxQueuedBytes    int
	HandshakeTimeout  time.Duration
	WriteTimeout      time.Duration
}

// Handler exposes enabled optional endpoints. It is safe to mount into an
// application-owned http.ServeMux and never starts an HTTP listener itself.
type Handler struct {
	features Feature
	groups   map[string]*Group

	authenticate          Authenticate
	authorize             Authorize
	authorizeSubscription AuthorizeSubscription
	origins               []string

	maxMessageBytes   int
	maxActorBytes     int
	maxQueuedMessages int
	maxQueuedBytes    int
	handshakeTimeout  time.Duration
	writeTimeout      time.Duration
}

// NewHandler validates config and constructs a disabled handler for the zero
// feature set. A disabled handler returns 404 for every request and does not
// invoke authentication or start background work.
func NewHandler(config Config) (*Handler, error) {
	if config.Features&^knownFeatures != 0 {
		return nil, ErrInvalidConfig
	}
	if config.Features == 0 {
		return &Handler{}, nil
	}
	if config.Authenticate == nil || config.Authorize == nil || config.AuthorizeSubscription == nil || len(config.Groups) == 0 {
		return nil, ErrInvalidConfig
	}
	limits, err := normalizeLimits(
		config.MaxMessageBytes,
		config.MaxActorBytes,
		config.MaxQueuedMessages,
		config.MaxQueuedBytes,
		config.HandshakeTimeout,
		config.WriteTimeout,
	)
	if err != nil {
		return nil, err
	}
	if err := validateOriginPatterns(config.OriginPatterns); err != nil {
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
		features:              config.Features,
		groups:                groups,
		authenticate:          config.Authenticate,
		authorize:             config.Authorize,
		authorizeSubscription: config.AuthorizeSubscription,
		origins:               append([]string(nil), config.OriginPatterns...),
		maxMessageBytes:       limits.maxMessageBytes,
		maxActorBytes:         limits.maxActorBytes,
		maxQueuedMessages:     limits.maxQueuedMessages,
		maxQueuedBytes:        limits.maxQueuedBytes,
		handshakeTimeout:      limits.handshakeTimeout,
		writeTimeout:          limits.writeTimeout,
	}, nil
}

// Mount mounts h below prefix and strips that prefix before routing its
// endpoints. For example, Mount(mux, "/crdt/") exposes "/crdt/ws" and the
// HTTP endpoints under "/crdt/http/".
func (h *Handler) Mount(mux *http.ServeMux, prefix string) (err error) {
	if h == nil || mux == nil {
		return ErrInvalidConfig
	}
	prefix, err = normalizeMountPrefix(prefix)
	if err != nil {
		return err
	}
	stripPrefix := strings.TrimSuffix(prefix, "/")
	defer func() {
		if recover() != nil {
			err = ErrInvalidConfig
		}
	}()
	mux.Handle(prefix, http.StripPrefix(stripPrefix, h))
	return nil
}

func normalizeMountPrefix(prefix string) (string, error) {
	if prefix == "" {
		return "/", nil
	}
	if !strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, "?#") {
		return "", ErrInvalidConfig
	}
	cleaned := path.Clean(prefix)
	if cleaned == "." {
		cleaned = "/"
	}
	if !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}
	return cleaned, nil
}

// ServeHTTP routes only enabled transport endpoints. It does not serve an
// index page or start a listener.
func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if h == nil {
		http.Error(writer, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if h.features.Enabled(FeatureWebSocket) && request.URL.Path == "/ws" {
		h.serveWebSocket(writer, request)
		return
	}
	if h.features.Enabled(FeatureHTTP) {
		group, operation, ok := h.groupForHTTPPath(request.URL.Path)
		if ok {
			switch operation {
			case "changes":
				h.serveHTTPChanges(writer, request, group)
			case "events":
				h.serveHTTPEvents(writer, request, group)
			}
			return
		}
	}
	http.NotFound(writer, request)
}

func (h *Handler) serveWebSocket(writer http.ResponseWriter, request *http.Request) {
	if !h.originAllowed(request) {
		http.Error(writer, "forbidden origin", http.StatusForbidden)
		return
	}
	peer, ok := h.authenticateRequest(writer, request)
	if !ok {
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
	group, response, err := h.readServerHandshake(handshakeContext, conn, peer)
	if err != nil {
		cancelHandshake()
		_ = conn.CloseNow()
		return
	}
	peerConnection := newWebSocketSubscriber(conn, h.maxQueuedMessages, h.maxQueuedBytes, h.writeTimeout)
	group.add(peerConnection)
	if err := conn.Write(handshakeContext, websocket.MessageText, response); err != nil {
		cancelHandshake()
		group.remove(peerConnection)
		peerConnection.close()
		return
	}
	cancelHandshake()
	conn.SetReadLimit(int64(h.maxMessageBytes))
	defer group.remove(peerConnection)
	defer peerConnection.close()
	go peerConnection.writeLoop()
	peerConnection.readLoop(peer, group, h.authorize, h.maxMessageBytes, h.maxActorBytes)
}

func (h *Handler) readServerHandshake(ctx context.Context, conn *websocket.Conn, peer Peer) (*Group, []byte, error) {
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
	if err := h.authorizeSubscription(peer, group.manifest); err != nil {
		return nil, nil, ErrUnauthorized
	}
	response, err := marshalHello(group.manifest)
	if err != nil {
		return nil, nil, err
	}
	return group, response, nil
}

func (h *Handler) serveHTTPChanges(writer http.ResponseWriter, request *http.Request, group *Group) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.originAllowed(request) {
		http.Error(writer, "forbidden origin", http.StatusForbidden)
		return
	}
	peer, ok := h.authenticateRequest(writer, request)
	if !ok {
		return
	}
	contentType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || contentType != "application/octet-stream" {
		http.Error(writer, "content type must be application/octet-stream", http.StatusUnsupportedMediaType)
		return
	}
	defer func() { _ = request.Body.Close() }()
	body := http.MaxBytesReader(writer, request.Body, int64(h.maxMessageBytes))
	data, err := io.ReadAll(body)
	if err != nil {
		http.Error(writer, "request body exceeds transport limit", http.StatusRequestEntityTooLarge)
		return
	}
	if len(data) == 0 {
		http.Error(writer, "request body is empty", http.StatusBadRequest)
		return
	}
	if _, err := group.receive(peer, h.authorize, data, h.maxMessageBytes, h.maxActorBytes); err != nil {
		if errors.Is(err, ErrUnauthorized) {
			http.Error(writer, "unauthorized", http.StatusForbidden)
			return
		}
		http.Error(writer, "invalid change", http.StatusBadRequest)
		return
	}
	setNoStoreHeaders(writer)
	writer.WriteHeader(http.StatusNoContent)
}

func (h *Handler) serveHTTPEvents(writer http.ResponseWriter, request *http.Request, group *Group) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.originAllowed(request) {
		http.Error(writer, "forbidden origin", http.StatusForbidden)
		return
	}
	peer, ok := h.authenticateRequest(writer, request)
	if !ok {
		return
	}
	if err := h.authorizeSubscription(peer, group.manifest); err != nil {
		http.Error(writer, "unauthorized", http.StatusForbidden)
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		http.Error(writer, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	stream := newSSESubscriber(h.maxQueuedMessages, h.maxQueuedBytes)
	group.add(stream)
	defer group.remove(stream)
	defer stream.close()

	setNoStoreHeaders(writer)
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Connection", "keep-alive")
	hello, err := marshalHello(group.manifest)
	if err != nil {
		http.Error(writer, "invalid manifest", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("X-CRDT-Manifest", base64.StdEncoding.EncodeToString(hello))
	writer.WriteHeader(http.StatusOK)
	if !writeSSEEvent(writer, "manifest", hello) {
		return
	}
	flusher.Flush()

	for {
		data, ok := stream.dequeueContext(request.Context())
		if !ok {
			return
		}
		if !writeSSEEvent(writer, "change", data) {
			return
		}
		flusher.Flush()
	}
}

func setNoStoreHeaders(writer http.ResponseWriter) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
}

func (h *Handler) groupForHTTPPath(requestPath string) (*Group, string, bool) {
	const prefix = "/http/groups/"
	if !strings.HasPrefix(requestPath, prefix) {
		return nil, "", false
	}
	parts := strings.Split(strings.TrimPrefix(requestPath, prefix), "/")
	if len(parts) != 2 || parts[0] == "" || (parts[1] != "changes" && parts[1] != "events") {
		return nil, "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(decoded) == 0 || base64.RawURLEncoding.EncodeToString(decoded) != parts[0] {
		return nil, "", false
	}
	group, exists := h.groups[string(decoded)]
	return group, parts[1], exists
}

func (h *Handler) authenticateRequest(writer http.ResponseWriter, request *http.Request) (Peer, bool) {
	peer, err := h.authenticate(request)
	if err != nil || strings.TrimSpace(peer.ID) == "" {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return Peer{}, false
	}
	return peer, true
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

func (h *Handler) originAllowed(request *http.Request) bool {
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
	for _, pattern := range h.origins {
		matched, err := path.Match(strings.ToLower(pattern), strings.ToLower(parsed.Host))
		if err == nil && matched {
			return true
		}
	}
	return false
}

type transportLimits struct {
	maxMessageBytes   int
	maxActorBytes     int
	maxQueuedMessages int
	maxQueuedBytes    int
	handshakeTimeout  time.Duration
	writeTimeout      time.Duration
}

func normalizeLimits(messageBytes, actorBytes, queuedMessages, queuedBytes int, handshakeTimeout, writeTimeout time.Duration) (transportLimits, error) {
	if messageBytes == 0 {
		messageBytes = defaultMaxMessageBytes
	}
	if actorBytes == 0 {
		actorBytes = defaultMaxActorBytes
	}
	if queuedMessages == 0 {
		queuedMessages = defaultMaxQueuedMessages
	}
	if queuedBytes == 0 {
		queuedBytes = defaultMaxQueuedBytes
		if queuedBytes < messageBytes {
			queuedBytes = messageBytes
		}
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
	if messageBytes < 1024 || messageBytes > maxWireBytes || queuedMessages <= 0 || queuedBytes < messageBytes || handshakeTimeout <= 0 || writeTimeout <= 0 {
		return transportLimits{}, ErrInvalidConfig
	}
	return transportLimits{
		maxMessageBytes:   messageBytes,
		maxActorBytes:     actorBytes,
		maxQueuedMessages: queuedMessages,
		maxQueuedBytes:    queuedBytes,
		handshakeTimeout:  handshakeTimeout,
		writeTimeout:      writeTimeout,
	}, nil
}

type peerQueue struct {
	outbound    chan []byte
	done        chan struct{}
	maxBytes    int64
	queuedBytes atomic.Int64
	closeOnce   sync.Once
}

func newPeerQueue(maxMessages, maxBytes int) peerQueue {
	return peerQueue{
		outbound: make(chan []byte, maxMessages),
		done:     make(chan struct{}),
		maxBytes: int64(maxBytes),
	}
}

func (q *peerQueue) enqueue(data []byte) bool {
	if q == nil || len(data) == 0 || int64(len(data)) > q.maxBytes {
		return false
	}
	select {
	case <-q.done:
		return false
	default:
	}
	copyData := append([]byte(nil), data...)
	size := int64(len(copyData))
	for {
		current := q.queuedBytes.Load()
		if current > q.maxBytes-size {
			return false
		}
		if q.queuedBytes.CompareAndSwap(current, current+size) {
			break
		}
	}
	select {
	case q.outbound <- copyData:
		return true
	case <-q.done:
		q.queuedBytes.Add(-size)
		return false
	default:
		q.queuedBytes.Add(-size)
		return false
	}
}

func (q *peerQueue) dequeue() ([]byte, bool) {
	return q.dequeueContext(context.Background())
}

func (q *peerQueue) dequeueContext(ctx context.Context) ([]byte, bool) {
	if q == nil {
		return nil, false
	}
	select {
	case <-ctx.Done():
		return nil, false
	case <-q.done:
		return nil, false
	default:
	}
	select {
	case <-ctx.Done():
		return nil, false
	case <-q.done:
		return nil, false
	case data := <-q.outbound:
		q.queuedBytes.Add(-int64(len(data)))
		select {
		case <-ctx.Done():
			return nil, false
		case <-q.done:
			return nil, false
		default:
			return data, true
		}
	}
}

func (q *peerQueue) close() {
	if q != nil {
		q.closeOnce.Do(func() { close(q.done) })
	}
}

type webSocketSubscriber struct {
	context      context.Context
	cancel       context.CancelFunc
	queue        peerQueue
	conn         *websocket.Conn
	writeTimeout time.Duration
	closeOnce    sync.Once
}

func newWebSocketSubscriber(conn *websocket.Conn, maxMessages, maxBytes int, writeTimeout time.Duration) *webSocketSubscriber {
	connectionContext, cancel := context.WithCancel(context.Background())
	return &webSocketSubscriber{
		context:      connectionContext,
		cancel:       cancel,
		queue:        newPeerQueue(maxMessages, maxBytes),
		conn:         conn,
		writeTimeout: writeTimeout,
	}
}

func (c *webSocketSubscriber) enqueue(data []byte) bool {
	return c != nil && c.queue.enqueue(data)
}

func (c *webSocketSubscriber) readLoop(peer Peer, group *Group, authorize Authorize, maxMessageBytes, maxActorBytes int) {
	for {
		messageType, data, err := c.conn.Read(c.context)
		if err != nil || messageType != websocket.MessageBinary {
			return
		}
		if _, err := group.receive(peer, authorize, data, maxMessageBytes, maxActorBytes); err != nil {
			return
		}
	}
}

func (c *webSocketSubscriber) writeLoop() {
	for {
		data, ok := c.queue.dequeue()
		if !ok {
			return
		}
		writeContext, cancel := context.WithTimeout(c.context, c.writeTimeout)
		err := c.conn.Write(writeContext, websocket.MessageBinary, data)
		cancel()
		if err != nil {
			c.close()
			return
		}
	}
}

func (c *webSocketSubscriber) close() {
	if c != nil {
		c.closeOnce.Do(func() {
			c.cancel()
			c.queue.close()
			_ = c.conn.CloseNow()
		})
	}
}

type sseSubscriber struct {
	queue peerQueue
}

func newSSESubscriber(maxMessages, maxBytes int) *sseSubscriber {
	return &sseSubscriber{queue: newPeerQueue(maxMessages, maxBytes)}
}

func (s *sseSubscriber) enqueue(data []byte) bool {
	return s != nil && s.queue.enqueue(data)
}

func (s *sseSubscriber) dequeue() ([]byte, bool) {
	if s == nil {
		return nil, false
	}
	return s.queue.dequeue()
}

func (s *sseSubscriber) dequeueContext(ctx context.Context) ([]byte, bool) {
	if s == nil {
		return nil, false
	}
	return s.queue.dequeueContext(ctx)
}

func (s *sseSubscriber) close() {
	if s != nil {
		s.queue.close()
	}
}

func writeSSEEvent(writer io.Writer, event string, data []byte) bool {
	encoded := base64.StdEncoding.EncodeToString(data)
	_, err := fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event, encoded)
	return err == nil
}
