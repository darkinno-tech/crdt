package extensions

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/im10furry/crdt/counter"
	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/replica"
	"github.com/coder/websocket"
)

func TestWebSocketClientRejectsLiveProtocolViolations(t *testing.T) {
	manifest := testManifest(t, "scripted-websocket")
	for _, script := range []func(context.Context, *websocket.Conn) error{
		func(ctx context.Context, connection *websocket.Conn) error {
			return connection.Write(ctx, websocket.MessageText, []byte("unexpected"))
		},
		func(ctx context.Context, connection *websocket.Conn) error {
			return connection.Write(ctx, websocket.MessageBinary, []byte{transportVersion})
		},
		func(ctx context.Context, connection *websocket.Conn) error {
			return connection.Write(ctx, websocket.MessageBinary, rawChange("writer", 1, []byte{0}))
		},
	} {
		server := scriptedWebSocketServer(t, script)
		endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
		client, err := DialWebSocket(context.Background(), endpoint, manifest, ClientConfig{OnChange: func(replica.Change) error { return nil }})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-client.Done():
		case <-time.After(time.Second):
			t.Fatal("protocol-invalid client did not stop")
		}
		if client.Err() == nil {
			t.Fatal("protocol-invalid client did not report an error")
		}
		_ = client.Close()
		server.Close()
	}
}

func TestWebSocketClientHandlesDialAndWriteFailure(t *testing.T) {
	manifest := testManifest(t, "write-failure")
	if _, err := DialWebSocket(context.Background(), "http://invalid", manifest, ClientConfig{OnChange: func(replica.Change) error { return nil }}); err == nil {
		t.Fatal("invalid WebSocket endpoint dial succeeded")
	}
	server := scriptedWebSocketServer(t, func(context.Context, *websocket.Conn) error {
		return nil
	})
	defer server.Close()
	client, err := DialWebSocket(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), manifest, ClientConfig{OnChange: func(replica.Change) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	// The receive loop may observe the scripted server's close first. Both
	// orderings establish the same precondition for this test: Publish writes
	// to a closed connection.
	if err := client.connection.CloseNow(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	change := rawReplicaChange(t, manifest, "writer", 1)
	if client.Publish(context.Background(), change) == nil {
		t.Fatal("write to closed WebSocket succeeded")
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("closed writer client did not stop")
	}
}

func TestWebSocketDialRejectsSubprotocolAndRemoteManifest(t *testing.T) {
	manifest := testManifest(t, "dial-contract")
	withoutProtocol := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err == nil {
			_ = connection.CloseNow()
		}
	}))
	defer withoutProtocol.Close()
	endpoint := "ws" + strings.TrimPrefix(withoutProtocol.URL, "http")
	if _, err := DialWebSocket(context.Background(), endpoint, manifest, ClientConfig{OnChange: func(replica.Change) error { return nil }}); !errors.Is(err, errInvalidWireMessage) {
		t.Fatalf("missing subprotocol error = %v", err)
	}

	wrongManifest := testManifest(t, "other-group")
	for _, response := range [][]byte{
		mustHello(t, wrongManifest),
		[]byte("not-json"),
	} {
		server := handshakeWebSocketServer(t, response)
		endpoint = "ws" + strings.TrimPrefix(server.URL, "http")
		if _, err := DialWebSocket(context.Background(), endpoint, manifest, ClientConfig{OnChange: func(replica.Change) error { return nil }}); err == nil {
			t.Fatal("invalid remote WebSocket handshake succeeded")
		}
		server.Close()
	}
}

