package extensions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
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

func TestYJSStoreClientDoesNotFollowRedirects(t *testing.T) {
	token := strings.Repeat("r", 32)
	redirected := make(chan struct{}, 1)
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		redirected <- struct{}{}
		writeYJSStoreJSON(t, writer, yjsStateVectorResponse{StateVector: base64.StdEncoding.EncodeToString([]byte{1})})
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL, http.StatusFound)
	}))
	defer source.Close()

	store := testYJSStore(t, source.URL, token)
	if _, err := store.StateVector(context.Background(), testYJSDocument()); !errors.Is(err, ErrYJSStoreUnavailable) {
		t.Fatalf("redirected sidecar error = %v, want %v", err, ErrYJSStoreUnavailable)
	}
	select {
	case <-redirected:
		t.Fatal("YJSStore followed a redirect away from its configured endpoint")
	default:
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

func TestYJSStoreResponseBoundariesAndClose(t *testing.T) {
	closeCalls := 0
	client := &http.Client{Transport: yjsStoreRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: yjsStoreReadCloser{
				Reader: strings.NewReader(`{"stateVector":"` + base64.StdEncoding.EncodeToString([]byte{1}) + `"}`),
				close:  func() { closeCalls++ },
			},
		}, nil
	})}
	store := testYJSStoreWithClient(t, "http://127.0.0.1:8080", strings.Repeat("q", 32), client)
	if vector, err := store.StateVector(context.Background(), testYJSDocument()); err != nil || string(vector) != string([]byte{1}) {
		t.Fatalf("StateVector() = %x, %v", vector, err)
	}
	if closeCalls != 1 {
		t.Fatalf("response body Close calls = %d, want 1", closeCalls)
	}

	if _, err := readYJSStoreResponse(strings.NewReader("x"), 0); !errors.Is(err, ErrYJSStoreUnavailable) {
		t.Fatalf("non-positive response limit error = %v", err)
	}
	if _, err := readYJSStoreResponse(strings.NewReader("too-large"), 3); !errors.Is(err, ErrYJSStoreLimit) {
		t.Fatalf("oversized response error = %v", err)
	}
	if _, err := readYJSStoreResponse(yjsStoreErrorReader{}, 3); !errors.Is(err, ErrYJSStoreUnavailable) {
		t.Fatalf("response read failure error = %v", err)
	}
	if _, err := decodeYJSStoreBytes("", 1); !errors.Is(err, ErrYJSStoreLimit) {
		t.Fatalf("empty encoded bytes error = %v", err)
	}
	if _, err := decodeYJSStoreBytes(base64.StdEncoding.EncodeToString([]byte{1, 2}), 1); !errors.Is(err, ErrYJSStoreRejected) {
		t.Fatalf("oversized encoded bytes error = %v", err)
	}

	for _, fixture := range []struct {
		status int
		body   string
		want   error
	}{
		{http.StatusBadRequest, "not-json", ErrYJSStoreUnavailable},
		{http.StatusBadRequest, `{"code":"unexpected"}`, ErrYJSStoreRejected},
		{http.StatusServiceUnavailable, `{"code":"unexpected"}`, ErrYJSStoreUnavailable},
	} {
		if err := yjsStoreHTTPError("snapshot", fixture.status, []byte(fixture.body)); !errors.Is(err, fixture.want) {
			t.Fatalf("status=%d body=%q error = %v, want %v", fixture.status, fixture.body, err, fixture.want)
		}
	}
}

