package extensions

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/counter"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/replica"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestGRPCRelayReplicatesManifestBoundChange(t *testing.T) {
	group, manifest, relayState := newGRPCCounterGroup(t)
	server, _, err := NewGRPCServer(GRPCConfig{
		Groups: []*Group{group},
		Authenticate: func(ctx context.Context) (Peer, error) {
			values := metadata.ValueFromIncomingContext(ctx, "peer")
			if len(values) != 1 {
				return Peer{}, ErrUnauthorized
			}
			return Peer{ID: values[0]}, nil
		},
		Authorize: func(peer Peer, _ replica.Manifest, dot replica.Dot) error {
			if peer.ID != dot.Actor {
				return ErrUnauthorized
			}
			return nil
		},
		AuthorizeSubscription: func(Peer, replica.Manifest) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := grpcBufconn(t, server)
	observer := grpcSync(t, connection, manifest, "observer")
	writer := grpcSync(t, connection, manifest, "writer")
	writerState, _ := newCounterInbox(t, manifest, "writer")
	change := incrementChange(t, writerState, manifest, "writer", 1, 7)
	encoded, err := marshalChange(change)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Send(&SyncMessage{Payload: &SyncMessage_Change{Change: encoded}}); err != nil {
		t.Fatal(err)
	}
	message, err := observer.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if message.GetHello() != nil || len(message.GetChange()) == 0 {
		t.Fatalf("observer message = %#v", message)
	}
	dot, delta, err := unmarshalChange(message.GetChange(), defaultMaxMessageBytes, defaultMaxActorBytes)
	if err != nil || dot != change.Dot || string(delta) != string(change.Delta()) {
		t.Fatalf("received gRPC envelope dot=%#v delta=%x err=%v", dot, delta, err)
	}
	eventually(t, func() bool { return counterValue(t, relayState) == 7 && group.Frontier().Counter("writer") == 1 })
}

func TestGRPCRelayRejectsInvalidFirstMessageAndForgedActor(t *testing.T) {
	group, manifest, relayState := newGRPCCounterGroup(t)
	server, _, err := NewGRPCServer(GRPCConfig{
		Groups:       []*Group{group},
		Authenticate: func(context.Context) (Peer, error) { return Peer{ID: "alice"}, nil },
		Authorize: func(peer Peer, _ replica.Manifest, dot replica.Dot) error {
			if peer.ID != dot.Actor {
				return ErrUnauthorized
			}
			return nil
		},
		AuthorizeSubscription: func(Peer, replica.Manifest) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := grpcBufconn(t, server)
	invalid, err := NewRelayClient(connection).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := invalid.Send(&SyncMessage{Payload: &SyncMessage_Change{Change: []byte("bad")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := invalid.Recv(); grpcCode(err) != "InvalidArgument" {
		t.Fatalf("invalid first message code = %v, want InvalidArgument", err)
	}

	forged := grpcSync(t, connection, manifest, "alice")
	writer, err := counter.NewGCounter("mallory")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := writer.Increment(1)
	if err != nil {
		t.Fatal(err)
	}
	change := newCounterChange(t, manifest, "mallory", 1, delta)
	encoded, err := marshalChange(change)
	if err != nil {
		t.Fatal(err)
	}
	if err := forged.Send(&SyncMessage{Payload: &SyncMessage_Change{Change: encoded}}); err != nil {
		t.Fatal(err)
	}
	if _, err := forged.Recv(); grpcCode(err) != "PermissionDenied" {
		t.Fatalf("forged actor code = %v, want PermissionDenied", err)
	}
	if got := counterValue(t, relayState); got != 0 {
		t.Fatalf("forged change mutated relay = %d", got)
	}
}

func TestGRPCRelayConfigurationAndQueueBoundaries(t *testing.T) {
	if _, err := NewGRPCRelay(GRPCConfig{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty gRPC config = %v", err)
	}
	group, _, _ := newGRPCCounterGroup(t)
	base := GRPCConfig{
		Groups:                []*Group{group},
		Authenticate:          func(context.Context) (Peer, error) { return Peer{ID: "peer"}, nil },
		Authorize:             func(Peer, replica.Manifest, replica.Dot) error { return nil },
		AuthorizeSubscription: func(Peer, replica.Manifest) error { return nil },
	}
	invalid := base
	invalid.Groups = []*Group{nil}
	if _, err := NewGRPCRelay(invalid); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil group config = %v", err)
	}
	invalid = base
	invalid.Groups = []*Group{group, group}
	if _, err := NewGRPCRelay(invalid); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("duplicate group config = %v", err)
	}
	invalid = base
	invalid.MaxMessageBytes = 1
	if _, err := NewGRPCRelay(invalid); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid gRPC limit config = %v", err)
	}
	relay, err := NewGRPCRelay(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(relay.ServerOptions()) != 2 || (*GRPCRelay)(nil).ServerOptions() != nil {
		t.Fatal("unexpected gRPC server options")
	}
	server, registered, err := NewGRPCServer(base)
	if err != nil || registered == nil || server == nil {
		t.Fatalf("NewGRPCServer server=%v relay=%v err=%v", server, registered, err)
	}
	connection := grpcBufconn(t, server)
	unknown, err := NewRelayClient(connection).Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	unknownHello, err := marshalHello(testManifest(t, "unknown-grpc-group"))
	if err != nil {
		t.Fatal(err)
	}
	if err := unknown.Send(&SyncMessage{Payload: &SyncMessage_Hello{Hello: unknownHello}}); err != nil {
		t.Fatal(err)
	}
	if _, err := unknown.Recv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unknown manifest error = %v", err)
	}

	subscriber := newGRPCSubscriber(2, 32)
	if !subscriber.enqueueAll([][]byte{[]byte("one"), []byte("two")}) {
		t.Fatal("bounded gRPC queue rejected fitting batch")
	}
	if subscriber.enqueue([]byte("three")) {
		t.Fatal("full gRPC queue accepted item")
	}
	if data, ok := subscriber.dequeueContext(context.Background()); !ok || string(data) != "one" {
		t.Fatalf("queue first=%q ok=%v", data, ok)
	}
	subscriber.close()
	if _, ok := subscriber.dequeueContext(context.Background()); ok {
		t.Fatal("closed gRPC queue dequeued data")
	}
	if grpcReceiveStatus(io.EOF) != nil || status.Code(grpcReceiveStatus(errors.New("read"))) != codes.Unavailable {
		t.Fatal("receive status mapping mismatch")
	}
	if status.Code(grpcSendStatus(errors.New("write"))) != codes.Unknown {
		t.Fatal("send status mapping mismatch")
	}
	if status.Code(grpcReceiveStatus(status.Error(codes.InvalidArgument, "status"))) != codes.InvalidArgument || status.Code(grpcSendStatus(status.Error(codes.InvalidArgument, "status"))) != codes.InvalidArgument {
		t.Fatal("status preserving mapping mismatch")
	}
	var nilSubscriber *grpcSubscriber
	if nilSubscriber.enqueue(nil) || nilSubscriber.enqueueAll(nil) {
		t.Fatal("nil gRPC subscriber accepted data")
	}
	nilSubscriber.close()
}

func newGRPCCounterGroup(t testing.TB) (*Group, replica.Manifest, *counter.GCounter) {
	t.Helper()
	manifest, err := replica.NewManifest("grpc-counter", "example.com/counter/v1", 1, replica.Protocol{StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	state, err := counter.NewGCounter("relay")
	if err != nil {
		t.Fatal(err)
	}
	group, err := NewGroup(GroupConfig{Manifest: manifest, MaxPendingChanges: 8, MaxPendingBytes: 16 << 10, Apply: func(data []byte) error {
		delta, err := counter.UnmarshalGCounterDeltaWithLimits(data, frame.DefaultLimits())
		if err != nil {
			return err
		}
		return state.ApplyDelta(delta)
	}})
	if err != nil {
		t.Fatal(err)
	}
	return group, manifest, state
}

func grpcBufconn(t testing.TB, server *grpc.Server) *grpc.ClientConn {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	connection, err := grpc.DialContext(ctx, "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

func grpcSync(t testing.TB, connection *grpc.ClientConn, manifest replica.Manifest, peer string) grpc.BidiStreamingClient[SyncMessage, SyncMessage] {
	t.Helper()
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("peer", peer))
	stream, err := NewRelayClient(connection).Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hello, err := marshalHello(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&SyncMessage{Payload: &SyncMessage_Hello{Hello: hello}}); err != nil {
		t.Fatal(err)
	}
	response, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	remote, err := unmarshalHello(response.GetHello())
	if err != nil || manifest.Compatible(remote) != nil {
		t.Fatalf("gRPC handshake response=%#v err=%v", response, err)
	}
	return stream
}

func grpcCode(err error) string {
	if err == nil {
		return ""
	}
	return grpc.Code(err).String()
}

func BenchmarkGRPCRelayLoopback(b *testing.B) {
	group, manifest, _ := newGRPCCounterGroup(b)
	server, _, err := NewGRPCServer(GRPCConfig{
		Groups:       []*Group{group},
		Authenticate: func(context.Context) (Peer, error) { return Peer{ID: "writer"}, nil },
		Authorize: func(peer Peer, _ replica.Manifest, dot replica.Dot) error {
			if peer.ID != dot.Actor {
				return ErrUnauthorized
			}
			return nil
		},
		AuthorizeSubscription: func(Peer, replica.Manifest) error { return nil },
	})
	if err != nil {
		b.Fatal(err)
	}
	connection := grpcBufconn(b, server)
	observer := grpcSync(b, connection, manifest, "observer")
	writer := grpcSync(b, connection, manifest, "writer")
	writerState, _ := newCounterInbox(b, manifest, "writer")
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		change := incrementChange(b, writerState, manifest, "writer", uint64(index+1), 1)
		encoded, err := marshalChange(change)
		if err != nil {
			b.Fatal(err)
		}
		if err := writer.Send(&SyncMessage{Payload: &SyncMessage_Change{Change: encoded}}); err != nil {
			b.Fatal(err)
		}
		message, err := observer.Recv()
		if err != nil || len(message.GetChange()) == 0 {
			b.Fatalf("observe loopback message=%#v err=%v", message, err)
		}
	}
}