func TestWebSocketClientPublishRejectsInvalidLocalChanges(t *testing.T) {
	server, group, manifest, _ := newCounterHandler(t, FeatureWebSocket, nil)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	client, err := DialWebSocket(context.Background(), endpoint, manifest, ClientConfig{
		Header:   bearerHeader("writer"),
		OnChange: func(replica.Change) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	group.peersMu.Lock()
	peers := len(group.peers)
	group.peersMu.Unlock()
	if peers != 1 {
		t.Fatalf("successful WebSocket dial registered %d peers, want 1", peers)
	}
	if err := client.Publish(context.Background(), replica.Change{}); err == nil {
		t.Fatal("zero WebSocket change published")
	}
	client.maxActorBytes = 1
	if err := client.Publish(context.Background(), rawReplicaChange(t, manifest, "writer", 1)); !errors.Is(err, errInvalidWireMessage) {
		t.Fatalf("actor-bound WebSocket publish error = %v", err)
	}
}

func TestWebSocketClientIgnoresBatchLimitWhenBatchesAreDisabled(t *testing.T) {
	server, _, manifest, _ := newCounterHandler(t, FeatureWebSocket, nil)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	client, err := DialWebSocket(context.Background(), endpoint, manifest, ClientConfig{
		Header:          bearerHeader("writer"),
		MaxBatchChanges: maximumBatchChanges + 1,
		OnChange:        func(replica.Change) error { return nil },
	})
	if err != nil {
		t.Fatalf("v1 dial rejected an irrelevant batch limit: %v", err)
	}
	defer func() { _ = client.Close() }()
	if err := client.PublishBatch(context.Background(), nil); !errors.Is(err, ErrBatchUnsupported) {
		t.Fatalf("v1 PublishBatch error = %v, want %v", err, ErrBatchUnsupported)
	}
}

func TestHTTPClientReadLoopRejectsLiveProtocolViolations(t *testing.T) {
	manifest := testManifest(t, "scripted-http")
	hello, err := marshalHello(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestEvent := sseEvent("manifest", hello)
	for _, trailing := range []string{
		sseEvent("other", []byte{1}),
		sseEvent("change", []byte{transportVersion}),
		sseEvent("change", rawChange("writer", 1, []byte{0})),
		"",
	} {
		ctx, cancel := context.WithCancel(context.Background())
		body := io.NopCloser(strings.NewReader(manifestEvent + trailing))
		client := &HTTPClient{
			manifest:        manifest,
			maxMessageBytes: 1024,
			maxActorBytes:   128,
			context:         ctx,
			cancel:          cancel,
			body:            body,
			done:            make(chan struct{}),
			onChange:        func(replica.Change) error { return nil },
		}
		go client.readLoop(bufio.NewReader(body))
		select {
		case <-client.Done():
		case <-time.After(time.Second):
			t.Fatal("HTTP protocol-invalid client did not stop")
		}
		if client.Err() == nil {
			t.Fatal("HTTP protocol-invalid client did not report an error")
		}
	}
}

func TestConnectHTTPValidatesInputAndRemoteManifest(t *testing.T) {
	manifest := testManifest(t, "connect-validation")
	if _, err := ConnectHTTP(context.Background(), "https://example.com", manifest, ClientConfig{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil callback error = %v", err)
	}
	if _, err := ConnectHTTP(context.Background(), "https://example.com", manifest, ClientConfig{MaxMessageBytes: 1023, OnChange: func(replica.Change) error { return nil }}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid limits error = %v", err)
	}
	if _, err := ConnectHTTP(context.Background(), "https://example.com", replica.Manifest{}, ClientConfig{OnChange: func(replica.Change) error { return nil }}); err == nil {
		t.Fatal("invalid manifest connected")
	}
	if _, err := ConnectHTTP(context.Background(), "ws://example.com", manifest, ClientConfig{OnChange: func(replica.Change) error { return nil }}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid HTTP endpoint error = %v", err)
	}

	wrong := testManifest(t, "other-group")
	hello, err := marshalHello(wrong)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("X-CRDT-Manifest", base64.StdEncoding.EncodeToString(hello))
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if _, err := ConnectHTTP(context.Background(), server.URL, manifest, ClientConfig{OnChange: func(replica.Change) error { return nil }}); err == nil {
		t.Fatal("incompatible remote manifest connected")
	}
}

func TestHTTPClientPublishValidatesAndPropagatesTransportFailures(t *testing.T) {
	server, _, manifest, _ := newCounterHandler(t, FeatureHTTP, nil)
	client := newHTTPCounterClient(t, server.URL, manifest, "writer")
	if err := client.client.Publish(context.Background(), replica.Change{}); err == nil {
		t.Fatal("invalid HTTP change published")
	}
	valid := rawReplicaChange(t, manifest, "writer", 1)
	client.client.maxActorBytes = 1
	if err := client.client.Publish(context.Background(), valid); !errors.Is(err, errInvalidWireMessage) {
		t.Fatalf("actor bound error = %v", err)
	}
	client.client.maxActorBytes = 128
	client.client.httpClient = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	})}
	if err := client.client.Publish(context.Background(), valid); err == nil {
		t.Fatal("offline HTTP publish succeeded")
	}
	client.client.httpClient = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusConflict, Status: "409 Conflict", Body: io.NopCloser(strings.NewReader("conflict"))}, nil
	})}
	if err := client.client.Publish(context.Background(), valid); err == nil {
		t.Fatal("non-successful HTTP publish succeeded")
	}
}

func scriptedWebSocketServer(t testing.TB, script func(context.Context, *websocket.Conn) error) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}})
		if err != nil {
			return
		}
		defer func() { _ = connection.CloseNow() }()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		messageType, data, err := connection.Read(ctx)
		if err != nil || messageType != websocket.MessageText {
			return
		}
		manifest, err := unmarshalHello(data)
		if err != nil {
			return
		}
		hello, err := marshalHello(manifest)
		if err != nil {
			return
		}
		if err := connection.Write(ctx, websocket.MessageText, hello); err != nil {
			return
		}
		_ = script(ctx, connection)
	}))
	return server
}

func handshakeWebSocketServer(t testing.TB, response []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}})
		if err != nil {
			return
		}
		defer func() { _ = connection.CloseNow() }()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		messageType, _, err := connection.Read(ctx)
		if err != nil || messageType != websocket.MessageText {
			return
		}
		_ = connection.Write(ctx, websocket.MessageText, response)
	}))
	return server
}

func rawChange(actor string, counter uint64, delta []byte) []byte {
	encoded := []byte{transportVersion}
	encoded = frame.AppendUvarint(encoded, uint64(len(actor)))
	encoded = append(encoded, actor...)
	encoded = frame.AppendUvarint(encoded, counter)
	encoded = frame.AppendUvarint(encoded, uint64(len(delta)))
	return append(encoded, delta...)
}

func rawReplicaChange(t testing.TB, manifest replica.Manifest, actor string, sequence uint64) replica.Change {
	t.Helper()
	writer, err := counter.NewGCounter(actor)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := writer.Increment(1)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	change, err := replica.NewChange(manifest, replica.Dot{Actor: actor, Counter: sequence}, encoded)
	if err != nil {
		t.Fatal(err)
	}
	return change
}

func sseEvent(event string, data []byte) string {
	return "event: " + event + "\ndata: " + base64.StdEncoding.EncodeToString(data) + "\n\n"
}
