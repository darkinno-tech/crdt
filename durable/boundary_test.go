package durable

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/replica"
	"github.com/coder/websocket"
	bolt "go.etcd.io/bbolt"
)

func TestStoreConfigurationCloseAndCorruptionFailures(t *testing.T) {
	if _, err := OpenStore("", StoreConfig{MaxEvents: 1, MaxBytes: 1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty path error = %v", err)
	}
	if _, err := OpenStore(t.TempDir()+"/missing/relay.db", StoreConfig{MaxEvents: 1, MaxBytes: 1}); err == nil {
		t.Fatal("missing parent directory opened")
	}
	path := t.TempDir() + "/relay.db"
	store := durableTestStore(t, path, 4, 1<<20)
	manifest := durableTestManifest(t)
	if _, err := store.Append("", durableTestChange(t, manifest, "alice", 1, 1)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty group append = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second close = %v", err)
	}
	if _, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "alice", 1, 1)); !errors.Is(err, ErrClosed) {
		t.Fatalf("append after close = %v", err)
	}
	if _, _, err := store.Replay(manifest.GroupID, 0, 0, 1, manifest, crdt.ProtocolPolicy{}, 1, 1); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid replay config = %v", err)
	}

	store = durableTestStore(t, path, 4, 1<<20)
	defer func() { _ = store.Close() }()
	if events, highWater, err := store.Replay("unused-group", 0, 4, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); err != nil || highWater != 0 || len(events) != 0 {
		t.Fatalf("empty group replay events=%+v high=%d err=%v", events, highWater, err)
	}
	if _, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "alice", 1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(transaction *bolt.Tx) error {
		group, err := store.groupBucket(transaction, manifest.GroupID, false)
		if err != nil {
			return err
		}
		return group.Bucket(bucketMeta).Put(keyCount, []byte{1})
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Replay(manifest.GroupID, 0, 4, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("corrupt metadata replay = %v", err)
	}
	if bytesToSequence([]byte{1}) != 0 {
		t.Fatal("invalid sequence key decoded")
	}
	if _, _, err := parseDotBinding([]byte{1}); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("invalid dot binding = %v", err)
	}
}

func TestStoreRejectsByteLimitedReplayAndDamagedRecords(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 8, 1<<20)
	defer func() { _ = store.Close() }()
	change := durableTestChange(t, manifest, "alice", 1, 1)
	if _, err := store.Append(manifest.GroupID, change); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Replay(manifest.GroupID, 0, 8, 1, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrReplayUnavailable) {
		t.Fatalf("byte-limited replay = %v", err)
	}
	if err := store.db.Update(func(transaction *bolt.Tx) error {
		group, err := store.groupBucket(transaction, manifest.GroupID, false)
		if err != nil {
			return err
		}
		return group.Bucket(bucketEvents).Put(sequenceKey(1), []byte{changeMessage})
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Replay(manifest.GroupID, 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("damaged event replay = %v", err)
	}
	if err := store.db.Update(func(transaction *bolt.Tx) error { return transaction.DeleteBucket(bucketGroups) }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "alice", 2, 2)); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("missing groups append = %v", err)
	}
}

