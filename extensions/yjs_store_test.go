package extensions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestYJSStoreClientContractAndLocalBounds(t *testing.T) {
	document := testYJSDocument()
	token := strings.Repeat("x", 32)
	called := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		called++
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer "+token || request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected request %s authorization=%q content-type=%q", request.Method, request.Header.Get("Authorization"), request.Header.Get("Content-Type"))
		}
		switch request.URL.Path {
		case "/v1/yjs/apply":
			var body yjsApplyRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Document != document.yjsJSON() || body.Update != base64.StdEncoding.EncodeToString([]byte{1, 2}) {
				t.Fatalf("apply body = %#v", body)
			}
			writeYJSStoreJSON(t, writer, yjsApplyResponse{Applied: true, Cursor: 3, StateVector: base64.StdEncoding.EncodeToString([]byte{3})})
		case "/v1/yjs/state-vector":
			writeYJSStoreJSON(t, writer, yjsStateVectorResponse{StateVector: base64.StdEncoding.EncodeToString([]byte{4})})
		case "/v1/yjs/diff":
			writeYJSStoreJSON(t, writer, yjsUpdateResponse{Update: base64.StdEncoding.EncodeToString([]byte{5, 6})})
		case "/v1/yjs/snapshot":
			writeYJSStoreJSON(t, writer, yjsSnapshotResponse{Update: base64.StdEncoding.EncodeToString([]byte{7}), StateVector: base64.StdEncoding.EncodeToString([]byte{8}), Cursor: 9})
		case "/v1/yjs/merge":
			var body yjsMergeRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.Updates) != 2 {
				t.Fatalf("merge count = %d", len(body.Updates))
			}
			writeYJSStoreJSON(t, writer, yjsUpdateResponse{Update: base64.StdEncoding.EncodeToString([]byte{9, 10})})
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()
	store := testYJSStore(t, server.URL, token)

	result, err := store.Apply(context.Background(), document, []byte{1, 2})
	if err != nil || !result.Applied || result.Cursor != 3 || string(result.StateVector) != string([]byte{3}) {
		t.Fatalf("Apply() = %#v, %v", result, err)
	}
	if vector, err := store.StateVector(context.Background(), document); err != nil || string(vector) != string([]byte{4}) {
		t.Fatalf("StateVector() = %x, %v", vector, err)
	}
	if update, err := store.Diff(context.Background(), document, []byte{1}); err != nil || string(update) != string([]byte{5, 6}) {
		t.Fatalf("Diff() = %x, %v", update, err)
	}
	if snapshot, err := store.Snapshot(context.Background(), document); err != nil || snapshot.Cursor != 9 || string(snapshot.Update) != string([]byte{7}) || string(snapshot.StateVector) != string([]byte{8}) {
		t.Fatalf("Snapshot() = %#v, %v", snapshot, err)
	}
	if update, err := store.Merge(context.Background(), document, [][]byte{{1}, {2}}); err != nil || string(update) != string([]byte{9, 10}) {
		t.Fatalf("Merge() = %x, %v", update, err)
	}
	if called != 5 {
		t.Fatalf("sidecar calls = %d, want 5", called)
	}

	if _, err := store.Apply(context.Background(), document, nil); !errors.Is(err, ErrYJSStoreLimit) {
		t.Fatalf("empty update error = %v", err)
	}
	if _, err := store.Diff(context.Background(), document, make([]byte, 65)); !errors.Is(err, ErrYJSStoreLimit) {
		t.Fatalf("oversized vector error = %v", err)
	}
	if _, err := store.Merge(context.Background(), document, [][]byte{{1}, {2}, {3}, {4}, {5}}); !errors.Is(err, ErrYJSStoreLimit) {
		t.Fatalf("oversized merge error = %v", err)
	}
	if called != 5 {
		t.Fatalf("invalid inputs called sidecar %d times", called)
	}
}

func TestYJSStoreClientRejectsUnsafeConfigurationAndMapsSidecarErrors(t *testing.T) {
	token := strings.Repeat("y", 32)
	for _, endpoint := range []string{"", "http://store.example", "ftp://localhost", "http://localhost/path", "https://example.com/?query=1"} {
		if _, err := NewYJSStore(YJSStoreConfig{Endpoint: endpoint, Token: token, MaxUpdateBytes: 128, MaxStateVectorBytes: 64, MaxSnapshotBytes: 1024, MaxMergeUpdates: 2}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("NewYJSStore(%q) error = %v", endpoint, err)
		}
	}
	if _, err := NewYJSStore(YJSStoreConfig{Endpoint: "http://localhost:8080", Token: "short", MaxUpdateBytes: 128, MaxStateVectorBytes: 64, MaxSnapshotBytes: 1024, MaxMergeUpdates: 2}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("short token error = %v", err)
	}

	for _, fixture := range []struct {
		status int
		code   string
		want   error
	}{
		{http.StatusUnauthorized, "unauthorized", ErrUnauthorized},
		{http.StatusRequestEntityTooLarge, "limit_exceeded", ErrYJSStoreLimit},
		{http.StatusBadRequest, "wrong_format", ErrYJSStoreRejected},
		{http.StatusInternalServerError, "corrupt_store", ErrYJSStoreUnavailable},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(fixture.status)
			_, _ = writer.Write([]byte(`{"code":"` + fixture.code + `"}`))
		}))
		store := testYJSStore(t, server.URL, token)
		_, err := store.StateVector(context.Background(), testYJSDocument())
		server.Close()
		if !errors.Is(err, fixture.want) {
			t.Fatalf("status=%d code=%s error = %v, want %v", fixture.status, fixture.code, err, fixture.want)
		}
	}
}

func TestYJSStoreDocumentIdentifiersAreStrict(t *testing.T) {
	store := testYJSStore(t, "http://127.0.0.1:1", strings.Repeat("z", 32))
	for _, document := range []YJSDocument{
		{Tenant: "tenant", Room: "notes", Epoch: 1, Schema: "schema", Format: ""},
		{Tenant: "tenant/other", Room: "notes", Epoch: 1, Schema: "schema", Format: YJSStoreFormatV1},
		{Tenant: "tenant", Room: "notes", Epoch: 1, Schema: ".schema", Format: YJSStoreFormatV1},
	} {
		if _, err := store.StateVector(context.Background(), document); !errors.Is(err, ErrYJSStoreRejected) {
			t.Fatalf("StateVector(%#v) error = %v", document, err)
		}
	}
}

func testYJSStore(t testing.TB, endpoint, token string) YJSStore {
	t.Helper()
	store, err := NewYJSStore(YJSStoreConfig{
		Endpoint:            endpoint,
		Token:               token,
		MaxUpdateBytes:      128,
		MaxStateVectorBytes: 64,
		MaxSnapshotBytes:    1024,
		MaxMergeUpdates:     4,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testYJSDocument() YJSDocument {
	return YJSDocument{Tenant: "tenant-a", Room: "notes", Epoch: 7, Schema: "prosemirror-v1", Format: YJSStoreFormatV1}
}

func writeYJSStoreJSON(t testing.TB, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func FuzzDecodeYJSStoreBytes(f *testing.F) {
	f.Add(base64.StdEncoding.EncodeToString([]byte{0, 1}), 64)
	f.Add("not-base64", 64)
	f.Add("", 1)
	f.Fuzz(func(t *testing.T, encoded string, maximum int) {
		maximum %= 1024
		_, _ = decodeYJSStoreBytes(encoded, maximum, false)
		_ = validYJSStoreIdentifier(encoded)
		_ = validYJSStoreToken(encoded)
	})
}
