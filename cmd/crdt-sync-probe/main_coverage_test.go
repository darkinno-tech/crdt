package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestProbeAcknowledgesRGADeliveryWithoutStateBody(t *testing.T) {
	receiver, err := newProbe("receiver", "secret", defaultRGAProtocol)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := newRGADelta("sender", defaultRGAProtocol, 3, "λ")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/rga", bytes.NewReader(encoded))
	request.Header.Set("X-CRDT-Probe-Token", "secret")
	response := httptest.NewRecorder()
	receiver.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("RGA acknowledgement status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if response.Body.Len() != 0 {
		t.Fatalf("RGA acknowledgement body = %q, want empty", response.Body.String())
	}
	if _, err := strconv.ParseInt(response.Header().Get(applyDurationHeader), 10, 64); err != nil {
		t.Fatalf("RGA acknowledgement duration = %q: %v", response.Header().Get(applyDurationHeader), err)
	}
	if state := receiver.state(); state.Text.Runes != 3 || state.Text.Pending != 0 {
		t.Fatalf("receiver state = %+v", state.Text)
	}
}

func TestProbeRejectsMalformedRGAAndTransportBodies(t *testing.T) {
	receiver, err := newProbe("receiver", "secret", defaultRGAProtocol)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/rga", bytes.NewBufferString("not-an-rga-frame"))
	request.Header.Set("X-CRDT-Probe-Token", "secret")
	response := httptest.NewRecorder()
	receiver.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("malformed RGA status = %d, want %d", response.Code, http.StatusBadRequest)
	}

	if _, err := readRequest(httptest.NewRequest(http.MethodPost, "/rga", bytes.NewBufferString("frame")), 0); err == nil {
		t.Fatal("readRequest accepted a non-positive limit")
	}

	malformedState := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("not-json"))
	}))
	t.Cleanup(malformedState.Close)
	if _, err := fetchState(malformedState.Client(), malformedState.URL, "secret"); err == nil {
		t.Fatal("fetchState accepted malformed JSON")
	}

	if err := postRepeated(http.DefaultClient, ":", "secret", []byte("frame"), 1); err == nil {
		t.Fatal("postRepeated accepted an invalid endpoint")
	}

	readFailure := errors.New("body read failure")
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       controlledBody{readErr: readFailure},
		}, nil
	})}
	if err := postRepeated(client, "http://example.test/rga", "secret", []byte("frame"), 1); !errors.Is(err, readFailure) {
		t.Fatalf("postRepeated error = %v, want %v", err, readFailure)
	}
}

func TestRunReportsSendDeliveryFailure(t *testing.T) {
	err := run([]string{
		"-mode", "send",
		"-target", "http://127.0.0.1:1",
		"-token", "secret",
		"-replica", "sender",
		"-counter-increment", "1",
		"-element", "",
		"-timeout", "100ms",
	})
	if err == nil {
		t.Fatal("run accepted an unreachable target")
	}
}