func TestConfigurationOriginsAndHTTPBoundaries(t *testing.T) {
	manifest := durableTestManifest(t)
	if _, err := NewGroup(GroupConfig{Manifest: manifest}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil validator group = %v", err)
	}
	store := durableTestStore(t, t.TempDir()+"/relay.db", 4, 1<<20)
	defer func() { _ = store.Close() }()
	if _, err := NewHandler(Config{Store: store}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("missing callbacks handler = %v", err)
	}
	handler, group := durableTestHandler(t, store, manifest)
	if _, err := NewHandler(Config{
		Store:                 store,
		Groups:                []*Group{group},
		Authenticate:          handler.authenticate,
		Authorize:             handler.authorize,
		AuthorizeSubscription: handler.authorizeSubscription,
		MaxMessageBytes:       1,
	}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid handler limits = %v", err)
	}
	if _, err := NewHandler(Config{
		Store:                 store,
		Groups:                []*Group{group},
		Authenticate:          handler.authenticate,
		Authorize:             handler.authorize,
		AuthorizeSubscription: handler.authorizeSubscription,
		OriginPatterns:        []string{"https://not-a-host-pattern"},
	}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid handler origin = %v", err)
	}
	if got := group.Manifest(); got.GroupID != manifest.GroupID {
		t.Fatalf("manifest = %+v", got)
	}
	if _, err := NewHandler(Config{
		Store:                 store,
		Groups:                []*Group{group, group},
		Authenticate:          handler.authenticate,
		Authorize:             handler.authorize,
		AuthorizeSubscription: handler.authorizeSubscription,
	}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("duplicate group handler = %v", err)
	}
	if err := validateOriginPatterns([]string{"*"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("wildcard origin = %v", err)
	}
	if err := validateOriginPatterns([]string{"*.example.test"}); err != nil {
		t.Fatalf("valid origin pattern = %v", err)
	}
	handler.origins = []string{"*.example.test"}
	for _, test := range []struct {
		origin string
		want   bool
	}{
		{"https://app.example.test", true},
		{"https://other.invalid", false},
		{"https://localhost/path", false},
		{"", true},
		{"https://relay.test", true},
	} {
		request := httptest.NewRequest(http.MethodGet, "http://relay.test/ws", nil)
		request.Host = "relay.test"
		request.Header.Set("Origin", test.origin)
		if got := handler.originAllowed(request); got != test.want {
			t.Fatalf("origin %q allowed=%v want %v", test.origin, got, test.want)
		}
	}
	for _, test := range []struct {
		method string
		path   string
		origin string
		want   int
	}{
		{http.MethodGet, "/missing", "", http.StatusNotFound},
		{http.MethodPost, "/ws", "", http.StatusMethodNotAllowed},
		{http.MethodGet, "/ws", "https://other.invalid", http.StatusForbidden},
		{http.MethodGet, "/ws", "", http.StatusUnauthorized},
	} {
		request := httptest.NewRequest(test.method, "http://relay.test"+test.path, nil)
		request.Header.Set("Origin", test.origin)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("%s %s got %d want %d", test.method, test.path, response.Code, test.want)
		}
	}
}

func TestReplayUnavailableHandshakeIsExplicit(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 4, 1<<20)
	defer func() { _ = store.Close() }()
	handler, _ := durableTestHandler(t, store, manifest)
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer alice"}},
		Subprotocols: []string{Subprotocol},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.CloseNow() }()
	hello, err := marshalHello(manifest, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	messageType, data, err := connection.Read(ctx)
	if err != nil || messageType != websocket.MessageText {
		t.Fatalf("replay rejection response type=%v data=%q err=%v", messageType, data, err)
	}
	if err := unmarshalError(data); !errors.Is(err, ErrReplayUnavailable) {
		t.Fatalf("replay rejection = %v", err)
	}
}

