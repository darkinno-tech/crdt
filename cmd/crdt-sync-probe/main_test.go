package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/darkinno/crdt/counter"
)

func TestProbeAuthenticatesAndDeduplicatesDelivery(t *testing.T) {
	receiver, err := newProbe("receiver", "secret")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(receiver)
	defer server.Close()

	unauthorized, err := http.Post(server.URL+"/counter", "application/octet-stream", bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, unauthorized.Body)
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.StatusCode, http.StatusUnauthorized)
	}

	source, err := counter.NewGCounter("sender")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := source.Increment(7)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := postRepeated(server.Client(), server.URL+"/counter", "secret", encoded, 3); err != nil {
		t.Fatal(err)
	}

	setSource, err := newProbe("set-sender", "secret")
	if err != nil {
		t.Fatal(err)
	}
	setDelta, err := setSource.set.Add("item")
	if err != nil {
		t.Fatal(err)
	}
	setEncoded, err := setDelta.MarshalBinary(stringCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := postRepeated(server.Client(), server.URL+"/orset", "secret", setEncoded, 3); err != nil {
		t.Fatal(err)
	}

	if got, err := receiver.counter.Value(); err != nil || got != 7 {
		t.Fatalf("counter value = %d, %v; want 7, nil", got, err)
	}
	if !receiver.set.Contains("item") {
		t.Fatal("receiver did not retain delivered OR-Set element")
	}
}

func TestProbeRejectsMalformedBody(t *testing.T) {
	receiver, err := newProbe("receiver", "secret")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/counter", bytes.NewBufferString("not-a-frame"))
	request.Header.Set("X-CRDT-Probe-Token", "secret")
	response := httptest.NewRecorder()
	receiver.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestLoadTokenReadsBoundedFile(t *testing.T) {
	if _, err := loadToken("", ""); err == nil {
		t.Fatal("loadToken() accepted no token source")
	}
	path := t.TempDir() + "/token"
	if err := os.WriteFile(path, []byte(" secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := loadToken("", path); err != nil || got != "secret" {
		t.Fatalf("loadToken() = %q, %v", got, err)
	}
	if _, err := loadToken("inline", path); err == nil {
		t.Fatal("loadToken() accepted both token sources")
	}
	if _, err := loadToken("", path+"-missing"); err == nil {
		t.Fatal("loadToken() accepted missing file")
	}
	oversizedPath := t.TempDir() + "/oversized-token"
	if err := os.WriteFile(oversizedPath, bytes.Repeat([]byte("a"), 1025), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken("", oversizedPath); err == nil {
		t.Fatal("loadToken() accepted oversized file")
	}
	if _, err := newProbe("", "secret"); err == nil {
		t.Fatal("newProbe() accepted empty replica")
	}
	emptyPath := t.TempDir() + "/empty-token"
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToken("", emptyPath); err == nil {
		t.Fatal("loadToken() accepted empty file")
	}
}

func TestParseTargetsNormalizesAndDeduplicates(t *testing.T) {
	targets, err := parseTargets(" http://one.example/ ,http://two.example,http://one.example ")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(targets), 2; got != want {
		t.Fatalf("target count = %d, want %d", got, want)
	}
	if _, err := parseTargets("ftp://invalid.example"); err == nil {
		t.Fatal("parseTargets() accepted unsupported scheme")
	}
}

func TestSendBroadcastsOneDeltaToEveryTarget(t *testing.T) {
	left, err := newProbe("left", "secret")
	if err != nil {
		t.Fatal(err)
	}
	right, err := newProbe("right", "secret")
	if err != nil {
		t.Fatal(err)
	}
	leftServer := httptest.NewServer(left)
	defer leftServer.Close()
	rightServer := httptest.NewServer(right)
	defer rightServer.Close()

	if err := send(leftServer.URL+","+rightServer.URL, "sender", "secret", 9, "shared", 4, time.Second); err != nil {
		t.Fatal(err)
	}
	for _, receiver := range []*probe{left, right} {
		if got, err := receiver.counter.Value(); err != nil || got != 9 {
			t.Fatalf("counter value = %d, %v; want 9, nil", got, err)
		}
		if !receiver.set.Contains("shared") {
			t.Fatal("receiver did not retain broadcast element")
		}
	}
	if err := run([]string{"-mode", "send", "-target", leftServer.URL, "-token", "secret", "-replica", "run-sender", "-counter-increment", "0", "-element", ""}); err == nil {
		t.Fatal("run() accepted an empty mutation")
	}
	if err := run([]string{"-mode", "send", "-target", leftServer.URL, "-token", "secret", "-replica", "run-sender", "-counter-increment", "0", "-element", "from-run", "-duplicates", "1", "-timeout", "1s"}); err != nil {
		t.Fatalf("run() send error = %v", err)
	}
	if !left.set.Contains("from-run") {
		t.Fatal("run() did not deliver element")
	}
	if err := run([]string{"-mode", "unknown", "-token", "secret", "-replica", "sender"}); err == nil {
		t.Fatal("run() accepted an unknown mode")
	}
}

func TestServeRejectsInvalidListenAddress(t *testing.T) {
	if err := serve("[", "receiver", "secret", time.Second); err == nil {
		t.Fatal("serve() accepted invalid address")
	}
}

func TestProbeHTTPErrorPathsAndRequestBounds(t *testing.T) {
	receiver, err := newProbe("receiver", "secret")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(receiver)
	defer server.Close()

	request, err := http.NewRequest(http.MethodGet, server.URL+"/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-CRDT-Probe-Token", "secret")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	closeBody(t, response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("state status = %d", response.StatusCode)
	}

	for _, path := range []string{"/missing", "/orset"} {
		request, err := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewBufferString("invalid"))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("X-CRDT-Probe-Token", "secret")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		closeBody(t, response.Body)
		want := http.StatusBadRequest
		if path == "/missing" {
			want = http.StatusNotFound
		}
		if response.StatusCode != want {
			t.Fatalf("%s status = %d, want %d", path, response.StatusCode, want)
		}
	}

	if _, err := readRequest(httptest.NewRequest(http.MethodPost, "/counter", nil)); err == nil {
		t.Fatal("readRequest() accepted empty body")
	}
	tooLarge := httptest.NewRequest(http.MethodPost, "/counter", bytes.NewReader(make([]byte, maxBodyBytes+1)))
	if _, err := readRequest(tooLarge); err == nil {
		t.Fatal("readRequest() accepted oversized body")
	}
	if _, err := fetchState(server.Client(), server.URL, "wrong"); err == nil {
		t.Fatal("fetchState() accepted unauthorized response")
	}

	failing := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeError(writer, http.StatusServiceUnavailable, errors.New("unavailable"))
	}))
	defer failing.Close()
	if err := postRepeated(failing.Client(), failing.URL, "secret", []byte("body"), 1); err == nil {
		t.Fatal("postRepeated() accepted error response")
	}
	if _, err := fetchState(failing.Client(), failing.URL, "secret"); err == nil {
		t.Fatal("fetchState() accepted error response")
	}
}

