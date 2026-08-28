package extensions

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/counter"
	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/replica"
)

func TestDisabledHandlerDoesNotExposeEndpointsOrAuthenticate(t *testing.T) {
	called := false
	handler, err := NewHandler(Config{
		Authenticate: func(*http.Request) (Peer, error) {
			called = true
			return Peer{ID: "unexpected"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	response, err := http.Get(server.URL + "/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled endpoint status = %d, want 404", response.StatusCode)
	}
	if called {
		t.Fatal("disabled handler invoked authentication")
	}
}

func TestNewHandlerValidatesFeatureBoundaries(t *testing.T) {
	if _, err := NewHandler(Config{Features: Feature(1 << 7)}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("unknown feature error = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewHandler(Config{Features: FeatureWebSocket}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("missing boundaries error = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewHandler(Config{Features: FeatureHTTP, OriginPatterns: []string{"*"}}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("wildcard origin error = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewHandler(Config{Features: FeatureHTTP, OriginPatterns: []string{"["}}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("malformed origin error = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewHandler(Config{Features: FeatureHTTP, OriginPatterns: []string{"https://app.example"}}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("URL origin pattern error = %v, want ErrInvalidConfig", err)
	}
}

func TestMountNormalizesAndRoutesPrefix(t *testing.T) {
	server, _, manifest, _ := newCounterHandler(t, FeatureHTTP, nil)
	handler, ok := server.Config.Handler.(*Handler)
	if !ok {
		t.Fatal("test server handler has unexpected type")
	}
	mux := http.NewServeMux()
	if err := handler.Mount(mux, "/sync"); err != nil {
		t.Fatal(err)
	}
	mounted := httptest.NewServer(mux)
	defer mounted.Close()
	endpoint, err := httpGroupURL(mounted.URL+"/sync", manifest.GroupID, "changes")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer operator-a")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("mounted route status = %d, want %d", response.StatusCode, http.StatusMethodNotAllowed)
	}
	if err := handler.Mount(nil, "/"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil mux error = %v, want ErrInvalidConfig", err)
	}
	if err := handler.Mount(mux, "/sync"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("duplicate mount error = %v, want ErrInvalidConfig", err)
	}
	if _, err := normalizeMountPrefix("sync"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("relative prefix error = %v, want ErrInvalidConfig", err)
	}
}

func TestWebSocketReplicatesDuplicateAndOutOfOrderChanges(t *testing.T) {
	server, _, manifest, relay := newCounterHandler(t, FeatureWebSocket, nil)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	left := newWebSocketCounterClient(t, endpoint, manifest, "operator-a")
	right := newWebSocketCounterClient(t, endpoint, manifest, "operator-b")

	first := incrementChange(t, left.state, manifest, "operator-a", 1, 2)
	if err := left.client.Publish(context.Background(), first); err != nil {
		t.Fatalf("publish first: %v", err)
	}
	if err := left.client.Publish(context.Background(), first); err != nil {
		t.Fatalf("retry first: %v", err)
	}
	second := incrementChange(t, left.state, manifest, "operator-a", 2, 3)
	third := incrementChange(t, left.state, manifest, "operator-a", 3, 4)
	if err := left.client.Publish(context.Background(), third); err != nil {
		t.Fatalf("publish third: %v", err)
	}
	if err := left.client.Publish(context.Background(), second); err != nil {
		t.Fatalf("publish second: %v", err)
	}

	eventually(t, func() bool {
		return counterValue(t, relay) == 9 &&
			counterValue(t, left.state) == 9 &&
			counterValue(t, right.state) == 9 &&
			left.inbox.Frontier().Counter("operator-a") == 3 &&
			right.inbox.Frontier().Counter("operator-a") == 3
	})
}

func TestHTTPReplicatesDuplicateAndOutOfOrderChanges(t *testing.T) {
	server, _, manifest, relay := newCounterHandler(t, FeatureHTTP, nil)
	left := newHTTPCounterClient(t, server.URL, manifest, "operator-a")
	right := newHTTPCounterClient(t, server.URL, manifest, "operator-b")

	first := incrementChange(t, left.state, manifest, "operator-a", 1, 2)
	if err := left.client.Publish(context.Background(), first); err != nil {
		t.Fatalf("publish first: %v", err)
	}
	if err := left.client.Publish(context.Background(), first); err != nil {
		t.Fatalf("retry first: %v", err)
	}
	second := incrementChange(t, left.state, manifest, "operator-a", 2, 3)
	third := incrementChange(t, left.state, manifest, "operator-a", 3, 4)
	if err := left.client.Publish(context.Background(), third); err != nil {
		t.Fatalf("publish third: %v", err)
	}
	if err := left.client.Publish(context.Background(), second); err != nil {
		t.Fatalf("publish second: %v", err)
	}

	eventually(t, func() bool {
		return counterValue(t, relay) == 9 &&
			counterValue(t, left.state) == 9 &&
			counterValue(t, right.state) == 9 &&
			left.inbox.Frontier().Counter("operator-a") == 3 &&
			right.inbox.Frontier().Counter("operator-a") == 3
	})
}

func TestHandlerRejectsUnauthenticatedForgedAndCrossOriginHTTPRequests(t *testing.T) {
	server, group, manifest, relay := newCounterHandler(t, FeatureHTTP, nil)
	changesURL, err := httpGroupURL(server.URL, manifest.GroupID, "changes")
	if err != nil {
		t.Fatal(err)
	}
	eventsURL, err := httpGroupURL(server.URL, manifest.GroupID, "events")
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(eventsURL)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", response.StatusCode)
	}
	_ = response.Body.Close()

	crossOrigin, err := http.NewRequest(http.MethodGet, eventsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	crossOrigin.Header.Set("Authorization", "Bearer operator-a")
	crossOrigin.Header.Set("Origin", "https://evil.example")
	response, err = http.DefaultClient.Do(crossOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", response.StatusCode)
	}
	_ = response.Body.Close()

	writer, err := counter.NewGCounter("operator-b")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := writer.Increment(7)
	if err != nil {
		t.Fatal(err)
	}
	forged := newCounterChange(t, manifest, "operator-b", 1, delta)
	encoded, err := marshalChange(forged)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, changesURL, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer operator-a")
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("forged actor status = %d, want 403", response.StatusCode)
	}
	_ = response.Body.Close()
	if got := counterValue(t, relay); got != 0 || group.Frontier().Counter("operator-b") != 0 {
		t.Fatalf("forged actor changed relay = %d, frontier=%#v", got, group.Frontier().Entries())
	}
}

func TestHTTPValidatesMethodContentTypeAndBodyLimits(t *testing.T) {
	server, _, manifest, _ := newCounterHandler(t, FeatureHTTP, func(config *Config) {
		config.MaxMessageBytes = 1024
		config.MaxQueuedBytes = 1024
	})
	changesURL, err := httpGroupURL(server.URL, manifest.GroupID, "changes")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, changesURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer operator-a")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != http.MethodPost {
		t.Fatalf("wrong method response = %d, allow=%q", response.StatusCode, response.Header.Get("Allow"))
	}
	_ = response.Body.Close()

	request, err = http.NewRequest(http.MethodPost, changesURL, strings.NewReader("wrong"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer operator-a")
	request.Header.Set("Content-Type", "text/plain")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong content type status = %d, want 415", response.StatusCode)
	}
	_ = response.Body.Close()

	request, err = http.NewRequest(http.MethodPost, changesURL, bytes.NewReader(make([]byte, 1025)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer operator-a")
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413", response.StatusCode)
	}
	_ = response.Body.Close()
}

func TestGroupDropsSlowSubscribersWithoutUnboundedQueue(t *testing.T) {
	_, group, manifest, _ := newCounterHandler(t, FeatureHTTP, func(config *Config) {
		config.MaxMessageBytes = 1024
		config.MaxQueuedMessages = 1
		config.MaxQueuedBytes = 1024
	})
	slow := newSSESubscriber(1, 1024)
	group.add(slow)
	t.Cleanup(slow.close)
	writer, err := counter.NewGCounter("operator-a")
	if err != nil {
		t.Fatal(err)
	}
	firstDelta, err := writer.Increment(1)
	if err != nil {
		t.Fatal(err)
	}
	secondDelta, err := writer.Increment(1)
	if err != nil {
		t.Fatal(err)
	}
	first := newCounterChange(t, manifest, "operator-a", 1, firstDelta)
	second := newCounterChange(t, manifest, "operator-a", 2, secondDelta)
	firstWire, err := marshalChange(first)
	if err != nil {
		t.Fatal(err)
	}
	secondWire, err := marshalChange(second)
	if err != nil {
		t.Fatal(err)
	}
	authorize := func(peer Peer, _ replica.Manifest, dot replica.Dot) error {
		if peer.ID != dot.Actor {
			return ErrUnauthorized
		}
		return nil
	}
	if _, err := group.receive(Peer{ID: "operator-a"}, authorize, firstWire, 1024, 128); err != nil {
		t.Fatal(err)
	}
	if _, err := group.receive(Peer{ID: "operator-a"}, authorize, secondWire, 1024, 128); err != nil {
		t.Fatal(err)
	}
	select {
	case <-slow.queue.done:
	default:
		t.Fatal("slow subscriber was not closed after its bounded queue filled")
	}
	group.peersMu.Lock()
	_, retained := group.peers[slow]
	group.peersMu.Unlock()
	if retained {
		t.Fatal("slow subscriber remained registered after queue overflow")
	}
}

func TestPeerQueueHonorsMessageByteAndCloseBounds(t *testing.T) {
	queue := newPeerQueue(1, 3)
	if !queue.enqueue([]byte{1, 2, 3}) {
		t.Fatal("queue rejected a bounded message")
	}
	if queue.enqueue([]byte{4}) {
		t.Fatal("queue accepted a message beyond its byte budget")
	}
	data, ok := queue.dequeue()
	if !ok || !bytes.Equal(data, []byte{1, 2, 3}) {
		t.Fatalf("dequeue = %x, %t", data, ok)
	}
	if queue.queuedBytes.Load() != 0 {
		t.Fatalf("queued bytes = %d, want 0", queue.queuedBytes.Load())
	}
	queue.close()
	if queue.enqueue([]byte{1}) {
		t.Fatal("closed queue accepted a message")
	}
	if _, ok := queue.dequeue(); ok {
		t.Fatal("closed queue returned a message")
	}
}

func TestOriginValidationAndRouteEncoding(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://relay.example/http/groups/x/events", nil)
	request.Host = "relay.example"
	request.Header.Set("Origin", "https://app.example")
	handler := &Handler{origins: []string{"app.example", "*.internal.example"}}
	if !handler.originAllowed(request) {
		t.Fatal("configured origin was rejected")
	}
	request.Header.Set("Origin", "https://evil.example")
	if handler.originAllowed(request) {
		t.Fatal("unconfigured origin was accepted")
	}
	request.Header.Set("Origin", "not a URL")
	if handler.originAllowed(request) {
		t.Fatal("malformed origin was accepted")
	}
	request.Header.Set("Origin", "")
	if !handler.originAllowed(request) {
		t.Fatal("non-browser request was rejected")
	}

	url, err := httpGroupURL("https://relay.example/base", "团队/a", "events")
	if err != nil || !strings.Contains(url, "/http/groups/") || strings.Contains(url, "团队") {
		t.Fatalf("encoded URL = %q, %v", url, err)
	}
	if _, err := httpGroupURL("ws://relay.example", "group", "events"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid HTTP endpoint error = %v, want ErrInvalidConfig", err)
	}
}

type webSocketCounterClient struct {
	client *WebSocketClient
	state  *counter.GCounter
	inbox  *replica.Inbox
}

func newWebSocketCounterClient(t *testing.T, endpoint string, manifest replica.Manifest, actor string) webSocketCounterClient {
	t.Helper()
	state, inbox := newCounterInbox(t, manifest, actor)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := DialWebSocket(ctx, endpoint, manifest, ClientConfig{
		Header: http.Header{"Authorization": []string{"Bearer " + actor}},
		OnChange: func(change replica.Change) error {
			_, err := inbox.Receive(change)
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return webSocketCounterClient{client: client, state: state, inbox: inbox}
}

type httpCounterClient struct {
	client *HTTPClient
	state  *counter.GCounter
	inbox  *replica.Inbox
}

func newHTTPCounterClient(t *testing.T, endpoint string, manifest replica.Manifest, actor string) httpCounterClient {
	t.Helper()
	state, inbox := newCounterInbox(t, manifest, actor)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := ConnectHTTP(ctx, endpoint, manifest, ClientConfig{
		Header: http.Header{"Authorization": []string{"Bearer " + actor}},
		OnChange: func(change replica.Change) error {
			_, err := inbox.Receive(change)
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return httpCounterClient{client: client, state: state, inbox: inbox}
}

func newCounterHandler(t *testing.T, features Feature, mutate func(*Config)) (*httptest.Server, *Group, replica.Manifest, *counter.GCounter) {
	t.Helper()
	manifest, err := replica.NewManifest("counter-group", "example.com/counter/v1", 1, replica.Protocol{
		StateID:          crdt.TypeIDGCounterState,
		DeltaID:          crdt.TypeIDGCounterDelta,
		SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	relay, err := counter.NewGCounter("relay")
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
			return relay.ApplyDelta(delta)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		Features: features,
		Groups:   []*Group{group},
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
		AuthorizeSubscription: func(peer Peer, _ replica.Manifest) error {
			if peer.ID == "denied" {
				return ErrUnauthorized
			}
			return nil
		},
	}
	if mutate != nil {
		mutate(&config)
	}
	handler, err := NewHandler(config)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, group, manifest, relay
}

func newCounterInbox(t testing.TB, manifest replica.Manifest, actor string) (*counter.GCounter, *replica.Inbox) {
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
	return state, inbox
}

func incrementChange(t testing.TB, state *counter.GCounter, manifest replica.Manifest, actor string, sequence, amount uint64) replica.Change {
	t.Helper()
	delta, err := state.Increment(amount)
	if err != nil {
		t.Fatal(err)
	}
	return newCounterChange(t, manifest, actor, sequence, delta)
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

func counterValue(t *testing.T, state *counter.GCounter) uint64 {
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

func TestConcurrentHTTPPublishersConverge(t *testing.T) {
	server, group, manifest, relay := newCounterHandler(t, FeatureHTTP, nil)
	const writers = 8
	clients := make([]httpCounterClient, 0, writers)
	changes := make([]replica.Change, 0, writers)
	for index := 0; index < writers; index++ {
		actor := "writer-" + string(rune('a'+index))
		client := newHTTPCounterClient(t, server.URL, manifest, actor)
		clients = append(clients, client)
		changes = append(changes, incrementChange(t, client.state, manifest, actor, 1, uint64(index+1)))
	}
	var publish sync.WaitGroup
	errChan := make(chan error, writers)
	for index, client := range clients {
		publish.Add(1)
		go func(client httpCounterClient, change replica.Change) {
			defer publish.Done()
			errChan <- client.client.Publish(context.Background(), change)
		}(client, changes[index])
	}
	publish.Wait()
	close(errChan)
	for err := range errChan {
		if err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, func() bool {
		want := uint64(writers * (writers + 1) / 2)
		if counterValue(t, relay) != want || len(group.Frontier().Entries()) != writers {
			return false
		}
		for _, client := range clients {
			if counterValue(t, client.state) != want {
				return false
			}
		}
		return true
	})
}

func TestMixedTransportSimulationConvergesAcrossDuplicateAndReorderedPublishes(t *testing.T) {
	server, group, manifest, relay := newCounterHandler(t, FeatureWebSocket|FeatureHTTP, nil)
	wsEndpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	alice := newWebSocketCounterClient(t, wsEndpoint, manifest, "alice")
	bob := newHTTPCounterClient(t, server.URL, manifest, "bob")
	observer := newWebSocketCounterClient(t, wsEndpoint, manifest, "observer")

	aliceFirst := incrementChange(t, alice.state, manifest, "alice", 1, 1)
	aliceSecond := incrementChange(t, alice.state, manifest, "alice", 2, 2)
	aliceThird := incrementChange(t, alice.state, manifest, "alice", 3, 3)
	bobFirst := incrementChange(t, bob.state, manifest, "bob", 1, 1)
	bobSecond := incrementChange(t, bob.state, manifest, "bob", 2, 2)
	bobThird := incrementChange(t, bob.state, manifest, "bob", 3, 3)

	// This deterministic schedule models retries plus independent reordering on
	// two transports. The replica inbox must only broadcast accepted dots and
	// install each actor's contiguous sequence before convergence is asserted.
	for _, publish := range []func() error{
		func() error { return alice.client.Publish(context.Background(), aliceThird) },
		func() error { return bob.client.Publish(context.Background(), bobSecond) },
		func() error { return alice.client.Publish(context.Background(), aliceFirst) },
		func() error { return alice.client.Publish(context.Background(), aliceFirst) },
		func() error { return bob.client.Publish(context.Background(), bobFirst) },
		func() error { return bob.client.Publish(context.Background(), bobSecond) },
		func() error { return alice.client.Publish(context.Background(), aliceSecond) },
		func() error { return bob.client.Publish(context.Background(), bobThird) },
	} {
		if err := publish(); err != nil {
			t.Fatalf("mixed transport publish: %v", err)
		}
	}

	eventually(t, func() bool {
		return counterValue(t, relay) == 12 &&
			counterValue(t, alice.state) == 12 &&
			counterValue(t, bob.state) == 12 &&
			counterValue(t, observer.state) == 12 &&
			group.Frontier().Counter("alice") == 3 &&
			group.Frontier().Counter("bob") == 3 &&
			observer.inbox.Frontier().Counter("alice") == 3 &&
			observer.inbox.Frontier().Counter("bob") == 3
	})
}
