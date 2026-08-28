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

	"github.com/darkinno-tech/crdt"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/telemetry"
	"github.com/coder/websocket"
)

const (
	defaultMaxMessageBytes   = 1 << 20
	defaultMaxActorBytes     = 128
	defaultMaxQueuedMessages = 16
	defaultMaxQueuedBytes    = 4 << 20
	defaultMaxBatchChanges   = 16
	maximumBatchChanges      = 1 << 10
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
	// MaxBatchChanges bounds independently identified changes in one
	// crdt-sync-v2 WebSocket message. It is relevant only when
	// FeatureWebSocketBatch is enabled and cannot exceed MaxQueuedMessages,
	// so v1 and SSE peers can be queued atomically or disconnected.
	MaxBatchChanges  int
	HandshakeTimeout time.Duration
	WriteTimeout     time.Duration
	// Telemetry receives bounded, payload-free handshake and publication
	// outcomes. A nil Reporter is the default and leaves relay paths unchanged.
	Telemetry *telemetry.Reporter
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
	maxBatchChanges   int
	handshakeTimeout  time.Duration
	writeTimeout      time.Duration
	telemetry         *telemetry.Reporter
}

// NewHandler validates config and constructs a disabled handler for the zero
// feature set. A disabled handler returns 404 for every request and does not
// invoke authentication or start background work.
func NewHandler(config Config) (*Handler, error) {
	if config.Features&^knownFeatures != 0 {
		return nil, invalidConfig("extensions.new_handler", ErrInvalidConfig)
	}
	if config.Features.Enabled(FeatureWebSocketBatch) && !config.Features.Enabled(FeatureWebSocket) {
		return nil, invalidConfig("extensions.new_handler", ErrInvalidConfig)
	}
	if config.Features == 0 {
		return &Handler{}, nil
	}
	if config.Authenticate == nil || config.Authorize == nil || config.AuthorizeSubscription == nil || len(config.Groups) == 0 {
		return nil, invalidConfig("extensions.new_handler", ErrInvalidConfig)
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
		return nil, invalidConfig("extensions.new_handler", err)
	}
	maxBatchChanges := 0
	if config.Features.Enabled(FeatureWebSocketBatch) {
		maxBatchChanges, err = normalizeBatchChanges(config.MaxBatchChanges)
		if err != nil || maxBatchChanges > limits.maxQueuedMessages {
			return nil, invalidConfig("extensions.new_handler", ErrInvalidConfig)
		}
	}
	if err := validateOriginPatterns(config.OriginPatterns); err != nil {
		return nil, invalidConfig("extensions.new_handler", err)
	}
	groups := make(map[string]*Group, len(config.Groups))
	for _, group := range config.Groups {
		if group == nil || strings.TrimSpace(group.manifest.GroupID) == "" {
			return nil, invalidConfig("extensions.new_handler", ErrInvalidConfig)
		}
		if _, exists := groups[group.manifest.GroupID]; exists {
			return nil, invalidConfig("extensions.new_handler", ErrInvalidConfig)
		}
		hello, err := marshalHello(group.manifest)
		if err != nil || len(hello) > controlLimit(limits.maxMessageBytes) {
			return nil, invalidConfig("extensions.new_handler", ErrInvalidConfig)
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
		maxBatchChanges:       maxBatchChanges,
		handshakeTimeout:      limits.handshakeTimeout,
		writeTimeout:          limits.writeTimeout,
		telemetry:             config.Telemetry,
	}, nil
}

// Mount mounts h below prefix and strips that prefix before routing its
// endpoints. For example, Mount(mux, "/crdt/") exposes "/crdt/ws" and the
// HTTP endpoints under "/crdt/http/".
func (h *Handler) Mount(mux *http.ServeMux, prefix string) (err error) {
	if h == nil || mux == nil {
		return invalidConfig("extensions.mount", ErrInvalidConfig)
	}
	prefix, err = normalizeMountPrefix(prefix)
	if err != nil {
		return invalidConfig("extensions.mount", err)
	}
	stripPrefix := strings.TrimSuffix(prefix, "/")
	defer func() {
		if recover() != nil {
			err = invalidConfig("extensions.mount", ErrInvalidConfig)
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
	handshakeStarted := h.started()
	if !h.originAllowed(request) {
		h.record("handshake", handshakeStarted, ErrUnauthorized)
		http.Error(writer, "forbidden origin", http.StatusForbidden)
		return
	}
	peer, ok := h.authenticateRequest(writer, request)
	if !ok {
		h.record("handshake", handshakeStarted, ErrUnauthorized)
		return
	}
	subprotocols := []string{Subprotocol}
	if h.features.Enabled(FeatureWebSocketBatch) {
		subprotocols = []string{BatchSubprotocol, Subprotocol}
	}
	conn, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		Subprotocols:    subprotocols,
		OriginPatterns:  h.origins,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		h.record("handshake", handshakeStarted, err)
		return
	}
	batchEnabled := conn.Subprotocol() == BatchSubprotocol
	if (!batchEnabled && conn.Subprotocol() != Subprotocol) || (batchEnabled && !h.features.Enabled(FeatureWebSocketBatch)) {
		h.record("handshake", handshakeStarted, errInvalidWireMessage)
		_ = conn.CloseNow()
		return
	}
	conn.SetReadLimit(int64(controlLimit(h.maxMessageBytes)))
	handshakeContext, cancelHandshake := context.WithTimeout(context.Background(), h.handshakeTimeout)
	group, response, err := h.readServerHandshake(handshakeContext, conn, peer)
	if err != nil {
		h.record("handshake", handshakeStarted, err)
		cancelHandshake()
		_ = conn.CloseNow()
		return
	}
	peerConnection := newWebSocketSubscriber(conn, h.maxQueuedMessages, h.maxQueuedBytes, h.writeTimeout, batchEnabled)
	group.add(peerConnection)
	if err := conn.Write(handshakeContext, websocket.MessageText, response); err != nil {
		h.record("handshake", handshakeStarted, err)
		cancelHandshake()
		group.remove(peerConnection)
		peerConnection.close()
		return
	}
	h.record("handshake", handshakeStarted, nil)
	cancelHandshake()
	conn.SetReadLimit(int64(h.maxMessageBytes))
	defer group.remove(peerConnection)
	defer peerConnection.close()
	go peerConnection.writeLoop()
	peerConnection.readLoop(peer, group, h.authorize, h.maxMessageBytes, h.maxActorBytes, h.maxBatchChanges, h.started, h.record)
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
	started := h.started()
	report := func(err error) { h.record("append", started, err) }
	if request.Method != http.MethodPost {
		report(errInvalidWireMessage)
		writer.Header().Set("Allow", http.MethodPost)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.originAllowed(request) {
		report(ErrUnauthorized)
		http.Error(writer, "forbidden origin", http.StatusForbidden)
		return
	}
	peer, ok := h.authenticateRequest(writer, request)
	if !ok {
		report(ErrUnauthorized)
		return
	}
	contentType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || contentType != "application/octet-stream" {
		report(errInvalidWireMessage)
		http.Error(writer, "content type must be application/octet-stream", http.StatusUnsupportedMediaType)
		return
	}
	defer func() { _ = request.Body.Close() }()
	body := http.MaxBytesReader(writer, request.Body, int64(h.maxMessageBytes))
	data, err := io.ReadAll(body)
	if err != nil {
		report(errInvalidWireMessage)
		http.Error(writer, "request body exceeds transport limit", http.StatusRequestEntityTooLarge)
		return
	}
	if len(data) == 0 {
		report(errInvalidWireMessage)
		http.Error(writer, "request body is empty", http.StatusBadRequest)
		return
	}
	if _, err := group.receive(peer, h.authorize, data, h.maxMessageBytes, h.maxActorBytes); err != nil {
		report(err)
		if errors.Is(err, ErrUnauthorized) {
			http.Error(writer, "unauthorized", http.StatusForbidden)
			return
		}
		http.Error(writer, "invalid change", http.StatusBadRequest)
		return
	}
	report(nil)
	setNoStoreHeaders(writer)
	writer.WriteHeader(http.StatusNoContent)
}

func (h *Handler) started() time.Time {
	if h == nil || h.telemetry == nil {
		return time.Time{}
	}
	return time.Now()
}

func (h *Handler) record(operation string, started time.Time, err error) {
	if h != nil {
		recordExtensionsEvent(h.telemetry, operation, started, err)
	}
}

func recordExtensionsEvent(reporter *telemetry.Reporter, operation string, started time.Time, err error) {
	if reporter == nil {
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
		case errors.Is(err, ErrBatchLimit):
			outcome = telemetry.OutcomeRejected
			code = crdt.ErrorCodeResourceLimit
		case errors.Is(err, ErrClosed):
			code = crdt.ErrorCodeUnavailable
		case errors.Is(err, errInvalidWireMessage):
			outcome = telemetry.OutcomeRejected
			code = crdt.ErrorCodeInvalidInput
		}
	}
	duration := time.Duration(0)
	if !started.IsZero() {
		duration = time.Since(started)
	}
	reporter.Record(telemetry.Event{
		Component: "extensions",
		Operation: operation,
		Outcome:   outcome,
		Duration:  duration,
		ErrorCode: code,
	})
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
	if h == nil {
		return false
	}
	return originAllowed(request, h.origins)
}

func originAllowed(request *http.Request, origins []string) bool {
	if request == nil {
		return false
	}
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
	for _, pattern := range origins {
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

func normalizeBatchChanges(batchChanges int) (int, error) {
	if batchChanges == 0 {
		batchChanges = defaultMaxBatchChanges
	}
	if batchChanges <= 0 || batchChanges > maximumBatchChanges {
		return 0, ErrInvalidConfig
	}
	return batchChanges, nil
}

type peerQueue struct {
	mu          sync.Mutex
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
	return q.enqueueAll([][]byte{data})
}

func (q *peerQueue) enqueueAll(data [][]byte) bool {
	if q == nil || len(data) == 0 {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	select {
	case <-q.done:
		return false
	default:
	}
	if len(data) > cap(q.outbound)-len(q.outbound) {
		return false
	}
	copyData := make([][]byte, 0, len(data))
	var size int64
	for _, item := range data {
		if len(item) == 0 || int64(len(item)) > q.maxBytes-size {
			return false
		}
		copied := append([]byte(nil), item...)
		copyData = append(copyData, copied)
		size += int64(len(copied))
	}
	if q.queuedBytes.Load() > q.maxBytes-size {
		return false
	}
	q.queuedBytes.Add(size)
	for _, item := range copyData {
		q.outbound <- item
	}
	return true
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
		q.closeOnce.Do(func() {
			q.mu.Lock()
			defer q.mu.Unlock()
			close(q.done)
		})
	}
}

type webSocketSubscriber struct {
	context      context.Context
	cancel       context.CancelFunc
	queue        peerQueue
	conn         *websocket.Conn
	writeTimeout time.Duration
	batchEnabled bool
	closeOnce    sync.Once
}

func newWebSocketSubscriber(conn *websocket.Conn, maxMessages, maxBytes int, writeTimeout time.Duration, batchEnabled bool) *webSocketSubscriber {
	// #nosec G118 -- cancel is retained by the subscriber and invoked by close.
	connectionContext, cancel := context.WithCancel(context.Background())
	return &webSocketSubscriber{
		context:      connectionContext,
		cancel:       cancel,
		queue:        newPeerQueue(maxMessages, maxBytes),
		conn:         conn,
		writeTimeout: writeTimeout,
		batchEnabled: batchEnabled,
	}
}

func (c *webSocketSubscriber) enqueue(data []byte) bool {
	return c != nil && c.queue.enqueue(data)
}

func (c *webSocketSubscriber) enqueueAll(data [][]byte) bool {
	return c != nil && c.queue.enqueueAll(data)
}

func (c *webSocketSubscriber) batchesEnabled() bool {
	return c != nil && c.batchEnabled
}

func (c *webSocketSubscriber) readLoop(peer Peer, group *Group, authorize Authorize, maxMessageBytes, maxActorBytes, maxBatchChanges int, started func() time.Time, report func(string, time.Time, error)) {
	for {
		messageType, data, err := c.conn.Read(c.context)
		if err != nil || messageType != websocket.MessageBinary {
			return
		}
		operationStarted := time.Time{}
		if started != nil {
			operationStarted = started()
		}
		if c.batchEnabled && isChangeBatch(data) {
			if err := group.receiveBatch(peer, authorize, data, maxMessageBytes, maxActorBytes, maxBatchChanges); err != nil {
				if report != nil {
					report("append_batch", operationStarted, err)
				}
				return
			}
			if report != nil {
				report("append_batch", operationStarted, nil)
			}
			continue
		}
		if _, err := group.receive(peer, authorize, data, maxMessageBytes, maxActorBytes); err != nil {
			if report != nil {
				report("append", operationStarted, err)
			}
			return
		}
		if report != nil {
			report("append", operationStarted, nil)
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

func (s *sseSubscriber) enqueueAll(data [][]byte) bool {
	return s != nil && s.queue.enqueueAll(data)
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
