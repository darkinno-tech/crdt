// Command extensions-provider demonstrates the opt-in WebSocket and HTTP/SSE
// relay surfaces mounted into an application-owned HTTP mux.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/counter"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/extensions"
	"github.com/darkinno-tech/crdt/replica"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output io.Writer) error {
	manifest, err := replica.NewManifest("counter-demo", "example.com/extensions-provider/v1", 1, replica.Protocol{
		StateID:          crdt.TypeIDGCounterState,
		DeltaID:          crdt.TypeIDGCounterDelta,
		SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		return fmt.Errorf("create manifest: %w", err)
	}
	relay, err := counter.NewGCounter("relay")
	if err != nil {
		return fmt.Errorf("create relay: %w", err)
	}
	group, err := extensions.NewGroup(extensions.GroupConfig{
		Manifest:          manifest,
		MaxPendingChanges: 64,
		MaxPendingBytes:   1 << 20,
		Apply: func(data []byte) error {
			delta, err := counter.UnmarshalGCounterDeltaWithLimits(data, frame.DefaultLimits())
			if err != nil {
				return err
			}
			return relay.ApplyDelta(delta)
		},
	})
	if err != nil {
		return fmt.Errorf("create relay group: %w", err)
	}

	handler, err := extensions.NewHandler(extensions.Config{
		Features: extensions.FeatureWebSocket | extensions.FeatureHTTP,
		Groups:   []*extensions.Group{group},
		Authenticate: func(request *http.Request) (extensions.Peer, error) {
			const prefix = "Bearer "
			actor := strings.TrimPrefix(request.Header.Get("Authorization"), prefix)
			if actor == "" || actor == request.Header.Get("Authorization") {
				return extensions.Peer{}, extensions.ErrUnauthorized
			}
			return extensions.Peer{ID: actor}, nil
		},
		Authorize: func(peer extensions.Peer, _ replica.Manifest, dot replica.Dot) error {
			if peer.ID != dot.Actor {
				return extensions.ErrUnauthorized
			}
			return nil
		},
		AuthorizeSubscription: func(extensions.Peer, replica.Manifest) error {
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("configure extensions: %w", err)
	}
	mux := http.NewServeMux()
	if err := handler.Mount(mux, "/crdt/"); err != nil {
		return fmt.Errorf("mount extensions: %w", err)
	}
	server := httptest.NewServer(mux)
	defer server.Close()

	aliceState, aliceInbox, err := newReceiver(manifest, "alice")
	if err != nil {
		return err
	}
	bobState, bobInbox, err := newReceiver(manifest, "bob")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	websocketClient, err := extensions.DialWebSocket(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/crdt/ws", manifest, extensions.ClientConfig{
		Header: bearerHeader("alice"),
		OnChange: func(change replica.Change) error {
			_, err := aliceInbox.Receive(change)
			return err
		},
	})
	if err != nil {
		return fmt.Errorf("connect WebSocket: %w", err)
	}
	defer func() { _ = websocketClient.Close() }()
	httpClient, err := extensions.ConnectHTTP(ctx, server.URL+"/crdt", manifest, extensions.ClientConfig{
		Header: bearerHeader("bob"),
		OnChange: func(change replica.Change) error {
			_, err := bobInbox.Receive(change)
			return err
		},
	})
	if err != nil {
		return fmt.Errorf("connect HTTP: %w", err)
	}
	defer func() { _ = httpClient.Close() }()

	aliceChange, err := incrementChange(aliceState, manifest, "alice", 1, 2)
	if err != nil {
		return err
	}
	if err := websocketClient.Publish(ctx, aliceChange); err != nil {
		return fmt.Errorf("publish WebSocket change: %w", err)
	}
	if err := waitForValue(bobState, 2); err != nil {
		return fmt.Errorf("wait for HTTP receiver: %w", err)
	}
	websocketToHTTP, err := bobState.Value()
	if err != nil {
		return err
	}

	bobChange, err := incrementChange(bobState, manifest, "bob", 1, 3)
	if err != nil {
		return err
	}
	if err := httpClient.Publish(ctx, bobChange); err != nil {
		return fmt.Errorf("publish HTTP change: %w", err)
	}
	if err := waitForValue(aliceState, 5); err != nil {
		return fmt.Errorf("wait for WebSocket receiver: %w", err)
	}
	httpToWebSocket, err := aliceState.Value()
	if err != nil {
		return err
	}
	relayValue, err := relay.Value()
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "websocket_to_http=%d\nhttp_to_websocket=%d\nrelay=%d\n", websocketToHTTP, httpToWebSocket, relayValue); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return nil
}

func newReceiver(manifest replica.Manifest, actor string) (*counter.GCounter, *replica.Inbox, error) {
	state, err := counter.NewGCounter(actor)
	if err != nil {
		return nil, nil, err
	}
	frontier, err := replica.NewFrontier(nil)
	if err != nil {
		return nil, nil, err
	}
	inbox, err := replica.NewInbox(manifest, frontier, 64, 1<<20, func(data []byte) error {
		delta, err := counter.UnmarshalGCounterDeltaWithLimits(data, frame.DefaultLimits())
		if err != nil {
			return err
		}
		return state.ApplyDelta(delta)
	})
	if err != nil {
		return nil, nil, err
	}
	return state, inbox, nil
}

func incrementChange(state *counter.GCounter, manifest replica.Manifest, actor string, sequence, amount uint64) (replica.Change, error) {
	delta, err := state.Increment(amount)
	if err != nil {
		return replica.Change{}, err
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		return replica.Change{}, err
	}
	return replica.NewChange(manifest, replica.Dot{Actor: actor, Counter: sequence}, encoded)
}

func bearerHeader(actor string) http.Header {
	return http.Header{"Authorization": []string{"Bearer " + actor}}
}

func waitForValue(state *counter.GCounter, want uint64) error {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		current, err := state.Value()
		if err != nil {
			return err
		}
		if current == want {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	current, err := state.Value()
	if err != nil {
		return err
	}
	return fmt.Errorf("value = %d, want %d", current, want)
}
