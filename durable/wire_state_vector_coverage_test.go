package durable

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/replica"
)

func TestStateVectorWirePublicEnvelopeAndMalformedControlBoundaries(t *testing.T) {
	manifest := durableTestManifest(t)
	change := durableTestChange(t, manifest, "alice", 3, 1)
	encoded, err := EncodeChange(change)
	if err != nil {
		t.Fatal(err)
	}
	dot, delta, err := DecodeChange(encoded, 1<<20, 128)
	if err != nil || dot != change.Dot || string(delta) != string(change.Delta()) {
		t.Fatalf("DecodeChange() = %#v, %x, %v", dot, delta, err)
	}
	if _, _, err := DecodeChange(nil, 1<<20, 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("DecodeChange(nil) = %v", err)
	}

	vector, err := replica.NewFrontier(map[string]uint64{"alice": 3, "bob": 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stateVectorEntries(vector, 1, 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("stateVectorEntries over limit = %v", err)
	}
	if _, err := stateVectorEntries(vector, 4, 3); !errors.Is(err, errInvalidWire) {
		t.Fatalf("stateVectorEntries actor limit = %v", err)
	}
	for _, entries := range [][]stateVectorEntry{
		{{Actor: "alice", Counter: 1}, {Actor: "alice", Counter: 2}},
		{{Actor: "bob", Counter: 1}, {Actor: "alice", Counter: 2}},
		{{Actor: "", Counter: 1}},
		{{Actor: "alice", Counter: 0}},
	} {
		if _, err := frontierFromStateVectorEntries(entries, 4, 128); !errors.Is(err, errInvalidWire) {
			t.Fatalf("frontierFromStateVectorEntries(%#v) = %v", entries, err)
		}
	}
	if _, err := frontierFromStateVectorEntries([]stateVectorEntry{{Actor: "alice", Counter: 1}}, 0, 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("frontierFromStateVectorEntries invalid limits = %v", err)
	}
	if _, err := frontierFromStateVectorEntries([]stateVectorEntry{{Actor: "alice", Counter: 1}, {Actor: "bob", Counter: 1}}, 1, 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("frontierFromStateVectorEntries over limit = %v", err)
	}

	stateHello, err := marshalStateVectorHello(manifest, vector, 4, 128)
	if err != nil {
		t.Fatal(err)
	}
	stateWelcome, err := marshalStateVectorWelcome(manifest, 7)
	if err != nil {
		t.Fatal(err)
	}
	complete, err := marshalCatchUpComplete(7)
	if err != nil {
		t.Fatal(err)
	}
	overLimit := make([]byte, maxControlBytes+1)
	for _, data := range [][]byte{nil, overLimit, []byte(`{"version":3}`), []byte(`{"version":2,"unknown":true}`), append(stateHello, 'x')} {
		if _, _, err := unmarshalStateVectorHello(data, 4, 128); !errors.Is(err, errInvalidWire) {
			t.Fatalf("unmarshalStateVectorHello(%q) = %v", data, err)
		}
	}
	invalidCounterHello, err := json.Marshal(stateVectorHelloMessage{Version: stateVectorProtocolVersion, Manifest: manifest, StateVector: []stateVectorEntry{{Actor: "alice", Counter: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := unmarshalStateVectorHello(invalidCounterHello, 4, 128); !errors.Is(err, errInvalidWire) {
		t.Fatalf("unmarshalStateVectorHello zero counter = %v", err)
	}
	for _, data := range [][]byte{nil, overLimit, []byte(`{"version":1}`), append(stateWelcome, 'x')} {
		if _, _, err := unmarshalStateVectorWelcome(data); !errors.Is(err, errInvalidWire) {
			t.Fatalf("unmarshalStateVectorWelcome(%q) = %v", data, err)
		}
	}
	for _, data := range [][]byte{nil, overLimit, []byte(`{"version":1}`), append(complete, 'x')} {
		if _, err := unmarshalCatchUpComplete(data); !errors.Is(err, errInvalidWire) {
			t.Fatalf("unmarshalCatchUpComplete(%q) = %v", data, err)
		}
	}
}

func TestStateVectorStoreAndClientFailureBoundaries(t *testing.T) {
	manifest := durableTestManifest(t)
	noOp := func(Event) error { return nil }
	if _, err := NewReconnectClient("ws://example.test/ws", replica.Manifest{}, ClientConfig{OnEvent: noOp}); err == nil {
		t.Fatal("client accepted invalid manifest")
	}
	if _, err := NewReconnectClient("ws://example.test/ws", manifest, ClientConfig{OnEvent: noOp, MaxMessageBytes: 1}); err == nil {
		t.Fatal("client accepted invalid limits")
	}
	client, err := NewReconnectClient("ws://example.test/ws", manifest, ClientConfig{OnEvent: noOp})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Run(cancelled); err != nil {
		t.Fatalf("cancelled client Run = %v", err)
	}
	if _, err := client.marshalHello("unexpected", replica.Frontier{}); !errors.Is(err, errInvalidWire) {
		t.Fatalf("marshalHello unexpected subprotocol = %v", err)
	}
	if _, _, err := unmarshalWelcomeForSubprotocol("unexpected", nil); !errors.Is(err, errInvalidWire) {
		t.Fatalf("unmarshalWelcome unexpected subprotocol = %v", err)
	}
	invalidVector, err := replica.NewFrontier(map[string]uint64{strings.Repeat("a", 129): 1})
	if err != nil {
		t.Fatal(err)
	}
	invalidVectorClient, err := NewReconnectClient("ws://example.test/ws", manifest, ClientConfig{
		OnEvent:     noOp,
		OnCatchUp:   func(uint64) error { return nil },
		StateVector: func() replica.Frontier { return invalidVector },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := invalidVectorClient.Run(context.Background()); !errors.Is(err, errInvalidWire) {
		t.Fatalf("invalid state vector Run = %v", err)
	}

	var nilStore *Store
	if _, _, err := nilStore.CatchUp(manifest.GroupID, replica.Frontier{}, 1, 1, manifest, crdt.ProtocolPolicy{}, 1, 128); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil CatchUp = %v", err)
	}
	if _, _, err := nilStore.Replay(manifest.GroupID, 0, 1, 1, manifest, crdt.ProtocolPolicy{}, 1, 128); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Replay = %v", err)
	}

	store := durableTestStore(t, t.TempDir()+"/relay.db", 4, 1<<20)
	configuredHandler, _ := durableTestHandler(t, store, manifest)
	if _, err := NewHandler(Config{
		Store:                 store,
		Groups:                []*Group{nil},
		Authenticate:          configuredHandler.authenticate,
		Authorize:             configuredHandler.authorize,
		AuthorizeSubscription: configuredHandler.authorizeSubscription,
	}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil handler group = %v", err)
	}
	vector, err := replica.NewFrontier(map[string]uint64{"alice": 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CatchUp(manifest.GroupID, vector, 4, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrReplayUnavailable) {
		t.Fatalf("missing group CatchUp = %v", err)
	}
	if _, _, err := store.Replay(manifest.GroupID, 1, 4, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128); !errors.Is(err, ErrReplayUnavailable) {
		t.Fatalf("missing group Replay = %v", err)
	}
	if err := store.ensureActorIndex(""); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty actor index group = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CatchUp(manifest.GroupID, replica.Frontier{}, 1, 1, manifest, crdt.ProtocolPolicy{}, 1, 128); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed CatchUp = %v", err)
	}
	if _, _, err := store.Replay(manifest.GroupID, 0, 1, 1, manifest, crdt.ProtocolPolicy{}, 1, 128); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Replay = %v", err)
	}
	if err := store.ensureActorIndex(manifest.GroupID); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed actor index = %v", err)
	}
}

func TestDurableInternalFailClosedBoundaries(t *testing.T) {
	if _, err := NewGroup(GroupConfig{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty group = %v", err)
	}
	invalidManifest := durableTestManifest(t)
	invalidManifest.Epoch = 0
	if _, err := NewGroup(GroupConfig{Manifest: invalidManifest, Validate: func([]byte) error { return nil }}); err == nil {
		t.Fatal("group accepted an invalid manifest")
	}
	if err := validateOriginPatterns([]string{"["}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid origin pattern = %v", err)
	}
	for _, key := range [][]byte{nil, {1}, append([]byte{0}, make([]byte, 8)...)} {
		if _, err := dotFromKey(key); !errors.Is(err, ErrCorruptStore) {
			t.Fatalf("dotFromKey(%x) = %v", key, err)
		}
	}

	var handler *Handler
	handler.heartbeat(nil, Peer{}, nil)
	client := &ReconnectClient{limits: clientLimits{pingInterval: time.Millisecond, pingTimeout: time.Millisecond}}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	client.heartbeat(cancelled, nil)
	peer := newServerPeer(nil, 1, 1024, time.Millisecond)
	peer.close()
	done := make(chan struct{})
	go func() {
		peer.writeLoop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closed peer write loop did not return")
	}
}