func TestSendSupportsSingleMutationTypesAndInvalidInputs(t *testing.T) {
	receiver, err := newProbe("receiver", "secret")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(receiver)
	defer server.Close()
	if err := send(server.URL, "counter-sender", "secret", 4, "", 1, time.Second); err != nil {
		t.Fatalf("counter-only send error = %v", err)
	}
	if err := send(server.URL, "set-sender", "secret", 0, "set-only", 1, time.Second); err != nil {
		t.Fatalf("set-only send error = %v", err)
	}
	if got, err := receiver.counter.Value(); err != nil || got != 4 {
		t.Fatalf("counter value = %d, %v", got, err)
	}
	if !receiver.set.Contains("set-only") {
		t.Fatal("set-only mutation was not delivered")
	}
	if err := send("not-a-url", "sender", "secret", 1, "item", 1, time.Second); err == nil {
		t.Fatal("send() accepted invalid target")
	}
	if err := send(server.URL+",http://127.0.0.1:1", "sender", "secret", 1, "partial", 1, time.Second); err == nil {
		t.Fatal("send() accepted unreachable second target")
	}
	if err := run([]string{"-mode", "send", "-target", server.URL, "-replica", "sender"}); err == nil {
		t.Fatal("run() accepted missing token")
	}
	if err := run([]string{"-mode", "serve", "-listen", "[", "-token", "secret", "-replica", "sender"}); err == nil {
		t.Fatal("run() accepted invalid serve address")
	}
	if err := run([]string{"-mode", "send", "-token", "secret", "-replica", "sender", "-timeout", "not-a-duration"}); err == nil {
		t.Fatal("run() accepted invalid duration")
	}
}

func TestProbePropagatesTransportAndBodyFailures(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network failure")
	})}
	if err := postRepeated(client, "http://example.invalid/counter", "secret", []byte("frame"), 1); err == nil {
		t.Fatal("postRepeated() ignored transport failure")
	}
	if _, err := fetchState(client, "http://example.invalid", "secret"); err == nil {
		t.Fatal("fetchState() ignored transport failure")
	}
	if _, err := fetchState(http.DefaultClient, "://invalid", "secret"); err == nil {
		t.Fatal("fetchState() accepted invalid base URL")
	}
	request := httptest.NewRequest(http.MethodPost, "/counter", nil)
	request.Body = controlledBody{readErr: errors.New("body failure")}
	if _, err := readRequest(request); err == nil {
		t.Fatal("readRequest() ignored body read failure")
	}
}

func TestProbePropagatesBodyCloseFailures(t *testing.T) {
	closeFailure := errors.New("close failure")
	request := httptest.NewRequest(http.MethodPost, "/counter", nil)
	request.Body = controlledBody{reader: bytes.NewReader([]byte("frame")), closeErr: closeFailure}
	if _, err := readRequest(request); !errors.Is(err, closeFailure) {
		t.Fatalf("readRequest() error = %v, want close failure", err)
	}

	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return closeFailureResponse(http.StatusOK, "200 OK", `{"counts":{},"elements":[]}`, closeFailure), nil
	})}
	if _, err := fetchState(client, "http://example.invalid", "secret"); !errors.Is(err, closeFailure) {
		t.Fatalf("fetchState() error = %v, want close failure", err)
	}
	if err := postRepeated(client, "http://example.invalid/counter", "secret", []byte("frame"), 1); !errors.Is(err, closeFailure) {
		t.Fatalf("postRepeated() error = %v, want close failure", err)
	}

	statusClient := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return closeFailureResponse(http.StatusServiceUnavailable, "503 Service Unavailable", "", closeFailure), nil
	})}
	if _, err := fetchState(statusClient, "http://example.invalid", "secret"); !errors.Is(err, closeFailure) {
		t.Fatalf("fetchState() status close error = %v, want close failure", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func closeBody(t *testing.T, body io.Closer) {
	t.Helper()
	if err := body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
}

type controlledBody struct {
	reader   io.Reader
	readErr  error
	closeErr error
}

func (body controlledBody) Read(data []byte) (int, error) {
	if body.readErr != nil {
		return 0, body.readErr
	}
	return body.reader.Read(data)
}

func (body controlledBody) Close() error { return body.closeErr }

func closeFailureResponse(statusCode int, status, body string, closeErr error) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     status,
		Header:     make(http.Header),
		Body:       controlledBody{reader: bytes.NewBufferString(body), closeErr: closeErr},
	}
}
