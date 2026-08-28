package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/im10furry/crdt/counter"
	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/replica"
)

func TestGroupDoesNotBroadcastKnownDotsAgain(t *testing.T) {
	_, group, manifest, relay := newCounterServer(t)
	observer := &connection{
		context:  context.Background(),
		cancel:   func() {},
		outbound: make(chan []byte, 2),
	}
	group.add(observer)
	t.Cleanup(func() { group.remove(observer) })

	authorize := func(peer Peer, _ replica.Manifest, dot replica.Dot) error {
		if peer.ID != dot.Actor {
			return ErrUnauthorized
		}
		return nil
	}
	writer, err := counter.NewGCounter("operator-a")
	if err != nil {
		t.Fatal(err)
	}
	firstDelta, err := writer.Increment(1)
	if err != nil {
		t.Fatal(err)
	}
	first := newCounterChange(t, manifest, "operator-a", 1, firstDelta)
	firstWire, err := marshalChange(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := group.receive(Peer{ID: "operator-a"}, authorize, firstWire, 1<<20, 128); err != nil {
		t.Fatalf("receive first change: %v", err)
	}
	select {
	case <-observer.outbound:
	default:
		t.Fatal("accepted change was not broadcast")
	}

	conflictingWriter, err := counter.NewGCounter("different-source")
	if err != nil {
		t.Fatal(err)
	}
	conflictingDelta, err := conflictingWriter.Increment(7)
	if err != nil {
		t.Fatal(err)
	}
	conflicting := newCounterChange(t, manifest, "operator-a", 1, conflictingDelta)
	conflictingWire, err := marshalChange(conflicting)
	if err != nil {
		t.Fatal(err)
	}
	if err := group.receive(Peer{ID: "operator-a"}, authorize, conflictingWire, 1<<20, 128); err != nil {
		t.Fatalf("receive conflicting installed dot: %v", err)
	}
	select {
	case repeated := <-observer.outbound:
		t.Fatalf("installed dot was broadcast again: %x", repeated)
	default:
	}
	if got := counterValue(t, relay); got != 1 {
		t.Fatalf("conflicting installed dot changed relay state to %d", got)
	}

	futureDelta, err := writer.Increment(2)
	if err != nil {
		t.Fatal(err)
	}
	future := newCounterChange(t, manifest, "operator-a", 3, futureDelta)
	futureWire, err := marshalChange(future)
	if err != nil {
		t.Fatal(err)
	}
	if err := group.receive(Peer{ID: "operator-a"}, authorize, futureWire, 1<<20, 128); err != nil {
		t.Fatalf("receive future change: %v", err)
	}
	select {
	case <-observer.outbound:
	default:
		t.Fatal("accepted buffered change was not broadcast")
	}
	if err := group.receive(Peer{ID: "operator-a"}, authorize, futureWire, 1<<20, 128); err != nil {
		t.Fatalf("receive duplicate buffered change: %v", err)
	}
	select {
	case repeated := <-observer.outbound:
		t.Fatalf("buffered dot was broadcast again: %x", repeated)
	default:
	}
	if pending, _ := group.Pending(); pending != 1 {
		t.Fatalf("pending changes = %d, want 1", pending)
	}
}

func TestProviderDoesNotRelayInstalledDotOverWebSocket(t *testing.T) {
	server, _, manifest, relay := newCounterServer(t)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	publisher := newCounterClient(t, endpoint, manifest, "operator-a")

	observerState, err := counter.NewGCounter("observer")
	if err != nil {
		t.Fatal(err)
	}
	frontier, err := replica.NewFrontier(nil)
	if err != nil {
		t.Fatal(err)
	}
	observerInbox, err := replica.NewInbox(manifest, frontier, 8, 16<<10, func(data []byte) error {
		delta, err := counter.UnmarshalGCounterDeltaWithLimits(data, frame.DefaultLimits())
		if err != nil {
			return err
		}
		return observerState.ApplyDelta(delta)
	})
	if err != nil {
		t.Fatal(err)
	}
	observed := make(chan replica.Change, 2)
	observer, err := Dial(context.Background(), endpoint, manifest, ClientConfig{
		Header: http.Header{"Authorization": []string{"Bearer operator-b"}},
		OnChange: func(change replica.Change) error {
			if _, err := observerInbox.Receive(change); err != nil {
				return err
			}
			observed <- change
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close() })

	firstDelta, err := publisher.state.Increment(1)
	if err != nil {
		t.Fatal(err)
	}
	first := newCounterChange(t, manifest, "operator-a", 1, firstDelta)
	if err := publisher.client.Publish(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	select {
	case change := <-observed:
		if change.Dot != first.Dot {
			t.Fatalf("first observed dot = %#v, want %#v", change.Dot, first.Dot)
		}
	case <-time.After(time.Second):
		t.Fatal("observer did not receive the accepted change")
	}

	conflictingWriter, err := counter.NewGCounter("different-source")
	if err != nil {
		t.Fatal(err)
	}
	conflictingDelta, err := conflictingWriter.Increment(7)
	if err != nil {
		t.Fatal(err)
	}
	conflicting := newCounterChange(t, manifest, "operator-a", 1, conflictingDelta)
	if err := publisher.client.Publish(context.Background(), conflicting); err != nil {
		t.Fatal(err)
	}
	select {
	case change := <-observed:
		t.Fatalf("known dot was relayed again: %#v", change.Dot)
	case <-time.After(150 * time.Millisecond):
	}
	if got := counterValue(t, observerState); got != 1 {
		t.Fatalf("conflicting installed dot changed observer state to %d", got)
	}
	if got := counterValue(t, relay); got != 1 {
		t.Fatalf("conflicting installed dot changed relay state to %d", got)
	}
}
