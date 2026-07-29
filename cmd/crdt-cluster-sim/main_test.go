package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DarkInno/crdt/text"
)

func TestClusterServerRequiresAuthentication(t *testing.T) {
	server, err := newClusterServer("receiver", "secret")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/rga", bytes.NewBufferString("invalid"))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestClusterServerAcknowledgesWithoutStateAndMeasuresApply(t *testing.T) {
	server, err := newClusterServer("receiver", "secret")
	if err != nil {
		t.Fatal(err)
	}
	source, err := text.New("source")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := source.Insert(0, "a")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalBinaryWithLimits(frameLimits())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/rga", bytes.NewReader(encoded))
	request.Header.Set("X-CRDT-Cluster-Token", "secret")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if response.Body.Len() != 0 {
		t.Fatalf("ack response length = %d, want 0", response.Body.Len())
	}
	if _, err := parseApplyLatency(response.Header().Get(applyDurationHeader)); err != nil {
		t.Fatalf("ack apply duration: %v", err)
	}

	stateRequest := httptest.NewRequest(http.MethodGet, "/state", nil)
	stateRequest.Header.Set("X-CRDT-Cluster-Token", "secret")
	stateResponse := httptest.NewRecorder()
	server.ServeHTTP(stateResponse, stateRequest)
	if stateResponse.Code != http.StatusOK {
		t.Fatalf("state status = %d, want %d", stateResponse.Code, http.StatusOK)
	}
	var state clusterState
	if err := json.NewDecoder(stateResponse.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if state.Runes != 1 || state.Pending != 0 {
		t.Fatalf("state = %+v", state)
	}
}

func TestClusterSendConvergesReorderedDuplicateCuts(t *testing.T) {
	servers := make([]*httptest.Server, 0, 3)
	for index := 0; index < 3; index++ {
		receiver, err := newClusterServer("receiver-"+string(rune('a'+index)), "secret")
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(receiver)
		servers = append(servers, server)
		defer server.Close()
	}
	targets := servers[0].URL + "," + servers[1].URL + "," + servers[2].URL
	if _, err := send(targets, "device-a", "secret", 6, 8, 2, 0, 0, time.Second, 1); err != nil {
		t.Fatal(err)
	}
	report, err := send(targets, "device-b", "secret", 6, 8, 2, 0, 0, time.Second, 2)
	if err != nil {
		t.Fatal(err)
	}
	var canonical string
	for target, result := range report.TargetReports {
		if result.Deliveries != 24 || result.State.Pending != 0 || result.State.Runes != 84 {
			t.Fatalf("%s report = %+v", target, result)
		}
		if result.Latency.Count != result.Deliveries || result.ServerApplyLatency.Count != result.Deliveries || result.VerificationLatencyMilliseconds <= 0 {
			t.Fatalf("%s latency report = %+v", target, result)
		}
		if canonical == "" {
			canonical = result.State.CanonicalSHA256
		} else if result.State.CanonicalSHA256 != canonical {
			t.Fatalf("canonical state mismatch: %q != %q", result.State.CanonicalSHA256, canonical)
		}
	}
}

func TestClusterRejectsPublicListenAddressAndInvalidLoad(t *testing.T) {
	if err := validateLoopbackListen("0.0.0.0:49801"); err == nil {
		t.Fatal("validateLoopbackListen() accepted a public address")
	}
	if _, err := buildDeliveries("sender", maxLogicalUsers+1, 1, 1); err == nil {
		t.Fatal("buildDeliveries() accepted too many users")
	}
	if _, err := buildDeliveries("sender", 1, maxInsertRunes+1, 1); err == nil {
		t.Fatal("buildDeliveries() accepted too many runes")
	}
}
