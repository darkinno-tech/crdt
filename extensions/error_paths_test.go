package extensions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/counter"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/replica"
	"github.com/coder/websocket"
)

func TestGroupConfigurationAndDeliveryErrorPaths(t *testing.T) {
	if _, err := NewGroup(GroupConfig{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty group error = %v, want ErrInvalidConfig", err)
	}
	var nilGroup *Group
	if nilGroup.Manifest() != (replica.Manifest{}) {
		t.Fatal("nil group returned a manifest")
	}
	if len(nilGroup.Frontier().Entries()) != 0 {
		t.Fatal("nil group returned a populated frontier")
	}
	if changes, bytes := nilGroup.Pending(); changes != 0 || bytes != 0 {
		t.Fatalf("nil group pending = %d, %d", changes, bytes)
	}

	_, group, manifest, _ := newCounterHandler(t, FeatureHTTP, nil)
	if group.Manifest() != manifest {
		t.Fatal("group returned a different manifest")
	}
	if changes, bytes := group.Pending(); changes != 0 || bytes != 0 {
		t.Fatalf("new group pending = %d, %d", changes, bytes)
	}
	if _, err := group.receive(Peer{ID: "writer"}, func(Peer, replica.Manifest, replica.Dot) error { return nil }, []byte("bad"), 1024, 128); !errors.Is(err, errInvalidWireMessage) {
		t.Fatalf("invalid wire error = %v", err)
	}
	writer, err := counter.NewGCounter("writer")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := writer.Increment(1)
	if err != nil {
		t.Fatal(err)
	}
	change := newCounterChange(t, manifest, "writer", 1, delta)
	encoded, err := marshalChange(change)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := group.receive(Peer{ID: "writer"}, func(Peer, replica.Manifest, replica.Dot) error { return errors.New("deny") }, encoded, 1024, 128); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("authorization error = %v", err)
	}
}

func TestHandlerRejectsSubscriptionAndUnknownRoutes(t *testing.T) {
	server, _, manifest, _ := newCounterHandler(t, FeatureHTTP, nil)
	eventsURL, err := httpGroupURL(server.URL, manifest.GroupID, "events")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, eventsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer denied")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("denied subscription status = %d", response.StatusCode)
	}
	_ = response.Body.Close()

	changesURL, err := httpGroupURL(server.URL, manifest.GroupID, "changes")
	if err != nil {
		t.Fatal(err)
	}
	empty, err := http.NewRequest(http.MethodPost, changesURL, bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	empty.Header.Set("Authorization", "Bearer writer")
	empty.Header.Set("Content-Type", "application/octet-stream")
	response, err = http.DefaultClient.Do(empty)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty body status = %d", response.StatusCode)
	}
	_ = response.Body.Close()

	response, err = http.Get(server.URL + "/http/groups/not-base64/events")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route status = %d", response.StatusCode)
	}
	_ = response.Body.Close()

	var nilHandler *Handler
	recorder := httptest.NewRecorder()
	nilHandler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil handler status = %d", recorder.Code)
	}
}

