package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/counter"
	frame "github.com/darkinno-tech/crdt/encoding"
)

func TestSynchronizeMergesDivergentAndMissingGCounterStates(t *testing.T) {
	local := newTestStore(t)
	remote := newTestStore(t)
	mustAddCounter(t, local, "orders", "web", 2)
	mustAddCounter(t, local, "local-only", "web", 7)
	mustAddCounter(t, remote, "orders", "warehouse", 3)
	mustAddCounter(t, remote, "remote-only", "mobile", 5)

	server := newTestHTTPServer(t, remote, "secret")
	defer server.Close()
	report, err := synchronize(server.Client(), server.URL, "secret", local)
	if err != nil {
		t.Fatal(err)
	}
	if report.AlreadyEqual || report.RemoteStatesFetched != 2 || report.LocalStateWrites != 2 || report.RemoteMergeRequests != 2 {
		t.Fatalf("sync report = %+v", report)
	}
	assertSameRoot(t, local, remote)
	for _, store := range []*stateStore{local, remote} {
		assertCounts(t, store, "orders", map[string]uint64{"web": 2, "warehouse": 3})
		assertCounts(t, store, "local-only", map[string]uint64{"web": 7})
		assertCounts(t, store, "remote-only", map[string]uint64{"mobile": 5})
	}
}

func TestSynchronizeSameRootAvoidsInventoryAndStateTransfer(t *testing.T) {
	local := newTestStore(t)
	remote := newTestStore(t)
	for _, store := range []*stateStore{local, remote} {
		mustAddCounter(t, store, "orders", "web", 4)
	}
	serverValue, err := newSyncServer(remote, "secret")
	if err != nil {
		t.Fatal(err)
	}
	counter := &requestCounter{handler: serverValue, paths: make(map[string]int)}
	server := httptest.NewServer(counter)
	defer server.Close()
	report, err := synchronize(server.Client(), server.URL, "secret", local)
	if err != nil {
		t.Fatal(err)
	}
	if !report.AlreadyEqual || report.FinalRoot == "" {
		t.Fatalf("sync report = %+v", report)
	}
	if got := counter.count("/v1/merkle/root"); got != 1 {
		t.Fatalf("root requests = %d, want 1", got)
	}
	if got := counter.count("/v1/merkle/inventory"); got != 0 {
		t.Fatalf("inventory requests = %d, want 0", got)
	}
}

func TestThreeReplicaPartitionRepairConverges(t *testing.T) {
	left, middle, right := newTestStore(t), newTestStore(t), newTestStore(t)
	mustAddCounter(t, left, "orders", "web", 2)
	mustAddCounter(t, middle, "orders", "warehouse", 3)
	mustAddCounter(t, right, "orders", "mobile", 5)
	leftServer := newTestHTTPServer(t, left, "secret")
	middleServer := newTestHTTPServer(t, middle, "secret")
	rightServer := newTestHTTPServer(t, right, "secret")
	defer leftServer.Close()
	defer middleServer.Close()
	defer rightServer.Close()

	if _, err := synchronize(leftServer.Client(), middleServer.URL, "secret", left); err != nil {
		t.Fatalf("left -> middle repair: %v", err)
	}
	if _, err := synchronize(middleServer.Client(), rightServer.URL, "secret", middle); err != nil {
		t.Fatalf("middle -> right repair: %v", err)
	}
	if _, err := synchronize(rightServer.Client(), leftServer.URL, "secret", right); err != nil {
		t.Fatalf("right -> left repair: %v", err)
	}
	for _, store := range []*stateStore{left, middle, right} {
		assertCounts(t, store, "orders", map[string]uint64{"web": 2, "warehouse": 3, "mobile": 5})
	}
	assertSameRoot(t, left, middle)
	assertSameRoot(t, middle, right)
}

func TestSynchronizeRejectsDigestMismatchedRemoteStateBeforeWrite(t *testing.T) {
	remoteState := mustCounterState(t, "remote", 3)
	badDigest := strings.Repeat("0", sha256.Size*2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get(tokenHeader) != "secret" {
			writeError(writer, http.StatusUnauthorized, fmt.Errorf("unauthorized"))
			return
		}
		switch request.URL.Path {
		case "/v1/merkle/root":
			writeJSON(writer, http.StatusOK, rootResponse{Version: apiVersion, Root: badDigest, Entries: 1})
		case "/v1/merkle/inventory":
			writeJSON(writer, http.StatusOK, inventoryResponse{Version: apiVersion, Root: badDigest, Entries: []inventoryEntry{{Key: "orders", SHA256: badDigest, TypeID: crdt.TypeIDGCounterState}}})
		case "/v1/state/orders":
			writer.Header().Set("X-CRDT-State-SHA256", badDigest)
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(remoteState)
		default:
			writeError(writer, http.StatusNotFound, fmt.Errorf("not found"))
		}
	}))
	defer server.Close()
	local := newTestStore(t)
	if _, err := synchronize(server.Client(), server.URL, "secret", local); err == nil {
		t.Fatal("synchronize accepted a state with an inventory digest mismatch")
	}
	if got := len(local.inventory().Entries); got != 0 {
		t.Fatalf("local state count = %d, want 0", got)
	}
}