func TestYJSStoreRejectsMalformedSuccessResponsesAndLocalBounds(t *testing.T) {
	document := testYJSDocument()
	token := strings.Repeat("r", 32)
	for _, fixture := range []struct {
		name string
		path string
		body string
		call func(YJSStore) error
		want error
	}{
		{
			name: "apply-empty-vector",
			path: "/v1/yjs/apply",
			body: `{"applied":true,"cursor":1,"stateVector":""}`,
			call: func(store YJSStore) error {
				_, err := store.Apply(context.Background(), document, []byte{1})
				return err
			},
			want: ErrYJSStoreLimit,
		},
		{
			name: "diff-invalid-update",
			path: "/v1/yjs/diff",
			body: `{"update":"not-base64"}`,
			call: func(store YJSStore) error {
				_, err := store.Diff(context.Background(), document, []byte{1})
				return err
			},
			want: ErrYJSStoreRejected,
		},
		{
			name: "snapshot-empty-vector",
			path: "/v1/yjs/snapshot",
			body: `{"update":"AQ==","stateVector":"","cursor":1}`,
			call: func(store YJSStore) error {
				_, err := store.Snapshot(context.Background(), document)
				return err
			},
			want: ErrYJSStoreLimit,
		},
		{
			name: "snapshot-empty-update",
			path: "/v1/yjs/snapshot",
			body: `{"update":"","stateVector":"AQ==","cursor":1}`,
			call: func(store YJSStore) error {
				_, err := store.Snapshot(context.Background(), document)
				return err
			},
			want: ErrYJSStoreLimit,
		},
		{
			name: "merge-empty-update",
			path: "/v1/yjs/merge",
			body: `{"update":""}`,
			call: func(store YJSStore) error {
				_, err := store.Merge(context.Background(), document, [][]byte{{1}})
				return err
			},
			want: ErrYJSStoreLimit,
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != fixture.path {
					t.Fatalf("request path = %q, want %q", request.URL.Path, fixture.path)
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, fixture.body)
			}))
			defer server.Close()
			if err := fixture.call(testYJSStore(t, server.URL, token)); !errors.Is(err, fixture.want) {
				t.Fatalf("error = %v, want %v", err, fixture.want)
			}
		})
	}

	client := testYJSStore(t, "http://127.0.0.1:1", token).(*yjsStoreClient)
	if err := client.validateDocumentAndBytes(document, nil, 1, false); !errors.Is(err, ErrYJSStoreLimit) {
		t.Fatalf("empty document bytes error = %v", err)
	}
	if err := client.validateDocumentAndBytes(document, nil, 1, true); err != nil {
		t.Fatalf("allowed empty document bytes error = %v", err)
	}
	if err := client.validateUpdate(make([]byte, client.maxUpdateBytes+1)); !errors.Is(err, ErrYJSStoreLimit) {
		t.Fatalf("oversized update error = %v", err)
	}
	if err := client.validateUpdate([]byte{1}); err != nil {
		t.Fatalf("valid update error = %v", err)
	}
	if validYJSStoreToken("x" + string([]byte{0x7f}) + strings.Repeat("x", 30)) {
		t.Fatal("control-byte token was accepted")
	}
	if encodedYJSStoreBytes(0) != 0 {
		t.Fatal("zero byte count has a non-zero base64 size")
	}
}

func TestYJSStoreTransportFailuresAndMergeCapacityAreContained(t *testing.T) {
	document := testYJSDocument()
	store := testYJSStore(t, "http://127.0.0.1:1", strings.Repeat("s", 32))
	for _, operation := range []struct {
		name string
		call func() error
	}{
		{"apply", func() error { _, err := store.Apply(context.Background(), document, []byte{1}); return err }},
		{"diff", func() error { _, err := store.Diff(context.Background(), document, []byte{1}); return err }},
		{"snapshot", func() error { _, err := store.Snapshot(context.Background(), document); return err }},
		{"merge", func() error { _, err := store.Merge(context.Background(), document, [][]byte{{1}}); return err }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.call(); !errors.Is(err, ErrYJSStoreUnavailable) {
				t.Fatalf("transport error = %v, want %v", err, ErrYJSStoreUnavailable)
			}
		})
	}

	bounded, err := NewYJSStore(YJSStoreConfig{
		Endpoint:            "http://127.0.0.1:1",
		Token:               strings.Repeat("t", 32),
		MaxUpdateBytes:      128,
		MaxStateVectorBytes: 64,
		MaxSnapshotBytes:    128,
		MaxMergeUpdates:     4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bounded.Merge(context.Background(), document, [][]byte{make([]byte, 80), make([]byte, 80)}); !errors.Is(err, ErrYJSStoreLimit) {
		t.Fatalf("aggregate merge limit error = %v", err)
	}
	if _, err := bounded.Merge(context.Background(), document, [][]byte{nil}); !errors.Is(err, ErrYJSStoreLimit) {
		t.Fatalf("empty merge update error = %v", err)
	}
}

func testYJSStore(t testing.TB, endpoint, token string) YJSStore {
	t.Helper()
	return testYJSStoreWithClient(t, endpoint, token, nil)
}

func testYJSStoreWithClient(t testing.TB, endpoint, token string, client *http.Client) YJSStore {
	t.Helper()
	store, err := NewYJSStore(YJSStoreConfig{
		Endpoint:            endpoint,
		Token:               token,
		MaxUpdateBytes:      128,
		MaxStateVectorBytes: 64,
		MaxSnapshotBytes:    1024,
		MaxMergeUpdates:     4,
		HTTPClient:          client,
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
		_, _ = decodeYJSStoreBytes(encoded, maximum)
		_ = validYJSStoreIdentifier(encoded)
		_ = validYJSStoreToken(encoded)
	})
}

type yjsStoreRoundTripper func(*http.Request) (*http.Response, error)

func (roundTrip yjsStoreRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type yjsStoreReadCloser struct {
	io.Reader
	close func()
}

func (body yjsStoreReadCloser) Close() error {
	if body.close != nil {
		body.close()
	}
	return errors.New("close failed")
}

type yjsStoreErrorReader struct{}

func (yjsStoreErrorReader) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}