func TestWebSocketUpgradeAndHandshakeFailurePaths(t *testing.T) {
	server, _, manifest, _ := newCounterHandler(t, FeatureWebSocket, nil)
	endpoint := "ws" + server.URL[len("http"):] + "/ws"
	plain, err := http.NewRequest(http.MethodGet, server.URL+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	plain.Header.Set("Authorization", "Bearer writer")
	plain.Header.Set("Origin", "https://evil.example")
	response, err := http.DefaultClient.Do(plain)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin upgrade status = %d", response.StatusCode)
	}
	_ = response.Body.Close()

	plain, err = http.NewRequest(http.MethodGet, server.URL+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	plain.Header.Set("Authorization", "Bearer writer")
	response, err = http.DefaultClient.Do(plain)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode < http.StatusBadRequest {
		t.Fatalf("non-upgrade status = %d", response.StatusCode)
	}
	_ = response.Body.Close()

	for _, hello := range [][]byte{
		[]byte("not-json"),
		mustHello(t, testManifest(t, "unknown-group")),
	} {
		connection := rawWebSocket(t, endpoint, "writer")
		if err := connection.Write(context.Background(), websocket.MessageText, hello); err != nil {
			t.Fatal(err)
		}
		waitForWebSocketClose(t, connection)
		_ = connection.CloseNow()
	}
	connection := rawWebSocket(t, endpoint, "denied")
	if err := connection.Write(context.Background(), websocket.MessageText, mustHello(t, manifest)); err != nil {
		t.Fatal(err)
	}
	waitForWebSocketClose(t, connection)
	_ = connection.CloseNow()
}

func TestWebSocketAllowsConfiguredCrossOriginHost(t *testing.T) {
	server, _, manifest, _ := newCounterHandler(t, FeatureWebSocket, func(config *Config) {
		config.OriginPatterns = []string{"app.example"}
	})
	endpoint := "ws" + server.URL[len("http"):] + "/ws"
	connection, _, err := websocket.Dial(context.Background(), endpoint, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer writer"},
			"Origin":        []string{"https://app.example"},
		},
		Subprotocols: []string{Subprotocol},
	})
	if err != nil {
		t.Fatalf("configured cross-origin handshake: %v", err)
	}
	defer func() { _ = connection.CloseNow() }()
	if err := connection.Write(context.Background(), websocket.MessageText, mustHello(t, manifest)); err != nil {
		t.Fatal(err)
	}
	messageType, response, err := connection.Read(context.Background())
	if err != nil || messageType != websocket.MessageText {
		t.Fatalf("configured cross-origin response = %v, %v", messageType, err)
	}
	remote, err := unmarshalHello(response)
	if err != nil || manifest.Compatible(remote) != nil {
		t.Fatalf("configured cross-origin manifest = %#v, %v", remote, err)
	}
}

