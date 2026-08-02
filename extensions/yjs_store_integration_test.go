package extensions

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestYJSStoreNodeSidecarIntegration proves the Go client protocol against the
// pinned, real Yjs runtime. It is opt-in because ordinary Go-only consumers do
// not install Node dependencies; make yjs-store-test enables it in CI.
func TestYJSStoreNodeSidecarIntegration(t *testing.T) {
	store := startYJSStoreNodeSidecar(t, false)
	document := testYJSDocument()
	first, err := store.Apply(context.Background(), document, yjsHelloUpdate)
	if err != nil || !first.Applied || first.Cursor != 1 || len(first.StateVector) == 0 {
		t.Fatalf("first Apply = %#v, %v", first, err)
	}
	second, err := store.Apply(context.Background(), document, yjsHelloUpdate)
	if err != nil || second.Applied || second.Cursor != 1 {
		t.Fatalf("duplicate Apply = %#v, %v", second, err)
	}
	vector, err := store.StateVector(context.Background(), document)
	if err != nil || len(vector) == 0 {
		t.Fatalf("StateVector = %x, %v", vector, err)
	}
	delta, err := store.Diff(context.Background(), document, []byte{0}) // Y.encodeStateVector(new Y.Doc()).
	if err != nil || len(delta) == 0 {
		t.Fatalf("Diff = %x, %v", delta, err)
	}
	snapshot, err := store.Snapshot(context.Background(), document)
	if err != nil || snapshot.Cursor != 1 || len(snapshot.Update) == 0 || len(snapshot.StateVector) == 0 {
		t.Fatalf("Snapshot = %#v, %v", snapshot, err)
	}
	if _, err := store.Apply(context.Background(), document, []byte{255}); !errors.Is(err, ErrYJSStoreRejected) {
		t.Fatalf("invalid real Yjs update error = %v", err)
	}
}

// TestYJSHandlerNativeYWebsocketNodeIntegration runs two offline-native Yjs
// editors through the standard y-websocket provider, the Go relay, and the
// pinned durable Yjs engine. It then reconnects a fresh editor to prove that
// recovery comes from the durable semantic store rather than same-process
// BroadcastChannel delivery.
func TestYJSHandlerNativeYWebsocketNodeIntegration(t *testing.T) {
	store := startYJSStoreNodeSidecar(t, true)
	room, err := NewYJSRoom(YJSRoomConfig{
		Name:                "notes",
		MaxUpdateBytes:      4096,
		MaxStateVectorBytes: 1024,
		MaxSyncBytes:        32768,
		Store:               store,
		Document:            testYJSDocument(),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewYJSHandler(YJSConfig{
		Rooms: []*YJSRoom{room},
		Authenticate: func(request *http.Request) (Peer, error) {
			cookie, err := request.Cookie("yjs_session")
			if err != nil || cookie.Value != "native-yjs-editor" {
				return Peer{}, ErrUnauthorized
			}
			return Peer{ID: "native-yjs-editor"}, nil
		},
		Authorize:             func(Peer, string, YJSMessageKind) error { return nil },
		AuthorizeSubscription: func(Peer, string) error { return nil },
		MaxMessageBytes:       32768 + maxYJSWireOverhead,
		MaxQueuedMessages:     64,
		MaxQueuedBytes:        32768 + maxYJSWireOverhead,
		MaxAwarenessClients:   16,
		StoreTimeout:          5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	runtime := yjsStoreNodeRuntime(t, true)
	context, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(context, "node", "--no-experimental-webstorage", "relay.integration.mjs", "ws"+strings.TrimPrefix(server.URL, "http"))
	command.Dir = runtime
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("native y-websocket relay integration: %v\n%s", err, output)
	}
}

func startYJSStoreNodeSidecar(t *testing.T, needYWebsocket bool) YJSStore {
	t.Helper()
	runtime := yjsStoreNodeRuntime(t, needYWebsocket)
	dataDir := t.TempDir()
	if err := os.Chmod(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("n", 32)
	context, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(context, "node", "--no-experimental-webstorage", "server.mjs")
	command.Dir = runtime
	command.Env = append(os.Environ(),
		"YJS_STORE_DATA_DIR="+dataDir,
		"YJS_STORE_TOKEN="+token,
		"YJS_STORE_PORT=0",
		"YJS_STORE_MAX_UPDATE_BYTES=4096",
		"YJS_STORE_MAX_STATE_VECTOR_BYTES=1024",
		"YJS_STORE_MAX_SNAPSHOT_BYTES=32768",
		"YJS_STORE_MAX_MERGE_UPDATES=16",
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start Node YJSStore: %v", err)
	}
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Signal(os.Interrupt)
		}
		_ = command.Wait()
	})
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatalf("YJSStore did not report its listening endpoint: %v stderr=%s", scanner.Err(), stderr.String())
	}
	const prefix = "YJSStore listening on "
	endpoint := strings.TrimPrefix(scanner.Text(), prefix)
	if endpoint == scanner.Text() {
		t.Fatalf("unexpected YJSStore startup line %q stderr=%s", scanner.Text(), stderr.String())
	}
	store, err := NewYJSStore(YJSStoreConfig{
		Endpoint:            endpoint,
		Token:               token,
		MaxUpdateBytes:      4096,
		MaxStateVectorBytes: 1024,
		MaxSnapshotBytes:    32768,
		MaxMergeUpdates:     16,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func yjsStoreNodeRuntime(t *testing.T, needYWebsocket bool) string {
	t.Helper()
	if os.Getenv("CRDT_YJS_STORE_NODE_INTEGRATION") != "1" {
		t.Skip("set CRDT_YJS_STORE_NODE_INTEGRATION=1 after npm ci --prefix yjsstore/runtime")
	}
	runtime := filepath.Clean(filepath.Join("..", "yjsstore", "runtime"))
	modules := []string{"yjs"}
	if needYWebsocket {
		modules = append(modules, "y-websocket", "ws")
	}
	for _, module := range modules {
		if _, err := os.Stat(filepath.Join(runtime, "node_modules", module)); err != nil {
			t.Skipf("required Node module %s is not installed: %v", module, err)
		}
	}
	return runtime
}
