package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DarkInno/crdt/text"
)

type testRoundTripper struct {
	response *http.Response
	err      error
}

func (transport testRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return transport.response, transport.err
}

type testReadCloser struct {
	reader   *strings.Reader
	readErr  error
	closeErr error
}

func (body *testReadCloser) Read(data []byte) (int, error) {
	if body.readErr != nil {
		return 0, body.readErr
	}
	return body.reader.Read(data)
}

func (body *testReadCloser) Close() error {
	return body.closeErr
}

func TestRunRejectsUnsafeOrIncompleteConfiguration(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing token", args: []string{"-mode", "send", "-replica", "sender", "-target", "http://example.test"}},
		{name: "missing replica", args: []string{"-mode", "send", "-token", "secret", "-target", "http://example.test"}},
		{name: "missing send target", args: []string{"-mode", "send", "-token", "secret", "-replica", "sender"}},
		{name: "public listener", args: []string{"-mode", "serve", "-token", "secret", "-replica", "sender", "-listen", "0.0.0.0:49801"}},
		{name: "invalid mode", args: []string{"-mode", "unknown", "-token", "secret", "-replica", "sender"}},
		{name: "invalid users", args: []string{"-mode", "send", "-token", "secret", "-replica", "sender", "-target", "http://example.test", "-users", "0"}},
		{name: "invalid jitter", args: []string{"-mode", "send", "-token", "secret", "-replica", "sender", "-target", "http://example.test", "-jitter-min", "2ms", "-jitter-max", "1ms"}},
		{name: "invalid flag", args: []string{"-unknown"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := run(test.args); err == nil {
				t.Fatal("run succeeded, want error")
			}
		})
	}
}

func TestLoadTokenBoundaries(t *testing.T) {
	directory := t.TempDir()
	valid := filepath.Join(directory, "valid")
	if err := os.WriteFile(valid, []byte("  secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(directory, "oversized")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("a", 1025)), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(directory, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		value string
		path  string
		want  string
	}{
		{name: "inline", value: "secret", want: "secret"},
		{name: "file", path: valid, want: "secret"},
		{name: "both", value: "secret", path: valid},
		{name: "empty", path: empty},
		{name: "oversized", path: oversized},
		{name: "missing", path: filepath.Join(directory, "missing")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := loadToken(test.value, test.path)
			if test.want == "" {
				if err == nil {
					t.Fatal("loadToken succeeded, want error")
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("loadToken = %q, %v; want %q, nil", got, err, test.want)
			}
		})
	}
}

func TestRunSendsWithLightweightAcknowledgements(t *testing.T) {
	receiver, err := newClusterServer("receiver", "secret")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(receiver)
	defer server.Close()
	if err := run([]string{
		"-mode", "send",
		"-token", "secret",
		"-replica", "sender",
		"-target", server.URL,
		"-users", "1",
		"-insert-runes", "2",
		"-duplicates", "1",
		"-jitter-min", "1ms",
		"-jitter-max", "1ms",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestClusterServerRejectsMalformedAndIncompleteState(t *testing.T) {
	server, err := newClusterServer("receiver", "secret")
	if err != nil {
		t.Fatal(err)
	}
	bad := httptest.NewRequest(http.MethodPost, "/rga", strings.NewReader("not-a-frame"))
	bad.Header.Set("X-CRDT-Cluster-Token", "secret")
	badResponse := httptest.NewRecorder()
	server.ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusBadRequest {
		t.Fatalf("malformed status = %d, want %d", badResponse.Code, http.StatusBadRequest)
	}

	source, err := text.New("source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Insert(0, "a"); err != nil {
		t.Fatal(err)
	}
	second, err := source.Insert(1, "b")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := second.MarshalBinaryWithLimits(frameLimits())
	if err != nil {
		t.Fatal(err)
	}
	pending := httptest.NewRequest(http.MethodPost, "/rga", bytes.NewReader(encoded))
	pending.Header.Set("X-CRDT-Cluster-Token", "secret")
	pendingResponse := httptest.NewRecorder()
	server.ServeHTTP(pendingResponse, pending)
	if pendingResponse.Code != http.StatusNoContent {
		t.Fatalf("pending status = %d, want %d", pendingResponse.Code, http.StatusNoContent)
	}
	state := httptest.NewRequest(http.MethodGet, "/state", nil)
	state.Header.Set("X-CRDT-Cluster-Token", "secret")
	stateResponse := httptest.NewRecorder()
	server.ServeHTTP(stateResponse, state)
	if stateResponse.Code != http.StatusInternalServerError {
		t.Fatalf("incomplete state status = %d, want %d", stateResponse.Code, http.StatusInternalServerError)
	}

	notFound := httptest.NewRequest(http.MethodDelete, "/rga", nil)
	notFound.Header.Set("X-CRDT-Cluster-Token", "secret")
	notFoundResponse := httptest.NewRecorder()
	server.ServeHTTP(notFoundResponse, notFound)
	if notFoundResponse.Code != http.StatusNotFound {
		t.Fatalf("not found status = %d, want %d", notFoundResponse.Code, http.StatusNotFound)
	}
}

func TestHTTPAndTimingHelpersRejectInvalidResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/ack":
			writer.Header().Set(applyDurationHeader, "12")
			writer.WriteHeader(http.StatusNoContent)
		case "/missing-timing":
			writer.WriteHeader(http.StatusNoContent)
		case "/failure":
			writer.WriteHeader(http.StatusTeapot)
		case "/state":
			_ = json.NewEncoder(writer).Encode(clusterState{CanonicalSHA256: "state", TextSHA256: "text", Runes: 1})
		case "/bad-state":
			_, _ = writer.Write([]byte("not-json"))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := server.Client()
	if got, err := postDelta(client, server.URL+"/ack", "secret", []byte("data")); err != nil || got != 12*time.Microsecond {
		t.Fatalf("post ack = %v, %v", got, err)
	}
	for _, path := range []string{"/missing-timing", "/failure"} {
		if _, err := postDelta(client, server.URL+path, "secret", []byte("data")); err == nil {
			t.Fatalf("post %s succeeded, want error", path)
		}
	}
	state, err := fetchState(client, server.URL, "secret")
	if err != nil || state.Runes != 1 {
		t.Fatalf("fetch state = %+v, %v", state, err)
	}
	if _, err := fetchState(client, server.URL+"/bad-state", "secret"); err == nil {
		t.Fatal("bad state fetch succeeded, want error")
	}
	badStateServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("not-json"))
	}))
	defer badStateServer.Close()
	if _, err := fetchState(badStateServer.Client(), badStateServer.URL, "secret"); err == nil {
		t.Fatal("malformed state fetch succeeded, want error")
	}
	if _, err := postDelta(client, ":", "secret", []byte("data")); err == nil {
		t.Fatal("invalid post endpoint succeeded, want error")
	}
	if _, err := fetchState(client, ":", "secret"); err == nil {
		t.Fatal("invalid state endpoint succeeded, want error")
	}
	for _, value := range []string{"", "-1", "overflow", "9223372036854776"} {
		if _, err := parseApplyLatency(value); err == nil {
			t.Fatalf("parseApplyLatency(%q) succeeded, want error", value)
		}
	}
}