func TestServerAuthenticationBoundsAndPaths(t *testing.T) {
	store := newTestStore(t)
	server, err := newSyncServer(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := httptest.NewRecorder()
	server.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/merkle/root", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}
	for _, path := range []string{"/v1/state/..", "/v1/state/orders/child", "/v1/state/unsafe%20name"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set(tokenHeader, "secret")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("path %q status = %d, want %d", path, response.Code, http.StatusNotFound)
		}
	}
	oversized := httptest.NewRequest(http.MethodPut, "/v1/state/orders", bytes.NewReader(bytes.Repeat([]byte{'x'}, store.options.maxStateBytes+1)))
	oversized.Header.Set(tokenHeader, "secret")
	overResponse := httptest.NewRecorder()
	server.ServeHTTP(overResponse, oversized)
	if overResponse.Code != http.StatusBadRequest {
		t.Fatalf("oversized state status = %d, want %d", overResponse.Code, http.StatusBadRequest)
	}
	if got := len(store.inventory().Entries); got != 0 {
		t.Fatalf("oversized state changed store entries to %d", got)
	}
}

func TestOpenStateStoreRejectsUnsupportedAndUnsafeFiles(t *testing.T) {
	directory := t.TempDir()
	unsupported, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDGSetState, Payload: []byte{0}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "unsupported.frame"), unsupported, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openStateStore(directory, defaultStoreOptions()); err == nil {
		t.Fatal("openStateStore accepted an unsupported state type")
	}
	if err := os.Remove(filepath.Join(directory, "unsupported.frame")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "unsafe name.frame"), mustCounterState(t, "web", 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openStateStore(directory, defaultStoreOptions()); err == nil {
		t.Fatal("openStateStore accepted an unsafe filename")
	}
}

func TestRunGCounterAddCreatesAndMergesState(t *testing.T) {
	directory := t.TempDir()
	var output bytes.Buffer
	if err := run([]string{"-mode", "gcounter-add", "-state-dir", directory, "-key", "orders", "-replica", "web", "-amount", "2"}, &output); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"-mode", "gcounter-add", "-state-dir", directory, "-key", "orders", "-replica", "warehouse", "-amount", "3"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	var inventory storeInventory
	if err := json.Unmarshal(output.Bytes(), &inventory); err != nil || len(inventory.Entries) != 1 {
		t.Fatalf("gcounter-add output = %q, %v", output.String(), err)
	}
	store, err := openStateStore(directory, defaultStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	assertCounts(t, store, "orders", map[string]uint64{"web": 2, "warehouse": 3})
}

func TestConcurrentSynchronizeRemainsConvergent(t *testing.T) {
	local := newTestStore(t)
	remote := newTestStore(t)
	mustAddCounter(t, remote, "orders", "mobile", 5)
	server := newTestHTTPServer(t, remote, "secret")
	defer server.Close()

	var group sync.WaitGroup
	errors := make(chan error, 8)
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := synchronize(server.Client(), server.URL, "secret", local)
			errors <- err
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertSameRoot(t, local, remote)
	assertCounts(t, local, "orders", map[string]uint64{"mobile": 5})
}

func BenchmarkSynchronizeSameRoot(b *testing.B) {
	local, remote := newBenchmarkStore(b), newBenchmarkStore(b)
	populateBenchmarkStore(b, local, 256)
	populateBenchmarkStore(b, remote, 256)
	server := newBenchmarkHTTPServer(b, remote)
	defer server.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		report, err := synchronize(server.Client(), server.URL, "secret", local)
		if err != nil || !report.AlreadyEqual {
			b.Fatalf("same-root synchronize = %+v, %v", report, err)
		}
	}
}

func BenchmarkSynchronizeSparseRepair(b *testing.B) {
	local, remote := newBenchmarkStore(b), newBenchmarkStore(b)
	populateBenchmarkStore(b, local, 256)
	populateBenchmarkStore(b, remote, 256)
	server := newBenchmarkHTTPServer(b, remote)
	defer server.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		b.StopTimer()
		mustAddCounterBenchmark(b, remote, "counter-000", "remote", 1)
		b.StartTimer()
		report, err := synchronize(server.Client(), server.URL, "secret", local)
		if err != nil || report.AlreadyEqual || report.RemoteStatesFetched != 1 {
			b.Fatalf("sparse synchronize = %+v, %v", report, err)
		}
	}
}

type requestCounter struct {
	handler http.Handler
	mu      sync.Mutex
	paths   map[string]int
}

func (counter *requestCounter) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	counter.mu.Lock()
	counter.paths[request.URL.Path]++
	counter.mu.Unlock()
	counter.handler.ServeHTTP(writer, request)
}