func TestServerRejectsUnauthorizedConflictingAndOverCapacityPublishes(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 1, 1<<20)
	defer func() { _ = store.Close() }()
	handler, _ := durableTestHandler(t, store, manifest)
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

	unauthorized, closeUnauthorized := durableRawClient(t, endpoint, manifest, 0, "alice")
	defer closeUnauthorized()
	encoded, err := marshalChange(durableTestChange(t, manifest, "bob", 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := unauthorized.Write(context.Background(), websocket.MessageBinary, encoded); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		_, highWater, err := store.Replay(manifest.GroupID, 0, 2, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
		return err == nil && highWater == 0
	})

	publisher, closePublisher := durableRawClient(t, endpoint, manifest, 0, "alice")
	defer closePublisher()
	first := durableTestChange(t, manifest, "alice", 1, 1)
	encoded, err = marshalChange(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Write(context.Background(), websocket.MessageBinary, encoded); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		_, highWater, err := store.Replay(manifest.GroupID, 0, 2, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
		return err == nil && highWater == 1
	})
	conflicting, closeConflicting := durableRawClient(t, endpoint, manifest, 0, "alice")
	defer closeConflicting()
	encoded, err = marshalChange(durableTestChange(t, manifest, "alice", 1, 9))
	if err != nil {
		t.Fatal(err)
	}
	if err := conflicting.Write(context.Background(), websocket.MessageBinary, encoded); err != nil {
		t.Fatal(err)
	}
	overflow, closeOverflow := durableRawClient(t, endpoint, manifest, 0, "alice")
	defer closeOverflow()
	encoded, err = marshalChange(durableTestChange(t, manifest, "alice", 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	if err := overflow.Write(context.Background(), websocket.MessageBinary, encoded); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		_, highWater, err := store.Replay(manifest.GroupID, 0, 2, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
		return err == nil && highWater == 1
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://relay.test/ws", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed store status = %d", response.Code)
	}
}

func TestReconnectClientConfigurationAndCursorSafety(t *testing.T) {
	manifest := durableTestManifest(t)
	if _, err := NewReconnectClient("ws://example.test/ws", manifest, ClientConfig{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil callback client = %v", err)
	}
	if _, err := NewReconnectClient("https://example.test/ws", manifest, ClientConfig{OnEvent: func(Event) error { return nil }}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid scheme client = %v", err)
	}
	client, err := NewReconnectClient("ws://example.test/ws", manifest, ClientConfig{
		MaxQueuedChanges: 1,
		OnEvent:          func(Event) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	change := durableTestChange(t, manifest, "alice", 1, 1)
	if err := client.Publish(context.Background(), change); err != nil {
		t.Fatal(err)
	}
	if err := client.Publish(context.Background(), change); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("full queue publish = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Publish(cancelled, change); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled publish = %v", err)
	}
	if err := client.Publish(context.Background(), replica.Change{}); err == nil {
		t.Fatal("invalid change publish succeeded")
	}
	client.limits.maxMessageBytes = 1
	if err := client.Publish(context.Background(), change); !errors.Is(err, errInvalidWire) {
		t.Fatalf("oversized client publish = %v", err)
	}
	client.limits.maxMessageBytes = defaultMaxMessageBytes
	client.restore(change)
	next, err := client.nextChange(context.Background())
	if err != nil || next.Dot != change.Dot {
		t.Fatalf("pending next = %+v err=%v", next, err)
	}
	cancelled, cancel = context.WithCancel(context.Background())
	cancel()
	client.outbound = make(chan replica.Change)
	if _, err := client.nextChange(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled next = %v", err)
	}
	client.setErr(errors.New("transient"))
	if client.Err() == nil || client.Cursor() != 0 {
		t.Fatalf("client state err=%v cursor=%d", client.Err(), client.Cursor())
	}
	if got := nextBackoff(time.Second, time.Second); got != time.Second {
		t.Fatalf("capped backoff = %s", got)
	}
	if clone := cloneHeader(nil); clone == nil {
		t.Fatal("nil header clone")
	}
	client.running.Store(true)
	if err := client.Run(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("parallel run = %v", err)
	}
	client.running.Store(false)
}

func TestReconnectClientReturnsReplayUnavailable(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 8, 1<<20)
	defer func() { _ = store.Close() }()
	if _, err := store.Append(manifest.GroupID, durableTestChange(t, manifest, "alice", 1, 1)); err != nil {
		t.Fatal(err)
	}
	handler, _ := durableTestHandler(t, store, manifest)
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	client := durableTestClient(t, endpoint, manifest, 2, func(Event) error { return nil })
	if err := client.Run(context.Background()); !errors.Is(err, ErrReplayUnavailable) {
		t.Fatalf("replay unavailable run = %v", err)
	}
}

func TestOnEventFailureDoesNotAdvanceCursor(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 8, 1<<20)
	defer func() { _ = store.Close() }()
	handler, _ := durableTestHandler(t, store, manifest)
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	called := make(chan struct{}, 1)
	client := durableTestClient(t, endpoint, manifest, 0, func(Event) error {
		select {
		case called <- struct{}{}:
		default:
		}
		return errors.New("checkpoint failed")
	})
	client.limits.minBackoff = 20 * time.Millisecond
	client.limits.maxBackoff = 20 * time.Millisecond
	stop := runDurableClient(t, client)
	defer stop()
	publisher, closePublisher := durableRawClient(t, endpoint, manifest, 0, "alice")
	defer closePublisher()
	encoded, err := marshalChange(durableTestChange(t, manifest, "alice", 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Write(context.Background(), websocket.MessageBinary, encoded); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("OnEvent was not called")
	}
	if client.Cursor() != 0 {
		t.Fatalf("cursor advanced after failed checkpoint: %d", client.Cursor())
	}
}

func TestServerPeerQueueIsBounded(t *testing.T) {
	manifest := durableTestManifest(t)
	peer := newServerPeer(nil, 1, 1024, time.Second)
	change := durableTestChange(t, manifest, "alice", 1, 1)
	if !peer.enqueue(Event{Sequence: 1, Change: change}) {
		t.Fatal("first enqueue failed")
	}
	if peer.enqueue(Event{Sequence: 2, Change: change}) {
		t.Fatal("queue accepted overflow")
	}
	peer.mu.Lock()
	peer.closed = true
	peer.mu.Unlock()
	if peer.enqueue(Event{Sequence: 3, Change: change}) {
		t.Fatal("closed peer accepted event")
	}
	if !peer.isClosed() {
		t.Fatal("closed peer reports open")
	}
}

func TestNilAndInvalidEdgesFailClosed(t *testing.T) {
	if _, err := normalizeLimits(Config{MaxReplayEvents: -1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative replay events = %v", err)
	}
	if _, err := normalizeLimits(Config{MaxReplayBytes: -1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative replay bytes = %v", err)
	}
	var nilClient *ReconnectClient
	if nilClient.Cursor() != 0 || !errors.Is(nilClient.Err(), ErrClosed) || !errors.Is(nilClient.Publish(context.Background(), replica.Change{}), ErrClosed) {
		t.Fatal("nil client boundary did not fail closed")
	}
	var nilGroup *Group
	if got := nilGroup.Manifest(); got != (replica.Manifest{}) {
		t.Fatalf("nil group manifest = %+v", got)
	}
	nilGroup.remove(nil)
	var nilPeer *serverPeer
	nilPeer.close()
	if !nilPeer.isClosed() {
		t.Fatal("nil peer should be closed")
	}
	if _, err := NewGroup(GroupConfig{Manifest: replica.Manifest{}, Validate: func([]byte) error { return nil }}); err == nil {
		t.Fatal("invalid manifest group succeeded")
	}
	if _, err := normalizeClientLimits(ClientConfig{MaxMessageBytes: 1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid client limits = %v", err)
	}
	if _, err := normalizeClientLimits(ClientConfig{MaxMerkleLeaves: -1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative merkle leaves = %v", err)
	}
	if _, err := normalizeClientLimits(ClientConfig{MaxMerkleBytes: -1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative merkle bytes = %v", err)
	}
	if got := nextBackoff(time.Millisecond, time.Second); got != 2*time.Millisecond {
		t.Fatalf("uncapped backoff = %s", got)
	}
	if _, err := OpenStore(t.TempDir()+"/relay.db", StoreConfig{MaxEvents: 1, MaxBytes: 1, OpenTimeout: -time.Second}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative timeout store = %v", err)
	}
	if _, _, err := parseDotBinding(make([]byte, 8+32)); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("zero dot binding = %v", err)
	}
}

func TestGroupPublishDirectlyFailsClosed(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 1, 1<<20)
	defer func() { _ = store.Close() }()
	group, err := NewGroup(GroupConfig{
		Manifest: manifest,
		Validate: func(data []byte) error {
			if len(data) == 1 {
				return errors.New("reject")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{
		store: store,
		authorize: func(peer Peer, _ replica.Manifest, dot replica.Dot) error {
			if peer.ID != dot.Actor {
				return ErrUnauthorized
			}
			return nil
		},
		limits: limits{maxMessageBytes: 1 << 20, maxActorBytes: 128},
	}
	change := durableTestChange(t, manifest, "alice", 1, 1)
	encoded, err := marshalChange(change)
	if err != nil {
		t.Fatal(err)
	}
	if err := group.publish(Peer{ID: "mallory"}, encoded, handler); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unauthorized direct publish = %v", err)
	}
	if err := group.publish(Peer{ID: "alice"}, encoded, handler); err != nil {
		t.Fatalf("direct publish = %v", err)
	}
	if err := group.publish(Peer{ID: "alice"}, encoded, handler); err != nil {
		t.Fatalf("direct duplicate publish = %v", err)
	}
	if err := group.publish(Peer{ID: "alice"}, mustMarshalChange(t, durableTestChange(t, manifest, "alice", 2, 2)), handler); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("direct overflow publish = %v", err)
	}
	if err := group.publish(Peer{ID: "alice"}, []byte{changeMessage}, handler); err == nil {
		t.Fatal("malformed direct publish succeeded")
	}
	rejecting, err := NewGroup(GroupConfig{Manifest: manifest, Validate: func([]byte) error { return errors.New("invalid concrete delta") }})
	if err != nil {
		t.Fatal(err)
	}
	if err := rejecting.publish(Peer{ID: "alice"}, encoded, handler); err == nil {
		t.Fatal("validator rejection publish succeeded")
	}
}

func mustMarshalChange(t *testing.T, change replica.Change) []byte {
	t.Helper()
	encoded, err := marshalChange(change)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
