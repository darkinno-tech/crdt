// Command websocket-provider runs the official WebSocket transport reference
// against two in-memory counter replicas. The provider is intentionally a
// reference implementation, not a production replication service.
package main

import (
	"context"
	"errors"
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
	"github.com/darkinno-tech/crdt/examples/websocket-provider/provider"
	"github.com/darkinno-tech/crdt/replica"
)

var receiveLimits = frame.DecoderLimits{
	MaxFrameBytes:  1 << 20,
	MaxPayload:     1 << 20,
	MaxCodecID:     128,
	MaxElements:    1024,
	MaxTags:        1024,
	MaxStringBytes: 1024,
}

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(writer io.Writer) error {
	manifest, err := replica.NewManifest("demo-counter", "example.com/demo-counter/v1", 1, replica.Protocol{
		StateID:          crdt.TypeIDGCounterState,
		DeltaID:          crdt.TypeIDGCounterDelta,
		SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		return err
	}
	relay, err := counter.NewGCounter("relay")
	if err != nil {
		return err
	}
	group, err := provider.NewGroup(provider.GroupConfig{
		Manifest:          manifest,
		MaxPendingChanges: 8,
		MaxPendingBytes:   64 << 10,
		Apply: func(data []byte) error {
			delta, err := counter.UnmarshalGCounterDeltaWithLimits(data, receiveLimits)
			if err != nil {
				return err
			}
			return relay.ApplyDelta(delta)
		},
	})
	if err != nil {
		return fmt.Errorf("create provider group: %w", err)
	}
	handler, err := provider.NewHandler(provider.Config{
		Groups: []*provider.Group{group},
		Authenticate: func(request *http.Request) (provider.Peer, error) {
			const prefix = "Bearer "
			value := request.Header.Get("Authorization")
			if !strings.HasPrefix(value, prefix) {
				return provider.Peer{}, provider.ErrUnauthorized
			}
			return provider.Peer{ID: strings.TrimPrefix(value, prefix)}, nil
		},
		Authorize: func(peer provider.Peer, _ replica.Manifest, dot replica.Dot) error {
			if peer.ID != dot.Actor {
				return provider.ErrUnauthorized
			}
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("create provider handler: %w", err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")

	left, err := connectCounter(context.Background(), endpoint, manifest, "operator-a")
	if err != nil {
		return fmt.Errorf("connect left replica: %w", err)
	}
	defer left.close()
	right, err := connectCounter(context.Background(), endpoint, manifest, "operator-b")
	if err != nil {
		return fmt.Errorf("connect right replica: %w", err)
	}
	defer right.close()

	firstDelta, err := left.counter.Increment(2)
	if err != nil {
		return err
	}
	secondDelta, err := left.counter.Increment(3)
	if err != nil {
		return err
	}
	first, err := counterChange(manifest, "operator-a", 1, firstDelta)
	if err != nil {
		return err
	}
	second, err := counterChange(manifest, "operator-a", 2, secondDelta)
	if err != nil {
		return err
	}
	// Send the later dot first. Both provider and client inboxes retain it
	// without advancing their delivery frontier until dot 1 arrives.
	if err := left.client.Publish(context.Background(), second); err != nil {
		return fmt.Errorf("publish second change: %w", err)
	}
	if err := left.client.Publish(context.Background(), first); err != nil {
		return fmt.Errorf("publish first change: %w", err)
	}
	if err := left.client.Publish(context.Background(), first); err != nil {
		return fmt.Errorf("retry first change: %w", err)
	}
	if err := waitForCounter(relay, left.counter, right.counter, group, 5); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "relay-value=5\nleft-value=5\nright-value=5\nfrontier-operator-a=%d\nduplicate-and-out-of-order-safe=true\n", group.Frontier().Counter("operator-a")); err != nil {
		return fmt.Errorf("write WebSocket provider result: %w", err)
	}
	return nil
}

type connectedCounter struct {
	client  *provider.Client
	counter *counter.GCounter
	inbox   *replica.Inbox
}

func connectCounter(ctx context.Context, endpoint string, manifest replica.Manifest, actor string) (*connectedCounter, error) {
	state, err := counter.NewGCounter(actor)
	if err != nil {
		return nil, err
	}
	frontier, err := replica.NewFrontier(nil)
	if err != nil {
		return nil, err
	}
	inbox, err := replica.NewInbox(manifest, frontier, 8, 64<<10, func(data []byte) error {
		delta, err := counter.UnmarshalGCounterDeltaWithLimits(data, receiveLimits)
		if err != nil {
			return err
		}
		return state.ApplyDelta(delta)
	})
	if err != nil {
		return nil, err
	}
	client, err := provider.Dial(ctx, endpoint, manifest, provider.ClientConfig{
		Header: http.Header{"Authorization": []string{"Bearer " + actor}},
		OnChange: func(change replica.Change) error {
			_, err := inbox.Receive(change)
			return err
		},
	})
	if err != nil {
		return nil, err
	}
	return &connectedCounter{client: client, counter: state, inbox: inbox}, nil
}

func (connected *connectedCounter) close() {
	if connected != nil && connected.client != nil {
		_ = connected.client.Close()
	}
}

func counterChange(manifest replica.Manifest, actor string, sequence uint64, delta counter.GCounterDelta) (replica.Change, error) {
	encoded, err := delta.MarshalBinary()
	if err != nil {
		return replica.Change{}, err
	}
	return replica.NewChange(manifest, replica.Dot{Actor: actor, Counter: sequence}, encoded)
}

func waitForCounter(relay, left, right *counter.GCounter, group *provider.Group, want uint64) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		relayValue, relayErr := relay.Value()
		leftValue, leftErr := left.Value()
		rightValue, rightErr := right.Value()
		if relayErr != nil || leftErr != nil || rightErr != nil {
			return errors.New("read counter value")
		}
		if relayValue == want && leftValue == want && rightValue == want && group.Frontier().Counter("operator-a") == 2 {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return errors.New("WebSocket provider did not converge before timeout")
}
