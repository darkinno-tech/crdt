package durable

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/counter"
	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/replica"
	"github.com/coder/websocket"
)

// BenchmarkDurableAppend measures one fully committed local operation-log
// append. It includes the bbolt transaction and sync boundary, not network or
// CRDT concrete-decoder cost.
func BenchmarkDurableAppend(b *testing.B) {
	manifest := durableBenchmarkManifest(b)
	store, err := OpenStore(b.TempDir()+"/relay.db", StoreConfig{MaxEvents: uint64(b.N) + 1, MaxBytes: uint64(b.N+1) * 4096})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	state, err := counter.NewGCounter("writer")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		delta, err := state.Increment(1)
		if err != nil {
			b.Fatal(err)
		}
		change, err := durableCounterChange(manifest, "writer", uint64(index+1), delta)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := store.Append(manifest.GroupID, change); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDurableAppendPrepared isolates Store.Append from concrete CRDT
// mutation and change construction while retaining the committed bbolt write.
func BenchmarkDurableAppendPrepared(b *testing.B) {
	manifest := durableBenchmarkManifest(b)
	store, err := OpenStore(b.TempDir()+"/relay.db", StoreConfig{MaxEvents: uint64(b.N) + 1, MaxBytes: uint64(b.N+1) * 4096})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	state, err := counter.NewGCounter("writer")
	if err != nil {
		b.Fatal(err)
	}
	changes := make([]replica.Change, b.N)
	for index := range changes {
		delta, err := state.Increment(1)
		if err != nil {
			b.Fatal(err)
		}
		changes[index], err = durableCounterChange(manifest, "writer", uint64(index+1), delta)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for _, change := range changes {
		if _, err := store.Append(manifest.GroupID, change); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDurableReplay measures a complete bounded replay of a 256-operation
// suffix from a local durable store. It is not a WAN, TLS, or consumer-apply
// benchmark.
func BenchmarkDurableReplay(b *testing.B) {
	manifest := durableBenchmarkManifest(b)
	store, err := OpenStore(b.TempDir()+"/relay.db", StoreConfig{MaxEvents: 512, MaxBytes: 2 << 20})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	state, err := counter.NewGCounter("writer")
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 256; index++ {
		delta, err := state.Increment(1)
		if err != nil {
			b.Fatal(err)
		}
		change, err := durableCounterChange(manifest, "writer", uint64(index+1), delta)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := store.Append(manifest.GroupID, change); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		events, highWater, err := store.Replay(manifest.GroupID, 0, 512, 2<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
		if err != nil || highWater != 256 || len(events) != 256 {
			b.Fatalf("replay events=%d high=%d err=%v", len(events), highWater, err)
		}
	}
}

// BenchmarkReconnectHandshakeLoopback measures a real local WebSocket durable
// hello/resume handshake against a mounted handler. It deliberately resumes at
// high-water so it measures reconnect setup rather than replay transfer.
func BenchmarkReconnectHandshakeLoopback(b *testing.B) {
	manifest := durableBenchmarkManifest(b)
	store, err := OpenStore(b.TempDir()+"/relay.db", StoreConfig{MaxEvents: 4, MaxBytes: 1 << 20})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	handler, _ := durableBenchmarkHandler(b, store, manifest)
	server := httptest.NewServer(handler)
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		context, cancel := context.WithTimeout(context.Background(), time.Second)
		connection, _, err := websocket.Dial(context, endpoint, &websocket.DialOptions{
			HTTPHeader:   http.Header{"Authorization": []string{"Bearer writer"}},
			Subprotocols: []string{Subprotocol},
		})
		if err != nil {
			cancel()
			b.Fatal(err)
		}
		hello, err := marshalHello(manifest, 0)
		if err != nil {
			b.Fatal(err)
		}
		if err := connection.Write(context, websocket.MessageText, hello); err != nil {
			b.Fatal(err)
		}
		messageType, data, err := connection.Read(context)
		if err != nil || messageType != websocket.MessageText {
			b.Fatalf("read welcome type=%v data=%q err=%v", messageType, data, err)
		}
		if _, _, err := unmarshalWelcome(data); err != nil {
			b.Fatal(err)
		}
		_ = connection.CloseNow()
		cancel()
	}
}

func durableBenchmarkManifest(b *testing.B) replica.Manifest {
	b.Helper()
	manifest, err := replica.NewManifest("durable-benchmark", "example.com/durable-benchmark/v1", 1, replica.Protocol{
		StateID:          crdt.TypeIDGCounterState,
		DeltaID:          crdt.TypeIDGCounterDelta,
		SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		b.Fatal(err)
	}
	return manifest
}

func durableBenchmarkHandler(b *testing.B, store *Store, manifest replica.Manifest) (*Handler, *Group) {
	b.Helper()
	group, err := NewGroup(GroupConfig{
		Manifest: manifest,
		Validate: func(data []byte) error {
			_, err := counter.UnmarshalGCounterDeltaWithLimits(data, frame.DefaultLimits())
			return err
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	handler, err := NewHandler(Config{
		Store:  store,
		Groups: []*Group{group},
		Authenticate: func(request *http.Request) (Peer, error) {
			return Peer{ID: strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")}, nil
		},
		Authorize:             func(Peer, replica.Manifest, replica.Dot) error { return nil },
		AuthorizeSubscription: func(Peer, replica.Manifest) error { return nil },
	})
	if err != nil {
		b.Fatal(err)
	}
	return handler, group
}
