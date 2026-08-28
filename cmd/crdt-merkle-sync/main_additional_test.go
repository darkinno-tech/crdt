package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/counter"
	frame "github.com/im10furry/crdt/encoding"
)

func TestStoreOptionsAndGCounterMergerBoundaries(t *testing.T) {
	defaults := frame.DefaultLimits()
	for _, options := range []storeOptions{
		{maxStateBytes: 0, maxObjects: 1, maxElements: 1, maxStringSize: 1},
		{maxStateBytes: defaults.MaxFrameBytes + 1, maxObjects: 1, maxElements: 1, maxStringSize: 1},
		{maxStateBytes: 1, maxObjects: 0, maxElements: 1, maxStringSize: 1},
		{maxStateBytes: 1, maxObjects: maximumObjects + 1, maxElements: 1, maxStringSize: 1},
		{maxStateBytes: 1, maxObjects: 1, maxElements: 0, maxStringSize: 1},
		{maxStateBytes: 1, maxObjects: 1, maxElements: defaults.MaxElements + 1, maxStringSize: 1},
		{maxStateBytes: 1, maxObjects: 1, maxElements: 1, maxStringSize: 0},
		{maxStateBytes: 1, maxObjects: 1, maxElements: 1, maxStringSize: defaults.MaxStringBytes + 1},
	} {
		if err := options.validate(); err == nil {
			t.Fatalf("options %+v were accepted", options)
		}
	}
	merger := gCounterMerger{}
	if err := merger.validate([]byte("invalid"), defaultStoreOptions().decoderLimits()); err == nil {
		t.Fatal("G-Counter merger accepted invalid state")
	}
	left := mustCounterState(t, "left", 2)
	right := mustCounterState(t, "right", 3)
	merged, err := merger.merge(left, right, defaultStoreOptions().decoderLimits())
	if err != nil {
		t.Fatal(err)
	}
	value, err := counter.NewGCounter("assert")
	if err != nil {
		t.Fatal(err)
	}
	if err := value.UnmarshalBinary(merged); err != nil {
		t.Fatal(err)
	}
	if got, want := value.Counts(), map[string]uint64{"left": 2, "right": 3}; !mapsEqual(got, want) {
		t.Fatalf("merged counts = %#v, want %#v", got, want)
	}
	if _, err := merger.merge(left, []byte("invalid"), defaultStoreOptions().decoderLimits()); err == nil {
		t.Fatal("G-Counter merger accepted invalid remote state")
	}
}

func TestStateStoreRejectsInvalidDirectoryEntriesAndBounds(t *testing.T) {
	if _, err := openStateStore("", defaultStoreOptions()); err == nil {
		t.Fatal("openStateStore accepted an empty path")
	}
	filePath := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openStateStore(filePath, defaultStoreOptions()); err == nil {
		t.Fatal("openStateStore accepted a regular file as a directory")
	}

	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "nested.frame"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := openStateStore(directory, defaultStoreOptions()); err == nil {
		t.Fatal("openStateStore accepted a nested directory")
	}
	if err := os.Remove(filepath.Join(directory, "nested.frame")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "one.frame"), mustCounterState(t, "one", 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "two.frame"), mustCounterState(t, "two", 1), 0o600); err != nil {
		t.Fatal(err)
	}
	options := defaultStoreOptions()
	options.maxObjects = 1
	if _, err := openStateStore(directory, options); err == nil {
		t.Fatal("openStateStore accepted too many states")
	}
}

