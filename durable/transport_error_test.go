package durable

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/replica"
	"github.com/coder/websocket"
	bolt "go.etcd.io/bbolt"
)

func TestStoreAdditionalLimitAndMetadataPaths(t *testing.T) {
	manifest := durableTestManifest(t)
	byteLimited := durableTestStore(t, t.TempDir()+"/relay.db", 4, 1)
	if _, err := byteLimited.Append(manifest.GroupID, durableTestChange(t, manifest, "alice", 1, 1)); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("byte limited append = %v", err)
	}
	if err := byteLimited.Close(); err != nil {
		t.Fatal(err)
	}
	parentFile := t.TempDir() + "/not-a-directory"
	if err := os.WriteFile(parentFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(parentFile+"/relay.db", StoreConfig{MaxEvents: 1, MaxBytes: 1}); err == nil {
		t.Fatal("file parent opened as directory")
	}
	insecure := t.TempDir() + "/insecure.db"
	if err := os.WriteFile(insecure, []byte("not-a-bbolt-file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(insecure, StoreConfig{MaxEvents: 1, MaxBytes: 1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("insecure store permissions = %v", err)
	}

	store := durableTestStore(t, t.TempDir()+"/meta.db", 4, 1<<20)
	defer func() { _ = store.Close() }()
	if err := store.db.Update(func(transaction *bolt.Tx) error {
		group, err := store.groupBucket(transaction, manifest.GroupID, true)
		if err != nil {
			return err
		}
		meta := group.Bucket(bucketMeta)
		return writeMeta(meta, 1, 2, 3)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.db.View(func(transaction *bolt.Tx) error {
		group, err := store.groupBucket(transaction, manifest.GroupID, false)
		if err != nil {
			return err
		}
		_, _, _, err = readMeta(group.Bucket(bucketMeta))
		return err
	}); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("inconsistent metadata = %v", err)
	}
	if _, _, err := store.Replay(manifest.GroupID, 0, 4, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrCorruptStore) {
		t.Fatalf("inconsistent metadata replay = %v", err)
	}
	fresh := durableTestStore(t, t.TempDir()+"/fresh.db", 4, 1<<20)
	defer func() { _ = fresh.Close() }()
	if events, highWater, err := fresh.Replay(manifest.GroupID, 0, 4, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); err != nil || len(events) != 0 || highWater != 0 {
		t.Fatalf("fresh replay events=%+v high=%d err=%v", events, highWater, err)
	}
	if _, err := fresh.Append(manifest.GroupID, durableTestChange(t, manifest, "alice", 1, 1)); err != nil {
		t.Fatal(err)
	}
	if events, highWater, err := fresh.Replay(manifest.GroupID, 1, 4, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); err != nil || len(events) != 0 || highWater != 1 {
		t.Fatalf("caught-up replay events=%+v high=%d err=%v", events, highWater, err)
	}
}

func TestServerRejectsMalformedAndIncompatibleHandshakes(t *testing.T) {
	manifest := durableTestManifest(t)
	store := durableTestStore(t, t.TempDir()+"/relay.db", 4, 1<<20)
	defer func() { _ = store.Close() }()
	handler, _ := durableTestHandler(t, store, manifest)
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	for _, hello := range [][]byte{
		[]byte(`{"version":1}`),
		mustHello(t, incompatibleManifest(t)),
	} {
		connection := rawDialOnly(t, endpoint)
		if err := connection.Write(context.Background(), websocket.MessageText, hello); err != nil {
			t.Fatal(err)
		}
		readContext, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _, err := connection.Read(readContext)
		cancel()
		if err == nil {
			t.Fatal("invalid handshake remained open")
		}
		_ = connection.CloseNow()
	}
}

func TestReconnectClientRejectsInvalidWelcomeAndRestoresWrite(t *testing.T) {
	manifest := durableTestManifest(t)
	invalidWelcome := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}})
		if err != nil {
			return
		}
		defer func() { _ = connection.CloseNow() }()
		if _, _, err := connection.Read(context.Background()); err == nil {
			_ = connection.Write(context.Background(), websocket.MessageText, []byte(`{"version":1,"code":"other"}`))
		}
	}))
	defer invalidWelcome.Close()
	invalidEndpoint := "ws" + strings.TrimPrefix(invalidWelcome.URL, "http")
	client := durableTestClient(t, invalidEndpoint, manifest, 0, func(Event) error { return nil })
	if err := client.Run(context.Background()); !errors.Is(err, errInvalidWire) {
		t.Fatalf("invalid welcome run = %v", err)
	}

	writeFailure := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}})
		if err != nil {
			return
		}
		defer func() { _ = connection.CloseNow() }()
		<-request.Context().Done()
	}))
	defer writeFailure.Close()
	endpoint := "ws" + strings.TrimPrefix(writeFailure.URL, "http")
	connection := rawDialOnly(t, endpoint)
	change := durableTestChange(t, manifest, "alice", 1, 1)
	client, err := NewReconnectClient("ws://example.test/ws", manifest, ClientConfig{OnEvent: func(Event) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	client.outbound <- change
	if err := connection.CloseNow(); err != nil {
		t.Fatal(err)
	}
	writeContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = client.writeLoop(writeContext, connection)
	if err == nil {
		t.Fatal("write loop succeeded against closed server")
	}
	restored, err := client.nextChange(context.Background())
	if err != nil || restored.Dot != change.Dot {
		t.Fatalf("restored write = %+v err=%v", restored, err)
	}
}

func TestServerPeerWritesSequencedEvent(t *testing.T) {
	manifest := durableTestManifest(t)
	received := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}})
		if err != nil {
			return
		}
		defer func() { _ = connection.CloseNow() }()
		_, data, err := connection.Read(context.Background())
		if err == nil {
			received <- data
		}
	}))
	defer server.Close()
	connection := rawDialOnly(t, "ws"+strings.TrimPrefix(server.URL, "http"))
	peer := newServerPeer(connection, 2, 1024, time.Second)
	event := Event{Sequence: 1, Change: durableTestChange(t, manifest, "alice", 1, 1)}
	if !peer.writeEvent(context.Background(), event) {
		t.Fatal("direct event write failed")
	}
	select {
	case data := <-received:
		if _, _, _, err := unmarshalEvent(data, 1<<20, 128); err != nil {
			t.Fatalf("event wire = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not receive event")
	}
	peer.close()
}

func rawDialOnly(t *testing.T, endpoint string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer alice"}},
		Subprotocols: []string{Subprotocol},
	})
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func mustHello(t *testing.T, manifest replica.Manifest) []byte {
	t.Helper()
	encoded, err := marshalHello(manifest, 0)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func incompatibleManifest(t *testing.T) replica.Manifest {
	t.Helper()
	manifest, err := replica.NewManifest("other-group", "example.com/durable-counter/v1", 1, replica.Protocol{
		StateID:          crdt.TypeIDGCounterState,
		DeltaID:          crdt.TypeIDGCounterDelta,
		SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}
