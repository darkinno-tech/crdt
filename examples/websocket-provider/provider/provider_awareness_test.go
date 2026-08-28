package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/awareness"
	"github.com/darkinno-tech/crdt/counter"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/replica"
)

func TestAwarenessRequiresV3AuthorizationAndSyncsLiveSnapshot(t *testing.T) {
	server, group, manifest := newCounterAwarenessServer(t)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")

	observerStore := mustAwarenessStore(t)
	observerUpdates := make(chan awareness.Update, 4)
	observer := newAwarenessClient(t, endpoint, manifest, "bob", observerStore, observerUpdates)

	publisherStore := mustAwarenessStore(t)
	publisher := newAwarenessClient(t, endpoint, manifest, "alice", publisherStore, nil)
	update, err := publisherStore.Set("alice", []byte(`{"cursor":{"anchor":"alice:3","association":"before"},"name":"Alice"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.PublishAwareness(context.Background(), update); err != nil {
		t.Fatal(err)
	}

	select {
	case received := <-observerUpdates:
		if received.Actor != "alice" || received.Clock != 1 || string(received.State) != `{"cursor":{"anchor":"alice:3","association":"before"},"name":"Alice"}` {
			t.Fatalf("observer update = %#v", received)
		}
	case <-time.After(time.Second):
		t.Fatal("observer did not receive awareness")
	}
	eventually(t, func() bool {
		states := group.Awareness()
		return len(states) == 1 && states[0].Actor == "alice" && states[0].Clock == 1
	})

	joiningStore := mustAwarenessStore(t)
	joiningUpdates := make(chan awareness.Update, 2)
	joining := newAwarenessClient(t, endpoint, manifest, "carol", joiningStore, joiningUpdates)
	select {
	case received := <-joiningUpdates:
		if received.Actor != "alice" || received.Clock != 1 {
			t.Fatalf("joining snapshot = %#v", received)
		}
	case <-time.After(time.Second):
		t.Fatal("new v3 peer did not receive live awareness snapshot")
	}
	if got := joiningStore.ActiveAt(time.Now()); len(got) != 1 || got[0].Actor != "alice" {
		t.Fatalf("joining store = %#v", got)
	}

	legacy := newCounterClient(t, endpoint, manifest, "legacy")
	if err := legacy.client.PublishAwareness(context.Background(), update); !errors.Is(err, ErrAwarenessUnsupported) {
		t.Fatalf("v1 PublishAwareness = %v, want %v", err, ErrAwarenessUnsupported)
	}
	// Awareness is a v3-only envelope. A v1 peer must neither receive it nor
	// disconnect; it must still be able to publish ordinary CRDT changes.
	select {
	case <-legacy.client.Done():
		t.Fatal("v1 client disconnected after a v3 awareness broadcast")
	case <-time.After(100 * time.Millisecond):
	}
	legacyDelta, err := legacy.state.Increment(2)
	if err != nil {
		t.Fatal(err)
	}
	legacyChange := newCounterChange(t, manifest, "legacy", 1, legacyDelta)
	if err := legacy.client.Publish(context.Background(), legacyChange); err != nil {
		t.Fatalf("v1 publish after awareness = %v", err)
	}
	eventually(t, func() bool {
		return group.Frontier().Counter("legacy") == 1
	})

	attackerStore := mustAwarenessStore(t)
	attackerUpdates := make(chan awareness.Update, 1)
	attacker := newAwarenessClient(t, endpoint, manifest, "eve", attackerStore, attackerUpdates)
	// Dial returns after the handshake response, before the server has necessarily
	// delivered its live-awareness snapshot. Wait for that snapshot before
	// installing the forged update locally: otherwise a concurrent snapshot and
	// forged value can legitimately conflict at clock 1, closing the client before
	// the authorization path under test receives the forged message.
	select {
	case received := <-attackerUpdates:
		if received.Actor != "alice" || received.Clock != 1 {
			t.Fatalf("attacker snapshot = %#v", received)
		}
	case <-time.After(time.Second):
		t.Fatal("attacker did not receive live awareness snapshot")
	}
	forged, err := attackerStore.Set("alice", []byte(`{"name":"forged"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := attacker.PublishAwareness(context.Background(), forged); err != nil {
		t.Fatal(err)
	}
	select {
	case <-attacker.Done():
	case <-time.After(time.Second):
		t.Fatal("unauthorized awareness actor did not close the connection")
	}
	if states := group.Awareness(); len(states) != 1 || string(states[0].State) != `{"cursor":{"anchor":"alice:3","association":"before"},"name":"Alice"}` {
		t.Fatalf("unauthorized update changed relay awareness: %#v", states)
	}

	_ = observer.Close()
	_ = publisher.Close()
	_ = joining.Close()
}

func TestNewHandlerRejectsAwarenessWithoutDedicatedAuthorizer(t *testing.T) {
	manifest, err := replica.NewManifest("presence-auth", "example.com/counter/v1", 1, replica.Protocol{
		StateID:          crdt.TypeIDGCounterState,
		DeltaID:          crdt.TypeIDGCounterDelta,
		SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	state, err := counter.NewGCounter("relay")
	if err != nil {
		t.Fatal(err)
	}
	group, err := NewGroup(GroupConfig{
		Manifest:          manifest,
		MaxPendingChanges: 1,
		MaxPendingBytes:   1024,
		Awareness:         mustAwarenessStore(t),
		Apply: func(data []byte) error {
			delta, err := counter.UnmarshalGCounterDeltaWithLimits(data, frame.DefaultLimits())
			if err != nil {
				return err
			}
			return state.ApplyDelta(delta)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewHandler(Config{
		Groups:       []*Group{group},
		Authenticate: func(*http.Request) (Peer, error) { return Peer{ID: "alice"}, nil },
		Authorize:    func(Peer, replica.Manifest, replica.Dot) error { return nil },
	})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewHandler without awareness authorizer = %v, want %v", err, ErrInvalidConfig)
	}
}

func newCounterAwarenessServer(t testing.TB) (*httptest.Server, *Group, replica.Manifest) {
	t.Helper()
	manifest, err := replica.NewManifest("presence-counter", "example.com/counter/v1", 1, replica.Protocol{
		StateID:          crdt.TypeIDGCounterState,
		DeltaID:          crdt.TypeIDGCounterDelta,
		SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	state, err := counter.NewGCounter("presence-relay")
	if err != nil {
		t.Fatal(err)
	}
	group, err := NewGroup(GroupConfig{
		Manifest:          manifest,
		MaxPendingChanges: 8,
		MaxPendingBytes:   16 << 10,
		Awareness:         mustAwarenessStore(t),
		Apply: func(data []byte) error {
			delta, err := counter.UnmarshalGCounterDeltaWithLimits(data, frame.DefaultLimits())
			if err != nil {
				return err
			}
			return state.ApplyDelta(delta)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(Config{
		Groups: []*Group{group},
		Authenticate: func(request *http.Request) (Peer, error) {
			const prefix = "Bearer "
			value := request.Header.Get("Authorization")
			if !strings.HasPrefix(value, prefix) {
				return Peer{}, ErrUnauthorized
			}
			return Peer{ID: strings.TrimPrefix(value, prefix)}, nil
		},
		Authorize: func(peer Peer, _ replica.Manifest, dot replica.Dot) error {
			if peer.ID != dot.Actor {
				return ErrUnauthorized
			}
			return nil
		},
		AuthorizeAwareness: func(peer Peer, _ replica.Manifest, update awareness.Update) error {
			if peer.ID != update.Actor {
				return ErrUnauthorized
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, group, manifest
}

func newAwarenessClient(t testing.TB, endpoint string, manifest replica.Manifest, actor string, store *awareness.Store, observed chan<- awareness.Update) *Client {
	t.Helper()
	client, err := Dial(context.Background(), endpoint, manifest, ClientConfig{
		Header:          http.Header{"Authorization": []string{"Bearer " + actor}},
		EnableAwareness: true,
		OnChange:        func(replica.Change) error { return nil },
		OnAwareness: func(update awareness.Update) error {
			if _, err := store.Apply(update, time.Now()); err != nil {
				return err
			}
			if observed != nil {
				select {
				case observed <- update:
				default:
				}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func mustAwarenessStore(t testing.TB) *awareness.Store {
	t.Helper()
	store, err := awareness.NewStore(awareness.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	return store
}