func TestHTTPEventsMethodAndStreamingWriterFailures(t *testing.T) {
	server, group, manifest, _ := newCounterHandler(t, FeatureHTTP, nil)
	eventsURL, err := httpGroupURL(server.URL, manifest.GroupID, "events")
	if err != nil {
		t.Fatal(err)
	}
	post, err := http.NewRequest(http.MethodPost, eventsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	post.Header.Set("Authorization", "Bearer writer")
	response, err := http.DefaultClient.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != http.MethodGet {
		t.Fatalf("events method response = %d allow=%q", response.StatusCode, response.Header.Get("Allow"))
	}
	_ = response.Body.Close()

	handler := server.Config.Handler.(*Handler)
	writer := &bareResponseWriter{header: make(http.Header)}
	request := httptest.NewRequest(http.MethodGet, "/http/groups/x/events", nil)
	request.Header.Set("Authorization", "Bearer writer")
	handler.serveHTTPEvents(writer, request, group)
	if writer.status != http.StatusInternalServerError {
		t.Fatalf("non-flushing writer status = %d", writer.status)
	}
}

func TestWebSocketClientRejectsMismatchesAndReportsCallbackErrors(t *testing.T) {
	server, _, manifest, _ := newCounterHandler(t, FeatureWebSocket, nil)
	endpoint := "ws" + server.URL[len("http"):] + "/ws"
	if _, err := DialWebSocket(context.Background(), endpoint, manifest, ClientConfig{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil callback error = %v", err)
	}
	wrong, err := replica.NewManifest(manifest.GroupID, "example.com/other/v1", manifest.Epoch, manifest.Protocol, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DialWebSocket(context.Background(), endpoint, wrong, ClientConfig{Header: bearerHeader("writer"), OnChange: func(replica.Change) error { return nil }}); err == nil {
		t.Fatal("mismatched manifest dial succeeded")
	}

	callbackErr := errors.New("callback failed")
	failed, err := DialWebSocket(context.Background(), endpoint, manifest, ClientConfig{
		Header: bearerHeader("observer"),
		OnChange: func(replica.Change) error {
			return callbackErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = failed.Close() }()
	publisher := newWebSocketCounterClient(t, endpoint, manifest, "writer")
	change := incrementChange(t, publisher.state, manifest, "writer", 1, 1)
	if err := publisher.client.Publish(context.Background(), change); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		select {
		case <-failed.Done():
			return failed.Err() != nil
		default:
			return false
		}
	})
	if failed.Publish(context.Background(), replica.Change{}) == nil {
		t.Fatal("closed client accepted invalid publish")
	}
	if failed.Done() == nil || failed.Err() == nil {
		t.Fatal("failed WebSocket client did not expose terminal state")
	}
}

func TestHTTPClientRejectsServerContractAndReportsCallbackErrors(t *testing.T) {
	manifest := testManifest(t, "http-errors")
	for _, handler := range []http.Handler{
		http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "no", http.StatusUnauthorized)
		}),
		http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "text/plain")
			writer.WriteHeader(http.StatusOK)
		}),
		http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.Header().Set("X-CRDT-Manifest", "invalid")
			writer.WriteHeader(http.StatusOK)
		}),
	} {
		server := httptest.NewServer(handler)
		_, err := ConnectHTTP(context.Background(), server.URL, manifest, ClientConfig{OnChange: func(replica.Change) error { return nil }})
		server.Close()
		if err == nil {
			t.Fatal("invalid HTTP server contract connected")
		}
	}

	server, _, liveManifest, _ := newCounterHandler(t, FeatureHTTP, nil)
	callbackErr := errors.New("callback failed")
	failed, err := ConnectHTTP(context.Background(), server.URL, liveManifest, ClientConfig{
		Header: bearerHeader("observer"),
		OnChange: func(replica.Change) error {
			return callbackErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = failed.Close() }()
	publisher := newHTTPCounterClient(t, server.URL, liveManifest, "writer")
	change := incrementChange(t, publisher.state, liveManifest, "writer", 1, 1)
	if err := publisher.client.Publish(context.Background(), change); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		select {
		case <-failed.Done():
			return failed.Err() != nil
		default:
			return false
		}
	})
	if failed.Publish(context.Background(), replica.Change{}) == nil {
		t.Fatal("closed HTTP client accepted invalid publish")
	}
	if failed.Done() == nil || failed.Err() == nil {
		t.Fatal("failed HTTP client did not expose terminal state")
	}
}

func TestHTTPStreamTimeoutAndCloseLateResult(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	connectionContext, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(connectionContext, http.MethodGet, "http://example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openHTTPStream(context.Background(), time.Millisecond, client, request, cancel); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stream timeout error = %v", err)
	}

	closed := &closeRecorder{}
	results := make(chan httpStreamResult, 1)
	results <- httpStreamResult{response: &http.Response{Body: closed}}
	closeLateHTTPStreamResult(results)
	if !closed.closed {
		t.Fatal("late HTTP response body was not closed")
	}
}

func TestClientNilMethodsAndInvalidReadStreams(t *testing.T) {
	var httpClient *HTTPClient
	if httpClient.Done() != nil || !errors.Is(httpClient.Err(), ErrClosed) || !errors.Is(httpClient.Close(), ErrClosed) || !errors.Is(httpClient.Publish(context.Background(), replica.Change{}), ErrClosed) {
		t.Fatal("nil HTTP client methods did not fail closed")
	}
	var webSocketClient *WebSocketClient
	if webSocketClient.Done() != nil || !errors.Is(webSocketClient.Err(), ErrClosed) || !errors.Is(webSocketClient.Close(), ErrClosed) || !errors.Is(webSocketClient.Publish(context.Background(), replica.Change{}), ErrClosed) {
		t.Fatal("nil WebSocket client methods did not fail closed")
	}

	manifest := testManifest(t, "synthetic")
	for _, stream := range []string{
		"event: change\ndata: YQ==\n\n",
		"event: manifest\ndata: YQ==\n\n",
	} {
		ctx, cancel := context.WithCancel(context.Background())
		body := io.NopCloser(strings.NewReader(stream))
		client := &HTTPClient{
			manifest:        manifest,
			maxMessageBytes: 1024,
			maxActorBytes:   128,
			context:         ctx,
			cancel:          cancel,
			body:            body,
			done:            make(chan struct{}),
		}
		go client.readLoop(bufio.NewReader(body))
		select {
		case <-client.Done():
		case <-time.After(time.Second):
			t.Fatal("synthetic client did not stop")
		}
		if client.Err() == nil {
			t.Fatal("synthetic invalid stream did not report an error")
		}
	}
}