func (counter *requestCounter) count(path string) int {
	counter.mu.Lock()
	defer counter.mu.Unlock()
	return counter.paths[path]
}

func newTestStore(t *testing.T) *stateStore {
	t.Helper()
	store, err := openStateStore(t.TempDir(), defaultStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func newTestHTTPServer(t *testing.T, store *stateStore, token string) *httptest.Server {
	t.Helper()
	server, err := newSyncServer(store, token)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(server)
}

func mustAddCounter(t *testing.T, store *stateStore, key, replica string, amount uint64) {
	t.Helper()
	if err := addGCounter(store, key, replica, amount); err != nil {
		t.Fatal(err)
	}
}

func mustCounterState(t *testing.T, replica string, amount uint64) []byte {
	t.Helper()
	value, err := counter.NewGCounter(replica)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Increment(amount); err != nil {
		t.Fatal(err)
	}
	state, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func assertSameRoot(t *testing.T, left, right *stateStore) {
	t.Helper()
	leftRoot, rightRoot := left.inventory().Root, right.inventory().Root
	if leftRoot != rightRoot {
		t.Fatalf("Merkle root mismatch: %s != %s", leftRoot, rightRoot)
	}
}

func assertCounts(t *testing.T, store *stateStore, key string, want map[string]uint64) {
	t.Helper()
	state, _, err := store.state(key)
	if err != nil {
		t.Fatal(err)
	}
	value, err := counter.NewGCounter("assert")
	if err != nil {
		t.Fatal(err)
	}
	if err := value.UnmarshalBinaryWithLimits(state, store.options.decoderLimits()); err != nil {
		t.Fatal(err)
	}
	got := value.Counts()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("state %q counts = %#v, want %#v", key, got, want)
	}
}

func newBenchmarkStore(b *testing.B) *stateStore {
	b.Helper()
	store, err := openStateStore(b.TempDir(), defaultStoreOptions())
	if err != nil {
		b.Fatal(err)
	}
	return store
}

func newBenchmarkHTTPServer(b *testing.B, store *stateStore) *httptest.Server {
	b.Helper()
	server, err := newSyncServer(store, "secret")
	if err != nil {
		b.Fatal(err)
	}
	return httptest.NewServer(server)
}

func populateBenchmarkStore(b *testing.B, store *stateStore, count int) {
	b.Helper()
	for index := 0; index < count; index++ {
		mustAddCounterBenchmark(b, store, fmt.Sprintf("counter-%03d", index), "seed", uint64(index+1))
	}
}

func mustAddCounterBenchmark(b *testing.B, store *stateStore, key, replica string, amount uint64) {
	b.Helper()
	if err := addGCounter(store, key, replica, amount); err != nil {
		b.Fatal(err)
	}
}

func TestValidateListenAddressAndTarget(t *testing.T) {
	if err := validateListenAddress("0.0.0.0:49821", false); err == nil {
		t.Fatal("validateListenAddress accepted a public listener")
	}
	if err := validateListenAddress("127.0.0.1:49821", false); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTarget("ftp://example.test"); err == nil {
		t.Fatal("parseTarget accepted a non-HTTP target")
	}
	if got, err := parseTarget("https://example.test/path/"); err != nil || got != "https://example.test/path" {
		t.Fatalf("parseTarget() = %q, %v", got, err)
	}
}

func TestLoadTokenAndDigestValidation(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "token")
	if err := os.WriteFile(path, []byte(" secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := loadToken("", path); err != nil || got != "secret" {
		t.Fatalf("loadToken() = %q, %v", got, err)
	}
	if _, err := loadToken("one", path); err == nil {
		t.Fatal("loadToken accepted token and token-file together")
	}
	digest := sha256.Sum256([]byte("state"))
	if !validDigest(hex.EncodeToString(digest[:])) || validDigest("ABC") {
		t.Fatal("validDigest validation mismatch")
	}
}

func TestReadRequestBodyClosesAndRejectsEmpty(t *testing.T) {
	request := httptest.NewRequest(http.MethodPut, "/v1/state/orders", nil)
	if _, err := readRequestBody(request, 1); err == nil {
		t.Fatal("readRequestBody accepted an empty request")
	}
	if _, err := readBounded(bytes.NewBufferString("x"), 0); err == nil {
		t.Fatal("readBounded accepted a zero bound")
	}
}

func TestServerRespondsToStateRoundTrip(t *testing.T) {
	store := newTestStore(t)
	mustAddCounter(t, store, "orders", "web", 2)
	server := newTestHTTPServer(t, store, "secret")
	defer server.Close()
	client := &http.Client{Timeout: time.Second}
	inventory, err := fetchInventory(client, server.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := inventoryIndex(inventory)
	if err != nil {
		t.Fatal(err)
	}
	state, err := fetchState(client, server.URL, "secret", entries["orders"], store.options)
	if err != nil {
		t.Fatal(err)
	}
	if len(state) == 0 {
		t.Fatal("state response was empty")
	}
}
