package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/im10furry/crdt/counter"
	"github.com/im10furry/crdt/text"
)

func TestProbeAuthenticatesAndDeduplicatesDelivery(t *testing.T) {
	receiver, err := newProbe("receiver", "secret", rgaProtocolV1)
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

	setSource, err := newProbe("set-sender", "secret", rgaProtocolV1)
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

func TestProbeRejectsSameLengthAndDifferentLengthTokens(t *testing.T) {
	value, err := newProbe("receiver", "secret", rgaProtocolV1)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"secreu", "short", "a much longer incorrect token"} {
		request := httptest.NewRequest(http.MethodGet, "/state", nil)
		request.Header.Set("X-CRDT-Probe-Token", token)
		if value.authorized(request) {
			t.Fatalf("authorized incorrect token %q", token)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/state", nil)
	request.Header.Set("X-CRDT-Probe-Token", "secret")
	if !value.authorized(request) {
		t.Fatal("rejected valid token")
	}
}

func TestProbeRejectsMalformedBody(t *testing.T) {
	receiver, err := newProbe("receiver", "secret", rgaProtocolV1)
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
	if _, err := newProbe("", "secret", rgaProtocolV1); err == nil {
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
	left, err := newProbe("left", "secret", rgaProtocolV1)
	if err != nil {
		t.Fatal(err)
	}
	right, err := newProbe("right", "secret", rgaProtocolV1)
	if err != nil {
		t.Fatal(err)
	}
	leftServer := httptest.NewServer(left)
	defer leftServer.Close()
	rightServer := httptest.NewServer(right)
	defer rightServer.Close()

	if err := send(leftServer.URL+","+rightServer.URL, "sender", "secret", rgaProtocolV1, 9, "shared", 0, "", 4, time.Second); err != nil {
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

func TestSendBroadcastsExplicitRGADeltaAndRejectsProtocolMismatch(t *testing.T) {
	for _, protocol := range []rgaProtocol{rgaProtocolV1, rgaProtocolRunV2} {
		t.Run(string(protocol), func(t *testing.T) {
			left, err := newProbe("left", "secret", protocol)
			if err != nil {
				t.Fatal(err)
			}
			right, err := newProbe("right", "secret", protocol)
			if err != nil {
				t.Fatal(err)
			}
			leftServer := httptest.NewServer(left)
			defer leftServer.Close()
			rightServer := httptest.NewServer(right)
			defer rightServer.Close()

			const runes = 64
			if err := send(leftServer.URL+","+rightServer.URL, "rga-sender", "secret", protocol, 0, "", runes, "λ", 3, time.Second); err != nil {
				t.Fatal(err)
			}
			want := strings.Repeat("λ", runes)
			for _, receiver := range []*probe{left, right} {
				if got := receiver.rga.String(); got != want {
					t.Fatalf("RGA text = %q, want %q", got, want)
				}
				state := receiver.state()
				if state.Text.Protocol != string(protocol) || state.Text.Runes != runes || state.Text.Pending != 0 || state.Text.SHA256 == "" {
					t.Fatalf("RGA state = %+v", state.Text)
				}
			}
		})
	}

	legacy, err := newProbe("legacy", "secret", rgaProtocolV1)
	if err != nil {
		t.Fatal(err)
	}
	runFrame, err := newRGADelta("sender", rgaProtocolRunV2, 1, "x")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/rga", bytes.NewReader(runFrame))
	request.Header.Set("X-CRDT-Probe-Token", "secret")
	response := httptest.NewRecorder()
	legacy.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || legacy.rga.String() != "" {
		t.Fatalf("v1 receiver accepted run-v2 frame: status=%d text=%q", response.Code, legacy.rga.String())
	}
	if _, err := newRGADelta("sender", rgaProtocolV1, maxRGARunesPerDelivery+1, "x"); err == nil {
		t.Fatal("newRGADelta accepted a rune count above its limit")
	}
	if _, err := newRGADelta("sender", rgaProtocolV1, 1, "xy"); err == nil {
		t.Fatal("newRGADelta accepted multiple runes")
	}
	disabled, err := newProbe("disabled", "secret", rgaProtocolDisabled)
	if err != nil {
		t.Fatal(err)
	}
	v1Frame, err := newRGADelta("sender", rgaProtocolV1, 1, "x")
	if err != nil {
		t.Fatal(err)
	}
	disabledRequest := httptest.NewRequest(http.MethodPost, "/rga", bytes.NewReader(v1Frame))
	disabledRequest.Header.Set("X-CRDT-Probe-Token", "secret")
	disabledResponse := httptest.NewRecorder()
	disabled.ServeHTTP(disabledResponse, disabledRequest)
	if disabledResponse.Code != http.StatusNotFound || disabled.rga.String() != "" {
		t.Fatalf("disabled RGA endpoint status=%d text=%q", disabledResponse.Code, disabled.rga.String())
	}
	if defaultRGAProtocol != rgaProtocolRunV2 {
		t.Fatalf("default RGA protocol = %q, want %q", defaultRGAProtocol, rgaProtocolRunV2)
	}
	if protocol, err := parseRGAProtocol(string(defaultRGAProtocol)); err != nil || protocol != rgaProtocolRunV2 {
		t.Fatalf("default RGA protocol parse = %q, %v", protocol, err)
	}
}

func TestProbeDefaultRunV2RejectsMismatchedScalarRGAFrames(t *testing.T) {
	receiver, err := newProbe("receiver", "secret", defaultRGAProtocol)
	if err != nil {
		t.Fatal(err)
	}
	source, err := text.New("source")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := source.Insert(0, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalBinaryWithLimits(rgaTransportLimits())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/rga", bytes.NewReader(encoded))
	request.Header.Set("X-CRDT-Probe-Token", "secret")
	response := httptest.NewRecorder()
	receiver.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || receiver.rga.String() != "" {
		t.Fatalf("stable run-v2 receiver accepted scalar v1 frame: status=%d text=%q", response.Code, receiver.rga.String())
	}
}

func TestSendBroadcastsDefaultRunV2AtBoundAndDeduplicates(t *testing.T) {
	left, err := newProbe("left", "secret", defaultRGAProtocol)
	if err != nil {
		t.Fatal(err)
	}
	right, err := newProbe("right", "secret", defaultRGAProtocol)
	if err != nil {
		t.Fatal(err)
	}
	leftServer := httptest.NewServer(left)
	defer leftServer.Close()
	rightServer := httptest.NewServer(right)
	defer rightServer.Close()

	if err := send(leftServer.URL+","+rightServer.URL, "rga-sender", "secret", defaultRGAProtocol, 0, "", maxRGARunesPerDelivery, "λ", 3, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("λ", maxRGARunesPerDelivery)
	for _, receiver := range []*probe{left, right} {
		if got := receiver.rga.String(); got != want {
			t.Fatalf("RGA text length = %d, want %d", len([]rune(got)), maxRGARunesPerDelivery)
		}
		state := receiver.state()
		if state.Text.Protocol != string(defaultRGAProtocol) || state.Text.Runes != maxRGARunesPerDelivery || state.Text.Pending != 0 {
			t.Fatalf("RGA state = %+v", state.Text)
		}
	}
	if _, err := newRGADelta("rga-sender", defaultRGAProtocol, maxRGARunesPerDelivery+1, "x"); err == nil {
		t.Fatal("newRGADelta accepted a rune count above the stable run-v2 limit")
	}
}

func TestServeRejectsInvalidListenAddress(t *testing.T) {
	if err := serve("[", "receiver", "secret", rgaProtocolV1, time.Second, false); err == nil {
		t.Fatal("serve() accepted invalid address")
	}
	if err := validateListenAddress("0.0.0.0:49511", false); err == nil {
		t.Fatal("loopback-only probe accepted a public listen address")
	}
	for _, address := range []string{"127.0.0.1:49511", "[::1]:49511", "localhost:49511"} {
		if err := validateListenAddress(address, false); err != nil {
			t.Fatalf("loopback address %q rejected: %v", address, err)
		}
	}
	if err := validateListenAddress("0.0.0.0:49511", true); err != nil {
		t.Fatalf("explicit non-loopback opt-in rejected: %v", err)
	}
	if _, err := parseRGAProtocol("unknown"); err == nil {
		t.Fatal("parseRGAProtocol accepted an unknown value")
	}
}

func TestProbeProtocolAndBoundServerFailurePaths(t *testing.T) {
	unknown := rgaProtocol("unknown")
	if _, err := newProbe("receiver", "secret", unknown); err == nil {
		t.Fatal("newProbe accepted an unknown RGA protocol")
	}
	if _, err := marshalRGADelta(text.Delta{}, unknown, rgaTransportLimits()); err == nil {
		t.Fatal("marshalRGADelta accepted an unknown protocol")
	}
	if _, err := unmarshalRGADelta(nil, unknown, rgaTransportLimits()); err == nil {
		t.Fatal("unmarshalRGADelta accepted an unknown protocol")
	}
	if err := send("http://example.invalid", "sender", "secret", unknown, 1, "", 0, "", 1, time.Second); err == nil {
		t.Fatal("send accepted an unknown RGA protocol")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	if err := serve(listener.Addr().String(), "receiver", "secret", rgaProtocolV1, time.Second, false); err == nil {
		t.Fatal("serve unexpectedly acquired an occupied loopback port")
	}
	if err := run([]string{
		"-mode", "send", "-target", "http://127.0.0.1:1", "-token", "secret", "-replica", "sender", "-rga-protocol", "unknown",
	}); err == nil {
		t.Fatal("run accepted an unknown RGA protocol")
	}
	if err := run([]string{
		"-mode", "send", "-target", "http://127.0.0.1:1", "-token", "secret", "-replica", "sender", "-rga-runes", "-1",
	}); err == nil {
		t.Fatal("run accepted a negative RGA rune count")
	}
}

func TestProbeHTTPErrorPathsAndRequestBounds(t *testing.T) {
	receiver, err := newProbe("receiver", "secret", rgaProtocolV1)
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

	if _, err := readRequest(httptest.NewRequest(http.MethodPost, "/counter", nil), maxSmallRequestBytes); err == nil {
		t.Fatal("readRequest() accepted empty body")
	}
	tooLarge := httptest.NewRequest(http.MethodPost, "/counter", bytes.NewReader(make([]byte, maxSmallRequestBytes+1)))
	if _, err := readRequest(tooLarge, maxSmallRequestBytes); err == nil {
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
	receiver, err := newProbe("receiver", "secret", rgaProtocolV1)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(receiver)
	defer server.Close()
	if err := send(server.URL, "counter-sender", "secret", rgaProtocolV1, 4, "", 0, "", 1, time.Second); err != nil {
		t.Fatalf("counter-only send error = %v", err)
	}
	if err := send(server.URL, "set-sender", "secret", rgaProtocolV1, 0, "set-only", 0, "", 1, time.Second); err != nil {
		t.Fatalf("set-only send error = %v", err)
	}
	if got, err := receiver.counter.Value(); err != nil || got != 4 {
		t.Fatalf("counter value = %d, %v", got, err)
	}
	if !receiver.set.Contains("set-only") {
		t.Fatal("set-only mutation was not delivered")
	}
	if err := send("not-a-url", "sender", "secret", rgaProtocolV1, 1, "item", 0, "", 1, time.Second); err == nil {
		t.Fatal("send() accepted invalid target")
	}
	if err := send(server.URL+",http://127.0.0.1:1", "sender", "secret", rgaProtocolV1, 1, "partial", 0, "", 1, time.Second); err == nil {
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
	if _, err := readRequest(request, maxSmallRequestBytes); err == nil {
		t.Fatal("readRequest() ignored body read failure")
	}
}

func TestProbePropagatesBodyCloseFailures(t *testing.T) {
	closeFailure := errors.New("close failure")
	request := httptest.NewRequest(http.MethodPost, "/counter", nil)
	request.Body = controlledBody{reader: bytes.NewReader([]byte("frame")), closeErr: closeFailure}
	if _, err := readRequest(request, maxSmallRequestBytes); !errors.Is(err, closeFailure) {
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

func TestProbeConcurrentDuplicateAndUnauthorizedTraffic(t *testing.T) {
	receiver, err := newProbe("receiver", "secret", rgaProtocolV1)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(receiver)
	defer server.Close()

	source, err := newProbe("sender", "secret", rgaProtocolV1)
	if err != nil {
		t.Fatal(err)
	}
	counterDelta, err := source.counter.Increment(11)
	if err != nil {
		t.Fatal(err)
	}
	counterData, err := counterDelta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	setDelta, err := source.set.Add("concurrent-item")
	if err != nil {
		t.Fatal(err)
	}
	setData, err := setDelta.MarshalBinary(stringCodec{})
	if err != nil {
		t.Fatal(err)
	}

	const workers = 96
	errCh := make(chan error, workers)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			if worker%3 == 0 {
				request, err := http.NewRequest(http.MethodPost, server.URL+"/counter", bytes.NewReader(counterData))
				if err != nil {
					errCh <- err
					return
				}
				request.Header.Set("X-CRDT-Probe-Token", "incorrect-token")
				response, err := server.Client().Do(request)
				if err != nil {
					errCh <- err
					return
				}
				closeErr := response.Body.Close()
				if closeErr != nil {
					errCh <- closeErr
					return
				}
				if response.StatusCode != http.StatusUnauthorized {
					errCh <- fmt.Errorf("unauthorized request status = %d", response.StatusCode)
				}
				return
			}
			endpoint, data := "/counter", counterData
			if worker%2 == 0 {
				endpoint, data = "/orset", setData
			}
			if err := postRepeated(server.Client(), server.URL+endpoint, "secret", data, 3); err != nil {
				errCh <- err
			}
		}(worker)
	}
	group.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	if got, err := receiver.counter.Value(); err != nil || got != 11 {
		t.Fatalf("counter value = %d, %v; want 11, nil", got, err)
	}
	if !receiver.set.Contains("concurrent-item") {
		t.Fatal("concurrent OR-Set delta was not retained")
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