func TestQueueContextCancellationAndSubscriberDequeue(t *testing.T) {
	queue := newPeerQueue(1, 16)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := queue.dequeueContext(ctx); ok {
		t.Fatal("cancelled context dequeued a message")
	}
	subscriber := newSSESubscriber(1, 16)
	if !subscriber.enqueue([]byte{1}) {
		t.Fatal("subscriber enqueue failed")
	}
	if data, ok := subscriber.dequeue(); !ok || len(data) != 1 {
		t.Fatalf("subscriber dequeue = %x, %t", data, ok)
	}
	subscriber.close()
}

func TestBoundaryValidationAndBadMessages(t *testing.T) {
	manifest := testManifest(t, "boundary-validation")
	if _, err := NewGroup(GroupConfig{
		Manifest:          replica.Manifest{},
		MaxPendingChanges: 1,
		MaxPendingBytes:   1024,
		Apply:             func([]byte) error { return nil },
	}); err == nil {
		t.Fatal("invalid manifest created a group")
	}
	hugeManifest, err := replica.NewManifest(strings.Repeat("g", maxControlBytes), manifest.SchemaID, manifest.Epoch, manifest.Protocol, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewGroup(GroupConfig{
		Manifest:          hugeManifest,
		MaxPendingChanges: 1,
		MaxPendingBytes:   1024,
		Apply:             func([]byte) error { return nil },
	}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("oversized control manifest error = %v", err)
	}

	failing, err := NewGroup(GroupConfig{
		Manifest:          manifest,
		MaxPendingChanges: 1,
		MaxPendingBytes:   1024,
		Apply:             func([]byte) error { return errors.New("apply failed") },
	})
	if err != nil {
		t.Fatal(err)
	}
	valid := rawReplicaChange(t, manifest, "writer", 1)
	validWire, err := marshalChange(valid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failing.receive(Peer{ID: "writer"}, allowActor, validWire, 1024, 128); err == nil {
		t.Fatal("failing application state accepted a change")
	}
	if failing.Frontier().Counter("writer") != 0 {
		t.Fatal("failed application state advanced the frontier")
	}

	wrongFrame, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDGCounterState, Payload: []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failing.receive(Peer{ID: "writer"}, allowActor, rawChange("writer", 1, wrongFrame), 1024, 128); err == nil {
		t.Fatal("wrong delta protocol was accepted")
	}
	if _, err := marshalChange(replica.Change{}); !errors.Is(err, errInvalidWireMessage) {
		t.Fatalf("zero change marshal error = %v", err)
	}
	for _, encoded := range [][]byte{
		rawChange("", 1, []byte{1}),
		rawChange("writer", 0, []byte{1}),
	} {
		if _, _, err := unmarshalChange(encoded, 1024, 128); !errors.Is(err, errInvalidWireMessage) {
			t.Fatalf("bad dot %x error = %v", encoded, err)
		}
	}
	if _, _, err := readSSEEvent(bufio.NewReader(strings.NewReader("event: change\n")), 1024); !errors.Is(err, io.EOF) {
		t.Fatalf("truncated SSE error = %v", err)
	}
}

func TestRoutingAndQueueDefensiveBoundaries(t *testing.T) {
	_, group, manifest, _ := newCounterHandler(t, FeatureHTTP, nil)
	handler := &Handler{groups: map[string]*Group{manifest.GroupID: group}}
	for _, requestPath := range []string{
		"/wrong",
		"/http/groups/",
		"/http/groups/not-base64/events",
		"/http/groups/" + base64.RawURLEncoding.EncodeToString([]byte(manifest.GroupID)) + "/other",
	} {
		if found, _, ok := handler.groupForHTTPPath(requestPath); ok || found != nil {
			t.Fatalf("invalid route %q resolved a group", requestPath)
		}
	}
	if prefix, err := normalizeMountPrefix(""); err != nil || prefix != "/" {
		t.Fatalf("empty mount prefix = %q, %v", prefix, err)
	}
	if err := handler.Mount(http.NewServeMux(), "/?bad"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid mount error = %v", err)
	}
	if err := validateOriginPatterns([]string{"relay.example"}); err != nil {
		t.Fatalf("valid origin pattern error = %v", err)
	}
	if err := validateOriginPatterns([]string{""}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty origin pattern error = %v", err)
	}
	for _, patterns := range [][]string{{"https://app.example"}, {"["}} {
		if err := validateOriginPatterns(patterns); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("invalid origin patterns %q error = %v", patterns, err)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "http://relay.example/", nil)
	request.Host = "relay.example"
	request.Header.Set("Origin", "https://relay.example")
	if !handler.originAllowed(request) {
		t.Fatal("same-host origin was rejected")
	}
	limits, err := normalizeLimits(8<<20, 0, 0, 0, 0, 0)
	if err != nil || limits.maxQueuedBytes != limits.maxMessageBytes {
		t.Fatalf("queue default did not follow large message limit: %#v, %v", limits, err)
	}
	if _, err := normalizeLimits(1024, frame.DefaultLimits().MaxStringBytes+1, 1, 1024, time.Second, time.Second); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("oversized actor limit error = %v", err)
	}

	var nilQueue *peerQueue
	if nilQueue.enqueue([]byte{1}) {
		t.Fatal("nil queue accepted a message")
	}
	if _, ok := nilQueue.dequeueContext(context.Background()); ok {
		t.Fatal("nil queue returned a message")
	}
	var nilSubscriber *sseSubscriber
	if _, ok := nilSubscriber.dequeue(); ok {
		t.Fatal("nil subscriber returned a message")
	}
}

func allowActor(peer Peer, _ replica.Manifest, dot replica.Dot) error {
	if peer.ID != dot.Actor {
		return ErrUnauthorized
	}
	return nil
}

func bearerHeader(actor string) http.Header {
	return http.Header{"Authorization": []string{"Bearer " + actor}}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type closeRecorder struct {
	mu     sync.Mutex
	closed bool
}

type bareResponseWriter struct {
	header http.Header
	status int
}

func (writer *bareResponseWriter) Header() http.Header { return writer.header }

func (writer *bareResponseWriter) Write(data []byte) (int, error) { return len(data), nil }

func (writer *bareResponseWriter) WriteHeader(status int) { writer.status = status }

func rawWebSocket(t testing.TB, endpoint, actor string) *websocket.Conn {
	t.Helper()
	connection, _, err := websocket.Dial(context.Background(), endpoint, &websocket.DialOptions{
		HTTPHeader:   bearerHeader(actor),
		Subprotocols: []string{Subprotocol},
	})
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func mustHello(t testing.TB, manifest replica.Manifest) []byte {
	t.Helper()
	hello, err := marshalHello(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return hello
}

func waitForWebSocketClose(t testing.TB, connection *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := connection.Read(ctx)
	if err == nil {
		t.Fatal("server did not close rejected WebSocket handshake")
	}
}

func (recorder *closeRecorder) Read([]byte) (int, error) { return 0, io.EOF }

func (recorder *closeRecorder) Close() error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.closed = true
	return nil
}
