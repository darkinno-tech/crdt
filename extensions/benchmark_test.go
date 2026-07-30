package extensions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DarkInno/crdt/counter"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/replica"
	"github.com/DarkInno/crdt/telemetry"
	"google.golang.org/grpc/metadata"
)

func BenchmarkHandlerRecordDisabled(b *testing.B) {
	handler := &Handler{}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		handler.record("append", time.Time{}, nil)
	}
}

func BenchmarkHandlerRecordOverloaded(b *testing.B) {
	block := make(chan struct{})
	reporter, err := telemetry.New(telemetry.Options{QueueSize: 1, Sink: func(telemetry.Event) { <-block }})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		reporter.Close()
		close(block)
		<-reporter.Done()
	})
	handler := &Handler{telemetry: reporter}
	handler.record("append", handler.started(), nil)
	time.Sleep(time.Millisecond)
	handler.record("append", handler.started(), nil)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		handler.record("append", handler.started(), nil)
	}
}

func BenchmarkGroupReceiveLoopback(b *testing.B) {
	manifest := testManifest(b, "benchmark-group")
	relay, err := counter.NewGCounter("relay")
	if err != nil {
		b.Fatal(err)
	}
	group, err := NewGroup(GroupConfig{
		Manifest:          manifest,
		MaxPendingChanges: 128,
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
		b.Fatal(err)
	}
	writer, err := counter.NewGCounter("writer")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		change := benchmarkCounterChange(b, writer, manifest, uint64(index+1))
		encoded, err := marshalChange(change)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := group.receive(Peer{ID: "writer"}, benchmarkAuthorize, encoded, 1<<20, 128); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWebSocketPublishLoopback(b *testing.B) {
	server, manifest := newBenchmarkRelay(b, FeatureWebSocket)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	delivered := make(chan struct{}, 1)
	client, err := DialWebSocket(context.Background(), endpoint, manifest, ClientConfig{
		Header: bearerHeader("writer"),
		OnChange: func(replica.Change) error {
			delivered <- struct{}{}
			return nil
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = client.Close() })
	benchmarkPublish(b, manifest, func(change replica.Change) error {
		return client.Publish(context.Background(), change)
	}, delivered)
}

func BenchmarkWebSocketBatchPublishLoopback(b *testing.B) {
	const batchSize = 8
	server, manifest := newBenchmarkRelay(b, FeatureWebSocket|FeatureWebSocketBatch)
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	delivered := make(chan struct{}, batchSize)
	client, err := DialWebSocket(context.Background(), endpoint, manifest, ClientConfig{
		Header:        bearerHeader("writer"),
		EnableBatches: true,
		OnChange: func(replica.Change) error {
			delivered <- struct{}{}
			return nil
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = client.Close() })
	writer, err := counter.NewGCounter("writer")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		changes := make([]replica.Change, 0, batchSize)
		for index := 0; index < batchSize; index++ {
			changes = append(changes, benchmarkCounterChange(b, writer, manifest, uint64(iteration*batchSize+index+1)))
		}
		if err := client.PublishBatch(context.Background(), changes); err != nil {
			b.Fatal(err)
		}
		for index := 0; index < batchSize; index++ {
			<-delivered
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(batchSize), "changes/op")
}

func BenchmarkHTTPPublishLoopback(b *testing.B) {
	server, manifest := newBenchmarkRelay(b, FeatureHTTP)
	delivered := make(chan struct{}, 1)
	client, err := ConnectHTTP(context.Background(), server.URL, manifest, ClientConfig{
		Header: bearerHeader("writer"),
		OnChange: func(replica.Change) error {
			delivered <- struct{}{}
			return nil
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = client.Close() })
	benchmarkPublish(b, manifest, func(change replica.Change) error {
		return client.Publish(context.Background(), change)
	}, delivered)
}

func BenchmarkGRPCClientPublishLoopback(b *testing.B) {
	group, manifest, _ := newGRPCCounterGroup(b)
	server, _, err := NewGRPCServer(GRPCConfig{
		Groups:                []*Group{group},
		Authenticate:          func(context.Context) (Peer, error) { return Peer{ID: "writer"}, nil },
		Authorize:             benchmarkAuthorize,
		AuthorizeSubscription: func(Peer, replica.Manifest) error { return nil },
	})
	if err != nil {
		b.Fatal(err)
	}
	connection := grpcBufconn(b, server)
	delivered := make(chan struct{}, 1)
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("peer", "writer"))
	client, err := OpenGRPC(ctx, NewRelayClient(connection), manifest, GRPCClientConfig{
		OnChange: func(replica.Change) error {
			delivered <- struct{}{}
			return nil
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = client.Close() })
	benchmarkPublish(b, manifest, func(change replica.Change) error {
		return client.Publish(context.Background(), change)
	}, delivered)
}

func benchmarkPublish(b *testing.B, manifest replica.Manifest, publish func(replica.Change) error, delivered <-chan struct{}) {
	b.Helper()
	writer, err := counter.NewGCounter("writer")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := publish(benchmarkCounterChange(b, writer, manifest, uint64(index+1))); err != nil {
			b.Fatal(err)
		}
		<-delivered
	}
	b.StopTimer()
}

func benchmarkCounterChange(b *testing.B, writer *counter.GCounter, manifest replica.Manifest, sequence uint64) replica.Change {
	b.Helper()
	delta, err := writer.Increment(1)
	if err != nil {
		b.Fatal(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		b.Fatal(err)
	}
	change, err := replica.NewChange(manifest, replica.Dot{Actor: "writer", Counter: sequence}, encoded)
	if err != nil {
		b.Fatal(err)
	}
	return change
}

func newBenchmarkRelay(b *testing.B, features Feature) (*httptest.Server, replica.Manifest) {
	b.Helper()
	manifest := testManifest(b, "benchmark-relay")
	relay, err := counter.NewGCounter("relay")
	if err != nil {
		b.Fatal(err)
	}
	group, err := NewGroup(GroupConfig{
		Manifest:          manifest,
		MaxPendingChanges: 128,
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
		b.Fatal(err)
	}
	handler, err := NewHandler(Config{
		Features:          features,
		Groups:            []*Group{group},
		MaxQueuedMessages: 4096,
		MaxQueuedBytes:    8 << 20,
		Authenticate:      benchmarkAuthenticate,
		Authorize:         benchmarkAuthorize,
		AuthorizeSubscription: func(Peer, replica.Manifest) error {
			return nil
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	server := httptest.NewServer(handler)
	b.Cleanup(server.Close)
	return server, manifest
}

func benchmarkAuthenticate(request *http.Request) (Peer, error) {
	const prefix = "Bearer "
	actor := strings.TrimPrefix(request.Header.Get("Authorization"), prefix)
	if actor == "" || actor == request.Header.Get("Authorization") {
		return Peer{}, ErrUnauthorized
	}
	return Peer{ID: actor}, nil
}

func benchmarkAuthorize(peer Peer, _ replica.Manifest, dot replica.Dot) error {
	if peer.ID != dot.Actor {
		return ErrUnauthorized
	}
	return nil
}
