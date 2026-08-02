package extensions

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestYJSAgentPeerReadsPublishesAndRevalidates(t *testing.T) {
	store := newAgentYJSStore()
	room, err := NewYJSRoom(YJSRoomConfig{
		Name:                "notes",
		MaxUpdateBytes:      4096,
		MaxStateVectorBytes: 64,
		MaxSyncBytes:        4096,
		Store:               store,
		Document:            testYJSDocument(),
	})
	if err != nil {
		t.Fatal(err)
	}
	allowRead, allowWrite := true, true
	var authorizationMu sync.Mutex
	handler, err := NewYJSHandler(YJSConfig{
		Rooms:        []*YJSRoom{room},
		Authenticate: func(*http.Request) (Peer, error) { return Peer{ID: "browser"}, nil },
		Authorize: func(peer Peer, name string, kind YJSMessageKind) error {
			authorizationMu.Lock()
			defer authorizationMu.Unlock()
			if peer.ID != "agent:copy-editor:run-7" || name != "notes" || kind != YJSUpdate || !allowWrite {
				return ErrUnauthorized
			}
			return nil
		},
		AuthorizeSubscription: func(peer Peer, name string) error {
			authorizationMu.Lock()
			defer authorizationMu.Unlock()
			if name != "notes" || (peer.ID == "agent:copy-editor:run-7" && !allowRead) || (peer.ID != "agent:copy-editor:run-7" && peer.ID != "browser") {
				return ErrUnauthorized
			}
			return nil
		},
		MaxMessageBytes:     4096 + maxYJSWireOverhead,
		MaxQueuedMessages:   8,
		MaxQueuedBytes:      4096 + maxYJSWireOverhead,
		MaxAwarenessClients: 8,
		StoreTimeout:        time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(handler)
	defer server.Close()
	observer := newYJSTestClient(t, server.URL, "browser")
	defer func() { _ = observer.CloseNow() }()
	if message := readYJSMessage(t, observer); !bytes.Equal(message, marshalYJSSync(yjsWireSyncStep1, store.snapshot.StateVector)) {
		t.Fatalf("observer bootstrap = %x", message)
	}

	agent, err := handler.OpenYJSAgentPeer(Peer{ID: "agent:copy-editor:run-7"}, "notes")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := agent.Snapshot(context.Background())
	if err != nil || !bytes.Equal(snapshot.Update, store.snapshot.Update) || !bytes.Equal(snapshot.StateVector, store.snapshot.StateVector) || snapshot.Cursor != store.snapshot.Cursor {
		t.Fatalf("Snapshot = %#v, %v", snapshot, err)
	}
	delta, err := agent.Diff(context.Background(), []byte{0})
	if err != nil || !bytes.Equal(delta, store.delta) {
		t.Fatalf("Diff = %x, %v", delta, err)
	}

	result, err := agent.Publish(context.Background(), yjsHelloUpdate)
	if err != nil || !result.Applied || result.Cursor != 8 {
		t.Fatalf("Publish = %#v, %v", result, err)
	}
	if message := readYJSMessage(t, observer); !bytes.Equal(message, marshalYJSSync(yjsWireUpdate, yjsHelloUpdate)) {
		t.Fatalf("agent broadcast = %x", message)
	}
	retry, err := agent.Publish(context.Background(), yjsHelloUpdate)
	if err != nil || retry.Applied || retry.Cursor != result.Cursor {
		t.Fatalf("duplicate Publish = %#v, %v", retry, err)
	}

	authorizationMu.Lock()
	allowWrite = false
	authorizationMu.Unlock()
	if _, err := agent.Publish(context.Background(), []byte{1}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked publish error = %v", err)
	}
	authorizationMu.Lock()
	allowRead = false
	authorizationMu.Unlock()
	if _, err := agent.Snapshot(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked snapshot error = %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.applyCalls != 2 || store.snapshotCalls != 1 || store.diffCalls != 1 || store.document != room.document || !bytes.Equal(store.remoteVector, []byte{0}) {
		t.Fatalf("store calls apply=%d snapshot=%d diff=%d document=%#v vector=%x", store.applyCalls, store.snapshotCalls, store.diffCalls, store.document, store.remoteVector)
	}
}

func TestYJSAgentPeerRejectsUnsafeBoundaries(t *testing.T) {
	opaque, err := NewYJSRoom(YJSRoomConfig{Name: "opaque", MaxUpdateBytes: 32, MaxHistoryBytes: 32, MaxUpdates: 1})
	if err != nil {
		t.Fatal(err)
	}
	storeRoom, err := NewYJSRoom(YJSRoomConfig{
		Name:                "stored",
		MaxUpdateBytes:      32,
		MaxStateVectorBytes: 8,
		MaxSyncBytes:        32,
		Store:               newAgentYJSStore(),
		Document:            testYJSDocument(),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewYJSHandler(YJSConfig{
		Rooms:                 []*YJSRoom{opaque, storeRoom},
		Authenticate:          func(*http.Request) (Peer, error) { return Peer{ID: "browser"}, nil },
		Authorize:             func(Peer, string, YJSMessageKind) error { return nil },
		AuthorizeSubscription: func(Peer, string) error { return nil },
		MaxMessageBytes:       1024,
		MaxQueuedMessages:     1,
		MaxQueuedBytes:        1024,
		MaxAwarenessClients:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (*YJSHandler)(nil).OpenYJSAgentPeer(Peer{ID: "agent"}, "stored"); !errors.Is(err, ErrYJSAgentPeerUnavailable) {
		t.Fatalf("nil handler error = %v", err)
	}
	if _, err := handler.OpenYJSAgentPeer(Peer{}, "stored"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("blank identity error = %v", err)
	}
	if _, err := handler.OpenYJSAgentPeer(Peer{ID: "agent"}, "missing"); !errors.Is(err, ErrYJSAgentPeerUnavailable) {
		t.Fatalf("unknown room error = %v", err)
	}
	if _, err := handler.OpenYJSAgentPeer(Peer{ID: "agent"}, "opaque"); !errors.Is(err, ErrYJSAgentPeerUnsupported) {
		t.Fatalf("opaque room error = %v", err)
	}
	agent, err := handler.OpenYJSAgentPeer(Peer{ID: "agent"}, "stored")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Diff(context.Background(), nil); !errors.Is(err, ErrYJSStoreLimit) {
		t.Fatalf("empty state vector error = %v", err)
	}
	if _, err := agent.Publish(context.Background(), make([]byte, 33)); !errors.Is(err, ErrYJSStoreLimit) {
		t.Fatalf("oversized update error = %v", err)
	}
	if _, err := agent.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot with a nil context = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := agent.Snapshot(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot error = %v", err)
	}
}

// BenchmarkYJSAgentPeerDuplicatePublish measures only the in-process agent
// admission path. The fake store deliberately reports a durable duplicate, so
// this excludes Node, disk, TLS, authorization storage, and live fan-out; use
// make yjs-store-benchmark for the real sidecar apply/diff/snapshot workload.
func BenchmarkYJSAgentPeerDuplicatePublish(b *testing.B) {
	store := newAgentYJSStore()
	room, err := NewYJSRoom(YJSRoomConfig{
		Name:                "notes",
		MaxUpdateBytes:      4096,
		MaxStateVectorBytes: 64,
		MaxSyncBytes:        4096,
		Store:               store,
		Document:            testYJSDocument(),
	})
	if err != nil {
		b.Fatal(err)
	}
	handler, err := NewYJSHandler(YJSConfig{
		Rooms:                 []*YJSRoom{room},
		Authenticate:          func(*http.Request) (Peer, error) { return Peer{ID: "browser"}, nil },
		Authorize:             func(Peer, string, YJSMessageKind) error { return nil },
		AuthorizeSubscription: func(Peer, string) error { return nil },
		MaxMessageBytes:       4096 + maxYJSWireOverhead,
		MaxQueuedMessages:     1,
		MaxQueuedBytes:        4096 + maxYJSWireOverhead,
		MaxAwarenessClients:   1,
		StoreTimeout:          time.Second,
	})
	if err != nil {
		b.Fatal(err)
	}
	agent, err := handler.OpenYJSAgentPeer(Peer{ID: "agent:benchmark"}, "notes")
	if err != nil {
		b.Fatal(err)
	}
	if _, err := agent.Publish(context.Background(), yjsHelloUpdate); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		result, err := agent.Publish(context.Background(), yjsHelloUpdate)
		if err != nil || result.Applied {
			b.Fatalf("duplicate Publish = %#v, %v", result, err)
		}
	}
}

type agentYJSStore struct {
	mu            sync.Mutex
	snapshot      YJSSnapshot
	delta         []byte
	document      YJSDocument
	remoteVector  []byte
	updates       map[string]struct{}
	applyCalls    int
	snapshotCalls int
	diffCalls     int
}

func newAgentYJSStore() *agentYJSStore {
	return &agentYJSStore{
		snapshot: YJSSnapshot{Update: append([]byte(nil), yjsHelloUpdate...), StateVector: []byte{1}, Cursor: 7},
		delta:    append([]byte(nil), yjsHelloUpdate...),
		updates:  make(map[string]struct{}),
	}
}

func (store *agentYJSStore) Apply(_ context.Context, document YJSDocument, update []byte) (YJSApplyResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.document = document
	store.applyCalls++
	if _, exists := store.updates[string(update)]; exists {
		return YJSApplyResult{Applied: false, Cursor: 8, StateVector: []byte{2}}, nil
	}
	store.updates[string(update)] = struct{}{}
	return YJSApplyResult{Applied: true, Cursor: 8, StateVector: []byte{2}}, nil
}

func (store *agentYJSStore) StateVector(_ context.Context, document YJSDocument) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.document = document
	return append([]byte(nil), store.snapshot.StateVector...), nil
}

func (store *agentYJSStore) Diff(_ context.Context, document YJSDocument, remoteVector []byte) ([]byte, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.document = document
	store.diffCalls++
	store.remoteVector = append([]byte(nil), remoteVector...)
	return append([]byte(nil), store.delta...), nil
}

func (store *agentYJSStore) Snapshot(_ context.Context, document YJSDocument) (YJSSnapshot, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.document = document
	store.snapshotCalls++
	return YJSSnapshot{
		Update:      append([]byte(nil), store.snapshot.Update...),
		StateVector: append([]byte(nil), store.snapshot.StateVector...),
		Cursor:      store.snapshot.Cursor,
	}, nil
}

func (store *agentYJSStore) Merge(context.Context, YJSDocument, [][]byte) ([]byte, error) {
	return nil, ErrYJSStoreUnavailable
}