func TestStateStoreMergeAndStateErrorPaths(t *testing.T) {
	store := newTestStore(t)
	if _, _, err := store.state("unsafe key"); err == nil {
		t.Fatal("state accepted an unsafe key")
	}
	if _, _, err := store.state("missing"); !errors.Is(err, errStateNotFound) {
		t.Fatalf("state missing error = %v", err)
	}
	if _, _, err := store.merge("orders", []byte("invalid")); err == nil {
		t.Fatal("merge accepted an invalid frame")
	}
	mustAddCounter(t, store, "orders", "web", 2)
	state, _, err := store.state("orders")
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.merge("orders", state); err != nil || changed {
		t.Fatalf("idempotent merge = changed=%t, err=%v", changed, err)
	}
	store.states["orders"] = stateRecord{data: state, typeID: crdt.TypeIDPNCounterState}
	if _, _, err := store.merge("orders", state); err == nil {
		t.Fatal("merge accepted an incompatible local state type")
	}
	store.states["orders"] = stateRecord{data: state, typeID: crdt.TypeIDGCounterState}
	store.options.maxObjects = 1
	if _, _, err := store.merge("another", mustCounterState(t, "other", 1)); err == nil {
		t.Fatal("merge accepted a state above max-objects")
	}

	badDirectory := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(badDirectory, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	store.dir = badDirectory
	if err := store.writeLocked("orders", state); err == nil {
		t.Fatal("writeLocked accepted a non-directory state path")
	}
}

func TestStateValidationAndInventoryValidationFailures(t *testing.T) {
	unsupported, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDPNCounterState, Payload: []byte{0}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateState(unsupported, defaultStoreOptions().decoderLimits()); err == nil {
		t.Fatal("validateState accepted an unsupported state")
	}
	if _, err := keyFromStateFilename("no-extension"); err == nil {
		t.Fatal("keyFromStateFilename accepted a missing extension")
	}
	if got, err := keyFromStateFilename("orders.frame"); err != nil || got != "orders" {
		t.Fatalf("keyFromStateFilename = %q, %v", got, err)
	}
	for _, inventory := range []inventoryResponse{
		{Version: 0, Root: strings.Repeat("0", 64)},
		{Version: apiVersion, Root: "not-a-root"},
		{Version: apiVersion, Root: strings.Repeat("0", 64), Entries: []inventoryEntry{{Key: "orders", SHA256: "bad", TypeID: crdt.TypeIDGCounterState}}},
		{Version: apiVersion, Root: strings.Repeat("0", 64), Entries: []inventoryEntry{{Key: "z", SHA256: strings.Repeat("0", 64), TypeID: crdt.TypeIDGCounterState}, {Key: "a", SHA256: strings.Repeat("0", 64), TypeID: crdt.TypeIDGCounterState}}},
		{Version: apiVersion, Root: strings.Repeat("0", 64), Entries: []inventoryEntry{{Key: "orders", SHA256: strings.Repeat("0", 64), TypeID: crdt.TypeIDPNCounterState}}},
	} {
		if _, err := inventoryIndex(inventory); err == nil {
			t.Fatalf("inventory %+v was accepted", inventory)
		}
	}
}

func TestRunAndServerConstructionFailures(t *testing.T) {
	directory := t.TempDir()
	for _, args := range [][]string{
		{"-unknown"},
		{"-state-dir", directory, "-timeout", "0"},
		{"-state-dir", directory},
		{"-mode", "gcounter-add", "-state-dir", directory},
		{"-mode", "sync", "-state-dir", directory},
		{"-mode", "sync", "-state-dir", directory, "-token", "secret", "-target", "not-a-url"},
		{"-mode", "serve", "-state-dir", directory},
		{"-mode", "serve", "-state-dir", directory, "-token", "secret", "-listen", "0.0.0.0:49821"},
		{"-mode", "gcounter-add", "-state-dir", directory, "-key", "orders", "-replica", "web", "-amount", "0"},
	} {
		if err := run(args, io.Discard); err == nil {
			t.Fatalf("run(%q) succeeded", args)
		}
	}
	store := newTestStore(t)
	if _, err := newSyncServer(nil, "secret"); err == nil {
		t.Fatal("newSyncServer accepted a nil store")
	}
	if _, err := newSyncServer(store, ""); err == nil {
		t.Fatal("newSyncServer accepted an empty token")
	}
	if err := serve("invalid", "secret", time.Second, false, store); err == nil {
		t.Fatal("serve accepted an invalid address")
	}
	if err := serve("127.0.0.1:0", "", time.Second, false, store); err == nil {
		t.Fatal("serve accepted an empty token")
	}

	store.states["orders"] = stateRecord{data: mustCounterState(t, "web", 1), typeID: crdt.TypeIDPNCounterState}
	if err := addGCounter(store, "orders", "web", 1); err == nil {
		t.Fatal("addGCounter accepted an incompatible existing state")
	}
}

func TestRunSyncProducesVerifiedReport(t *testing.T) {
	localDirectory := t.TempDir()
	local, err := openStateStore(localDirectory, defaultStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	mustAddCounter(t, local, "orders", "local", 2)
	remote := newTestStore(t)
	mustAddCounter(t, remote, "orders", "remote", 3)
	server := newTestHTTPServer(t, remote, "secret")
	defer server.Close()
	var output bytes.Buffer
	if err := run([]string{"-mode", "sync", "-state-dir", localDirectory, "-target", server.URL, "-token", "secret"}, &output); err != nil {
		t.Fatal(err)
	}
	var report syncReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil || report.FinalRoot == "" || report.AlreadyEqual {
		t.Fatalf("sync output = %q, report=%+v, err=%v", output.String(), report, err)
	}
	reloaded, err := openStateStore(localDirectory, defaultStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	assertSameRoot(t, reloaded, remote)
}

func TestServerStateMethodAndMissingStatePaths(t *testing.T) {
	store := newTestStore(t)
	server, err := newSyncServer(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/state/missing", nil),
		httptest.NewRequest(http.MethodDelete, "/v1/state/orders", nil),
		httptest.NewRequest(http.MethodGet, "/unknown", nil),
	} {
		request.Header.Set(tokenHeader, "secret")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound && response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s status = %d", request.Method, request.URL.Path, response.Code)
		}
	}
	if err := addGCounter(store, "unsafe key", "web", 1); err == nil {
		t.Fatal("addGCounter accepted an unsafe key")
	}
	if err := addGCounter(store, "orders", " ", 1); err == nil {
		t.Fatal("addGCounter accepted an invalid replica")
	}
	store.states["orders"] = stateRecord{data: []byte("invalid"), typeID: crdt.TypeIDGCounterState}
	if err := addGCounter(store, "orders", "web", 1); err == nil {
		t.Fatal("addGCounter accepted a corrupt stored state")
	}
}

func TestSynchronizeFailurePaths(t *testing.T) {
	if _, err := synchronize(nil, "http://example.test", "secret", newTestStore(t)); err == nil {
		t.Fatal("synchronize accepted a nil client")
	}
	if _, err := synchronize(&http.Client{}, "http://example.test", "secret", nil); err == nil {
		t.Fatal("synchronize accepted a nil store")
	}

	changedRootServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/merkle/root" {
			writeJSON(writer, http.StatusOK, rootResponse{Version: apiVersion, Root: strings.Repeat("0", 64), Entries: 0})
			return
		}
		writeJSON(writer, http.StatusOK, inventoryResponse{Version: apiVersion, Root: strings.Repeat("1", 64), Entries: nil})
	}))
	if _, err := synchronize(changedRootServer.Client(), changedRootServer.URL, "secret", newTestStore(t)); err == nil {
		changedRootServer.Close()
		t.Fatal("synchronize accepted a root change during inventory discovery")
	}
	changedRootServer.Close()

	malformedInventoryServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/merkle/root" {
			writeJSON(writer, http.StatusOK, rootResponse{Version: apiVersion, Root: strings.Repeat("0", 64), Entries: 1})
			return
		}
		writeJSON(writer, http.StatusOK, inventoryResponse{Version: apiVersion, Root: strings.Repeat("0", 64), Entries: []inventoryEntry{{Key: "orders", SHA256: strings.Repeat("0", 64), TypeID: crdt.TypeIDPNCounterState}}})
	}))
	if _, err := synchronize(malformedInventoryServer.Client(), malformedInventoryServer.URL, "secret", newTestStore(t)); err == nil {
		malformedInventoryServer.Close()
		t.Fatal("synchronize accepted an unsupported inventory type")
	}
	malformedInventoryServer.Close()

	inventoryFailureServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/merkle/root" {
			writeJSON(writer, http.StatusOK, rootResponse{Version: apiVersion, Root: strings.Repeat("0", 64), Entries: 1})
			return
		}
		writeError(writer, http.StatusInternalServerError, errors.New("inventory unavailable"))
	}))
	if _, err := synchronize(inventoryFailureServer.Client(), inventoryFailureServer.URL, "secret", newTestStore(t)); err == nil {
		inventoryFailureServer.Close()
		t.Fatal("synchronize accepted an inventory fetch failure")
	}
	inventoryFailureServer.Close()

	remote := newTestStore(t)
	mustAddCounter(t, remote, "one", "remote", 1)
	mustAddCounter(t, remote, "two", "remote", 1)
	remoteServer := newTestHTTPServer(t, remote, "secret")
	limited := newTestStore(t)
	limited.options.maxObjects = 1
	if _, err := synchronize(remoteServer.Client(), remoteServer.URL, "secret", limited); err == nil {
		remoteServer.Close()
		t.Fatal("synchronize exceeded the local object limit")
	}
	remoteServer.Close()

	local := newTestStore(t)
	mustAddCounter(t, local, "only-local", "local", 1)
	putFailureServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/merkle/root":
			writeJSON(writer, http.StatusOK, rootResponse{Version: apiVersion, Root: strings.Repeat("0", 64), Entries: 0})
		case "/v1/merkle/inventory":
			writeJSON(writer, http.StatusOK, inventoryResponse{Version: apiVersion, Root: strings.Repeat("0", 64), Entries: nil})
		default:
			writeError(writer, http.StatusInternalServerError, errors.New("write failed"))
		}
	}))
	if _, err := synchronize(putFailureServer.Client(), putFailureServer.URL, "secret", local); err == nil {
		putFailureServer.Close()
		t.Fatal("synchronize accepted a failed remote merge")
	}
	putFailureServer.Close()

	finalRemote := newTestStore(t)
	mustAddCounter(t, finalRemote, "remote", "remote", 1)
	initialInventory := finalRemote.inventory()
	rootRequests := 0
	finalMismatchServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/merkle/root":
			rootRequests++
			root := initialInventory.Root
			if rootRequests > 1 {
				root = strings.Repeat("0", 64)
			}
			writeJSON(writer, http.StatusOK, rootResponse{Version: apiVersion, Root: root, Entries: len(initialInventory.Entries)})
		case "/v1/merkle/inventory":
			writeJSON(writer, http.StatusOK, inventoryResponse{Version: apiVersion, Root: initialInventory.Root, Entries: initialInventory.Entries})
		case "/v1/state/remote":
			state, _, _ := finalRemote.state("remote")
			digest := sha256.Sum256(state)
			writer.Header().Set("X-CRDT-State-SHA256", hex.EncodeToString(digest[:]))
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(state)
		}
	}))
	if _, err := synchronize(finalMismatchServer.Client(), finalMismatchServer.URL, "secret", newTestStore(t)); err == nil {
		finalMismatchServer.Close()
		t.Fatal("synchronize accepted a final root mismatch")
	}
	finalMismatchServer.Close()

	finalFetchServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/merkle/root":
			writeError(writer, http.StatusInternalServerError, errors.New("root unavailable"))
		default:
			writeError(writer, http.StatusInternalServerError, errors.New("unavailable"))
		}
	}))
	if _, err := synchronize(finalFetchServer.Client(), finalFetchServer.URL, "secret", newTestStore(t)); err == nil {
		finalFetchServer.Close()
		t.Fatal("synchronize accepted a root fetch failure")
	}
	finalFetchServer.Close()
}