func TestTransportAndBodyFailures(t *testing.T) {
	boom := errors.New("transport or body failure")
	client := &http.Client{Transport: testRoundTripper{err: boom}}
	if _, err := postDelta(client, "http://cluster.test/rga", "secret", []byte("delta")); !errors.Is(err, boom) {
		t.Fatalf("post transport error = %v, want %v", err, boom)
	}
	if _, err := fetchState(client, "http://cluster.test", "secret"); !errors.Is(err, boom) {
		t.Fatalf("state transport error = %v, want %v", err, boom)
	}

	closingBody := &testReadCloser{reader: strings.NewReader(""), closeErr: boom}
	client = &http.Client{Transport: testRoundTripper{response: &http.Response{
		StatusCode: http.StatusNoContent,
		Header:     http.Header{applyDurationHeader: []string{"1"}},
		Body:       closingBody,
	}}}
	if _, err := postDelta(client, "http://cluster.test/rga", "secret", []byte("delta")); !errors.Is(err, boom) {
		t.Fatalf("post close error = %v, want %v", err, boom)
	}

	client = &http.Client{Transport: testRoundTripper{response: &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Body:       &testReadCloser{reader: strings.NewReader(""), closeErr: boom},
	}}}
	if _, err := fetchState(client, "http://cluster.test", "secret"); !errors.Is(err, boom) {
		t.Fatalf("state failed-response close error = %v, want %v", err, boom)
	}

	client = &http.Client{Transport: testRoundTripper{response: &http.Response{
		StatusCode: http.StatusOK,
		Body:       &testReadCloser{reader: strings.NewReader(`{"runes":1}`), closeErr: boom},
	}}}
	if _, err := fetchState(client, "http://cluster.test", "secret"); !errors.Is(err, boom) {
		t.Fatalf("state successful-response close error = %v, want %v", err, boom)
	}

	if _, err := readRequest(&http.Request{Body: &testReadCloser{reader: strings.NewReader(""), readErr: boom}}, 1); !errors.Is(err, boom) {
		t.Fatalf("request read error = %v, want %v", err, boom)
	}
	if _, err := readRequest(&http.Request{Body: &testReadCloser{reader: strings.NewReader("a"), closeErr: boom}}, 1); !errors.Is(err, boom) {
		t.Fatalf("request close error = %v, want %v", err, boom)
	}
}

