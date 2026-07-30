package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/counter"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/replica"
)

func TestProviderReplicatesDuplicateAndOutOfOrderChanges(t *testing.T) {
	server, group, manifest, state := newCounterServer(t)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	left := newCounterClient(t, endpoint, manifest, "operator-a")
	right := newCounterClient(t, endpoint, manifest, "operator-b")

	firstDelta, err := left.state.Increment(2)
	if err != nil {
		t.Fatal(err)
	}
	first := newCounterChange(t, manifest, "operator-a", 1, firstDelta)
	if err := left.client.Publish(context.Background(), first); err != nil {
		t.Fatalf("publish first: %v", err)
	}
	// Re-sending an ambiguous first write is safe. The provider does not relay
	// the known Dot again, and every inbox remains idempotent if another peer
	// retries it after reconnecting.
	if err := left.client.Publish(context.Background(), first); err != nil {
		t.Fatalf("publish duplicate: %v", err)
	}

	secondDelta, err := left.state.Increment(3)
	if err != nil {
		t.Fatal(err)
	}
	thirdDelta, err := left.state.Increment(4)
	if err != nil {
		t.Fatal(err)
	}
	second := newCounterChange(t, manifest, "operator-a", 2, secondDelta)
	third := newCounterChange(t, manifest, "operator-a", 3, thirdDelta)
	if err := left.client.Publish(context.Background(), third); err != nil {
		t.Fatalf("publish out-of-order third: %v", err)
	}
	eventually(t, func() bool {
		changes, _ := group.Pending()
		return changes == 1
	})
	if err := left.client.Publish(context.Background(), second); err != nil {
		t.Fatalf("publish second: %v", err)
	}

	eventually(t, func() bool {
		return counterValue(t, state) == 9 &&
			counterValue(t, left.state) == 9 &&
			counterValue(t, right.state) == 9 &&
			group.Frontier().Counter("operator-a") == 3 &&
			left.inbox.Frontier().Counter("operator-a") == 3 &&
			right.inbox.Frontier().Counter("operator-a") == 3
	})
	if changes, bytes := group.Pending(); changes != 0 || bytes != 0 {
		t.Fatalf("pending = %d changes, %d bytes", changes, bytes)
	}
}

func TestHandlerRejectsUnauthenticatedAndForgedActors(t *testing.T) {
	server, group, manifest, state := newCounterServer(t)
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	client := newCounterClient(t, endpoint, manifest, "operator-a")
	forgedWriter, err := counter.NewGCounter("operator-b")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := forgedWriter.Increment(7)
	if err != nil {
		t.Fatal(err)
	}
	forged := newCounterChange(t, manifest, "operator-b", 1, delta)
	if err := client.client.Publish(context.Background(), forged); err != nil {
		t.Fatalf("write forged frame: %v", err)
	}
	eventually(t, func() bool {
		select {
		case <-client.client.Done():
			return true
		default:
			return false
		}
	})
	if got := counterValue(t, state); got != 0 {
		t.Fatalf("server accepted forged change: %d", got)
	}
	if group.Frontier().Counter("operator-b") != 0 {
		t.Fatalf("server frontier accepted forged actor: %v", group.Frontier().Entries())
	}
}

func TestDialRejectsMismatchedManifest(t *testing.T) {
	server, _, manifest, _ := newCounterServer(t)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	wrong, err := replica.NewManifest(manifest.GroupID, "example.com/other-counter/v1", manifest.Epoch, manifest.Protocol, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = Dial(ctx, endpoint, wrong, ClientConfig{
		Header: http.Header{"Authorization": []string{"Bearer operator-a"}},
		OnChange: func(replica.Change) error {
			return nil
		},
	})
	if err == nil {
		t.Fatal("Dial accepted a mismatched manifest")
	}
}

func TestNewHandlerRequiresAuthenticationAndAuthorization(t *testing.T) {
	if _, err := NewHandler(Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty config error = %v, want ErrInvalidConfig", err)
	}
}

func TestChangeWireRejectsNonCanonicalAndTrailingData(t *testing.T) {
	if _, _, err := unmarshalChange([]byte{protocolVersion, 0x81, 0x00}, 1024, 128); !errors.Is(err, errInvalidWireMessage) {
		t.Fatalf("non-canonical actor length error = %v, want invalid wire message", err)
	}
	_, _, manifest, _ := newCounterServer(t)
	writer, err := counter.NewGCounter("operator-a")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := writer.Increment(1)
	if err != nil {
		t.Fatal(err)
	}
	change := newCounterChange(t, manifest, "operator-a", 1, delta)
	encoded, err := marshalChange(change)
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, 0)
	if _, _, err := unmarshalChange(encoded, 1024, 128); !errors.Is(err, errInvalidWireMessage) {
		t.Fatalf("trailing data error = %v, want invalid wire message", err)
	}
}

type counterClient struct {
	client *Client
	state  *counter.GCounter
	inbox  *replica.Inbox
}

func newCounterServer(t testing.TB) (*httptest.Server, *Group, replica.Manifest, *counter.GCounter) {
	t.Helper()
	manifest, err := replica.NewManifest("example-counter", "example.com/counter/v1", 1, replica.Protocol{
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
		MaxPendingChanges: 8,
		MaxPendingBytes:   16 << 10,
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
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, group, manifest, state
}

func newCounterClient(t testing.TB, endpoint string, manifest replica.Manifest, actor string) counterClient {
	t.Helper()
	state, err := counter.NewGCounter(actor)
	if err != nil {
		t.Fatal(err)
	}
	frontier, err := replica.NewFrontier(nil)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := replica.NewInbox(manifest, frontier, 8, 16<<10, func(data []byte) error {
		delta, err := counter.UnmarshalGCounterDeltaWithLimits(data, frame.DefaultLimits())
		if err != nil {
			return err
		}
		return state.ApplyDelta(delta)
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := Dial(ctx, endpoint, manifest, ClientConfig{
		Header: http.Header{"Authorization": []string{"Bearer " + actor}},
		OnChange: func(change replica.Change) error {
			_, err := inbox.Receive(change)
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
	})
	return counterClient{client: client, state: state, inbox: inbox}
}

func newCounterChange(t testing.TB, manifest replica.Manifest, actor string, sequence uint64, delta counter.GCounterDelta) replica.Change {
	t.Helper()
	encoded, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	change, err := replica.NewChange(manifest, replica.Dot{Actor: actor, Counter: sequence}, encoded)
	if err != nil {
		t.Fatal(err)
	}
	return change
}

func counterValue(t testing.TB, state *counter.GCounter) uint64 {
	t.Helper()
	value, err := state.Value()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}