func TestHTTPClientFailurePaths(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/merkle/root":
			writeJSON(writer, http.StatusOK, rootResponse{Version: 0, Root: strings.Repeat("0", 64)})
		case "/v1/merkle/inventory":
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte("not-json"))
		case "/v1/state/orders":
			writeError(writer, http.StatusInternalServerError, errors.New("failed"))
		default:
			writeError(writer, http.StatusInternalServerError, errors.New("failed"))
		}
	}))
	defer server.Close()
	client := server.Client()
	if _, err := fetchRoot(client, server.URL, "secret"); err == nil {
		t.Fatal("fetchRoot accepted an unsupported version")
	}
	if _, err := fetchInventory(client, server.URL, "secret"); err == nil {
		t.Fatal("fetchInventory accepted malformed JSON")
	}
	entry := inventoryEntry{Key: "orders", SHA256: strings.Repeat("0", 64), TypeID: crdt.TypeIDGCounterState}
	if _, err := fetchState(client, server.URL, "secret", entry, defaultStoreOptions()); err == nil {
		t.Fatal("fetchState accepted a failing server")
	}
	if err := putState(client, server.URL, "secret", "orders", mustCounterState(t, "web", 1)); err == nil {
		t.Fatal("putState accepted a failing server")
	}
	if err := getJSON(client, server.URL+"/missing", "secret", &rootResponse{}); err == nil {
		t.Fatal("getJSON accepted a failing server")
	}
}