func TestRequestTargetAndDeliveryHelpers(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("body"))
	data, err := readRequest(request, 4)
	if err != nil || string(data) != "body" {
		t.Fatalf("read request = %q, %v", data, err)
	}
	for _, body := range []string{"", "abc"} {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if _, err := readRequest(request, 2); err == nil {
			t.Fatalf("readRequest(%q) succeeded, want error", body)
		}
	}
	for _, targets := range []struct {
		value string
		count int
	}{
		{value: "http://one.test, http://one.test/ ,https://two.test", count: 2},
		{value: "", count: 0},
		{value: "ftp://one.test", count: 0},
	} {
		got, err := parseTargets(targets.value)
		if targets.count == 0 {
			if err == nil {
				t.Fatalf("parseTargets(%q) succeeded, want error", targets.value)
			}
			continue
		}
		if err != nil || len(got) != targets.count {
			t.Fatalf("parseTargets(%q) = %v, %v", targets.value, got, err)
		}
	}
	for _, users := range []int{0, maxLogicalUsers + 1} {
		if _, err := buildDeliveries("sender", users, 1, 1); err == nil {
			t.Fatalf("buildDeliveries users=%d succeeded, want error", users)
		}
	}
	if _, err := buildDeliveries("sender", 1, maxInsertRunes+1, 1); err == nil {
		t.Fatal("buildDeliveries oversized insert succeeded, want error")
	}
	if _, err := buildDeliveries("sender", 1, 1, 0); err == nil {
		t.Fatal("buildDeliveries zero duplicates succeeded, want error")
	}
	if deliveries, err := buildDeliveries("sender", 1, 2, 2); err != nil || len(deliveries) != 4 {
		t.Fatalf("buildDeliveries success = %d, %v", len(deliveries), err)
	}
	if got := randomJitter(nil, 0, 0); got != 0 {
		t.Fatalf("zero jitter = %v", got)
	}
	if got := randomJitter(rand.New(rand.NewSource(1)), time.Millisecond, time.Millisecond); got != time.Millisecond {
		t.Fatalf("fixed jitter = %v", got)
	}
	if got := randomJitter(rand.New(rand.NewSource(1)), time.Millisecond, 2*time.Millisecond); got < time.Millisecond || got > 2*time.Millisecond {
		t.Fatalf("random jitter = %v", got)
	}
	if summary := summarizeLatencies(nil); summary.Count != 0 {
		t.Fatalf("empty summary = %+v", summary)
	}
	if summary := summarizeLatencies([]time.Duration{3 * time.Millisecond, time.Millisecond, 2 * time.Millisecond}); summary.P50Milliseconds != 2 || summary.MaxMilliseconds != 3 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestSendAndListenValidationFailures(t *testing.T) {
	if err := validateLoopbackListen("127.0.0.1:49801"); err != nil {
		t.Fatalf("loopback listener = %v", err)
	}
	if err := validateLoopbackListen("invalid"); err == nil {
		t.Fatal("invalid listen address succeeded")
	}
	if err := validateLoopbackListen("localhost:49801"); err == nil {
		t.Fatal("hostname listener succeeded")
	}
	if err := serve("0.0.0.0:49801", "receiver", "secret", time.Second); err == nil {
		t.Fatal("public serve succeeded")
	}
	if _, err := newClusterServer("", "secret"); err == nil {
		t.Fatal("empty replica server succeeded")
	}
	failing := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	if _, err := send(failing.URL, "sender", "secret", 1, 1, 1, 0, 0, time.Second, 1); err == nil {
		t.Fatal("send to unhealthy target succeeded")
	}
	if _, err := send("ftp://invalid.test", "sender", "secret", 1, 1, 1, 0, 0, time.Second, 1); err == nil {
		t.Fatal("send to invalid target succeeded")
	}
	verificationFailure := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/rga" {
			writer.Header().Set(applyDurationHeader, "1")
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer verificationFailure.Close()
	if _, err := send(verificationFailure.URL, "sender", "secret", 1, 1, 1, 0, 0, time.Second, 1); err == nil {
		t.Fatal("send with failed verification succeeded")
	}
}