func TestHTTPHelperConstructionAndValidationFailures(t *testing.T) {
	client := &http.Client{Transport: errorRoundTripper{err: errors.New("transport")}}
	entry := inventoryEntry{Key: "orders", SHA256: strings.Repeat("0", 64), TypeID: crdt.TypeIDGCounterState}
	if _, err := fetchRoot(client, "http://example.test", "secret"); err == nil {
		t.Fatal("fetchRoot accepted a transport error")
	}
	if _, err := fetchInventory(client, "http://example.test", "secret"); err == nil {
		t.Fatal("fetchInventory accepted a transport error")
	}
	if _, err := fetchState(client, "http://example.test", "secret", entry, defaultStoreOptions()); err == nil {
		t.Fatal("fetchState accepted a transport error")
	}
	if err := putState(client, "http://example.test", "secret", "orders", mustCounterState(t, "web", 1)); err == nil {
		t.Fatal("putState accepted a transport error")
	}
	if err := getJSON(client, "http://example.test", "secret", &rootResponse{}); err == nil {
		t.Fatal("getJSON accepted a transport error")
	}
	if _, err := fetchState(http.DefaultClient, "%", "secret", entry, defaultStoreOptions()); err == nil {
		t.Fatal("fetchState accepted an invalid target")
	}
	if err := putState(http.DefaultClient, "%", "secret", "orders", mustCounterState(t, "web", 1)); err == nil {
		t.Fatal("putState accepted an invalid target")
	}
	if err := getJSON(http.DefaultClient, "%", "secret", &rootResponse{}); err == nil {
		t.Fatal("getJSON accepted an invalid endpoint")
	}

	validState := mustCounterState(t, "web", 1)
	digest := sha256.Sum256(validState)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-CRDT-State-SHA256", hex.EncodeToString(digest[:]))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(validState)
	}))
	defer server.Close()
	if _, err := fetchState(server.Client(), server.URL, "secret", inventoryEntry{Key: "orders", SHA256: hex.EncodeToString(digest[:]), TypeID: crdt.TypeIDPNCounterState}, defaultStoreOptions()); err == nil {
		t.Fatal("fetchState accepted a mismatched state type")
	}
	invalidState := []byte("invalid")
	invalidDigest := sha256.Sum256(invalidState)
	invalidServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-CRDT-State-SHA256", hex.EncodeToString(invalidDigest[:]))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(invalidState)
	}))
	defer invalidServer.Close()
	if _, err := fetchState(invalidServer.Client(), invalidServer.URL, "secret", inventoryEntry{Key: "orders", SHA256: hex.EncodeToString(invalidDigest[:]), TypeID: crdt.TypeIDGCounterState}, defaultStoreOptions()); err == nil {
		t.Fatal("fetchState accepted a malformed state body")
	}
}

func TestBodyAndTokenFailurePaths(t *testing.T) {
	if got, err := loadToken("secret", ""); err != nil || got != "secret" {
		t.Fatalf("loadToken value = %q, %v", got, err)
	}
	if _, err := readLimited(bytes.NewBufferString("xx"), 1); err == nil {
		t.Fatal("readLimited accepted an oversized body")
	}
	if _, err := readLimited(errorReader{}, 1); err == nil {
		t.Fatal("readLimited accepted a reader error")
	}
	response := &http.Response{Body: failingBody{Reader: bytes.NewReader([]byte("x")), closeErr: errors.New("close")}}
	if _, err := readHTTPBody(response, 1); err == nil {
		t.Fatal("readHTTPBody accepted a close failure")
	}
	request := httptest.NewRequest(http.MethodPut, "/v1/state/orders", nil)
	request.Body = failingBody{Reader: bytes.NewReader([]byte("x")), closeErr: errors.New("close")}
	if _, err := readRequestBody(request, 1); err == nil {
		t.Fatal("readRequestBody accepted a close failure")
	}
	if _, err := loadToken("", ""); err == nil {
		t.Fatal("loadToken accepted an empty token")
	}
	path := filepath.Join(t.TempDir(), "long-token")
	if err := os.WriteFile(path, bytes.Repeat([]byte{'t'}, maximumTokenBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken("", path); err == nil {
		t.Fatal("loadToken accepted an oversized token file")
	}
	emptyPath := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedFile(emptyPath, 1); err == nil {
		t.Fatal("readBoundedFile accepted an empty file")
	}
	if _, err := readBoundedFile(filepath.Join(t.TempDir(), "missing"), 1); err == nil {
		t.Fatal("readBoundedFile accepted a missing file")
	}
	if err := validateListenAddress("localhost:49821", false); err != nil {
		t.Fatal(err)
	}
	if err := validateListenAddress("0.0.0.0:49821", true); err != nil {
		t.Fatal(err)
	}
	if err := validateListenAddress("not-an-ip:49821", false); err == nil {
		t.Fatal("validateListenAddress accepted a non-loopback hostname")
	}
}

type failingBody struct {
	*bytes.Reader
	closeErr error
}

func (body failingBody) Close() error { return body.closeErr }

type errorRoundTripper struct{ err error }

func (roundTripper errorRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, roundTripper.err
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("read") }

func mapsEqual(left, right map[string]uint64) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}
