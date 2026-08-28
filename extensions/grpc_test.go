package extensions

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/counter"
	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/replica"
	"github.com/im10furry/crdt/telemetry"
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
	if err != nil || dot != change.Dot || !bytes.Equal(delta, change.Delta()) {
		t.Fatalf("received gRPC envelope dot=%#v delta=%x err=%v", dot, delta, err)
	}
	eventually(t, func() bool { return counterValue(t, relayState) == 7 && group.Frontier().Counter("writer") == 1 })
}

func TestGRPCRelayReportsRealHandshakeAndAppend(t *testing.T) {
	events := make(chan telemetry.Event, 8)
	reporter, err := telemetry.New(telemetry.Options{QueueSize: 8, Sink: func(event telemetry.Event) { events <- event }})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()

	group, manifest, _ := newGRPCCounterGroup(t)
	server, _, err := NewGRPCServer(GRPCConfig{
		Groups:                []*Group{group},
		Authenticate:          func(context.Context) (Peer, error) { return Peer{ID: "writer"}, nil },
		Authorize:             func(Peer, replica.Manifest, replica.Dot) error { return nil },
		AuthorizeSubscription: func(Peer, replica.Manifest) error { return nil },
		Telemetry:             reporter,
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := grpcBufconn(t, server)
	writer := grpcSync(t, connection, manifest, "writer")
	writerState, _ := newCounterInbox(t, manifest, "writer")
	change := incrementChange(t, writerState, manifest, "writer", 1, 1)
	encoded, err := marshalChange(change)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Send(&SyncMessage{Payload: &SyncMessage_Change{Change: encoded}}); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{}{"handshake": {}, "append": {}}
	deadline := time.After(time.Second)
	for len(want) > 0 {
		select {
		case event := <-events:
			if event.Component != "extensions" || event.Outcome != telemetry.OutcomeSuccess {
				t.Fatalf("event = %+v, want successful extensions event", event)
			}
			delete(want, event.Operation)
		case <-deadline:
			t.Fatalf("missing gRPC telemetry operations: %v", want)
		}
	}
}

func TestGRPCRelayReportsRejectedHandshake(t *testing.T) {
	for _, test := range []struct {
		name            string
		authenticate    GRPCAuthenticate
		authorizeReader AuthorizeSubscription
		sendHello       bool
	}{
		{name: "authentication", authenticate: func(context.Context) (Peer, error) { return Peer{}, ErrUnauthorized }, authorizeReader: func(Peer, replica.Manifest) error { return nil }},
		{name: "subscription", authenticate: func(context.Context) (Peer, error) { return Peer{ID: "reader"}, nil }, authorizeReader: func(Peer, replica.Manifest) error { return ErrUnauthorized }, sendHello: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := make(chan telemetry.Event, 1)
			reporter, err := telemetry.New(telemetry.Options{QueueSize: 1, Sink: func(event telemetry.Event) { events <- event }})
			if err != nil {
				t.Fatal(err)
			}
			defer reporter.Close()
			group, manifest, _ := newGRPCCounterGroup(t)
			server, _, err := NewGRPCServer(GRPCConfig{Groups: []*Group{group}, Authenticate: test.authenticate, Authorize: func(Peer, replica.Manifest, replica.Dot) error { return nil }, AuthorizeSubscription: test.authorizeReader, Telemetry: reporter})
			if err != nil {
				t.Fatal(err)
			}
			stream, err := NewRelayClient(grpcBufconn(t, server)).Sync(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if test.sendHello {
				hello, err := marshalHello(manifest)
				if err != nil {
					t.Fatal(err)
				}
				if err := stream.Send(&SyncMessage{Payload: &SyncMessage_Hello{Hello: hello}}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := stream.Recv(); status.Code(err) != codes.Unauthenticated && status.Code(err) != codes.PermissionDenied {
				t.Fatalf("handshake error = %v", err)
			}
			select {
			case event := <-events:
				if event.Operation != "handshake" || event.Outcome != telemetry.OutcomeRejected || event.ErrorCode != crdt.ErrorCodeUnauthorized {
					t.Fatalf("event = %+v", event)
				}
			case <-time.After(time.Second):
				t.Fatal("rejected handshake telemetry was not delivered")
			}
		})
	}
}

func TestGRPCClientReplicatesAndFailsClosed(t *testing.T) {
	group, manifest, _ := newGRPCCounterGroup(t)
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
	observerState, observerInbox := newCounterInbox(t, manifest, "observer")
	observerContext := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("peer", "observer"))
	observer, err := OpenGRPC(observerContext, NewRelayClient(connection), manifest, GRPCClientConfig{
		OnChange: func(change replica.Change) error {
			_, err := observerInbox.Receive(change)
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close() })
	writerContext := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("peer", "writer"))
	writer, err := OpenGRPC(writerContext, NewRelayClient(connection), manifest, GRPCClientConfig{
		OnChange: func(replica.Change) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	writerState, _ := newCounterInbox(t, manifest, "writer")
	change := incrementChange(t, writerState, manifest, "writer", 1, 11)
	if err := writer.Publish(context.Background(), change); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return counterValue(t, observerState) == 11 })
	if err := writer.Publish(context.Background(), replica.Change{}); err == nil {
		t.Fatal("gRPC client published an invalid local change")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Publish(context.Background(), change); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed gRPC client publish error = %v", err)
	}
}

func TestGRPCClientConfigurationAndCallbackFailure(t *testing.T) {
	manifest := testManifest(t, "grpc-client-config")
	if _, err := OpenGRPC(context.Background(), nil, manifest, GRPCClientConfig{OnChange: func(replica.Change) error { return nil }}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil gRPC relay error = %v", err)
	}
	group, liveManifest, _ := newGRPCCounterGroup(t)
	server, _, err := NewGRPCServer(GRPCConfig{
		Groups:                []*Group{group},
		Authenticate:          func(context.Context) (Peer, error) { return Peer{ID: "peer"}, nil },
		Authorize:             func(Peer, replica.Manifest, replica.Dot) error { return nil },
		AuthorizeSubscription: func(Peer, replica.Manifest) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := grpcBufconn(t, server)
	if _, err := OpenGRPC(context.Background(), NewRelayClient(connection), liveManifest, GRPCClientConfig{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil gRPC callback error = %v", err)
	}
	if _, err := OpenGRPC(context.Background(), NewRelayClient(connection), liveManifest, GRPCClientConfig{MaxMessageBytes: 1, OnChange: func(replica.Change) error { return nil }}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid gRPC client limit error = %v", err)
	}
	callbackErr := errors.New("callback failed")
	failed, err := OpenGRPC(context.Background(), NewRelayClient(connection), liveManifest, GRPCClientConfig{
		OnChange: func(replica.Change) error { return callbackErr },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = failed.Close() }()
	publisher, err := OpenGRPC(context.Background(), NewRelayClient(connection), liveManifest, GRPCClientConfig{
		OnChange: func(replica.Change) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = publisher.Close() }()
	publisherState, _ := newCounterInbox(t, liveManifest, "peer")
	change := incrementChange(t, publisherState, liveManifest, "peer", 1, 1)
	if err := publisher.Publish(context.Background(), change); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		select {
		case <-failed.Done():
			return errors.Is(failed.Err(), callbackErr)
		default:
			return false
		}
	})
	var nilClient *GRPCClient
	if nilClient.Done() != nil || !errors.Is(nilClient.Err(), ErrClosed) || !errors.Is(nilClient.Close(), ErrClosed) || !errors.Is(nilClient.Publish(context.Background(), replica.Change{}), ErrClosed) {
		t.Fatal("nil gRPC client methods did not fail closed")
	}
}

func TestGRPCClientRejectsRemoteContractViolations(t *testing.T) {
	manifest := testManifest(t, "grpc-client-contract")
	incompatible, err := replica.NewManifest(manifest.GroupID, "example.com/other/v1", manifest.Epoch, manifest.Protocol, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	incompatibleHello, err := marshalHello(incompatible)
	if err != nil {
		t.Fatal(err)
	}
	for name, first := range map[string]*SyncMessage{
		"change before manifest": {Payload: &SyncMessage_Change{Change: []byte("bad")}},
		"invalid manifest":       {Payload: &SyncMessage_Hello{Hello: []byte("bad")}},
		"incompatible manifest":  {Payload: &SyncMessage_Hello{Hello: incompatibleHello}},
	} {
		t.Run(name, func(t *testing.T) {
			connection := grpcScriptedConnection(t, func(stream grpc.BidiStreamingServer[SyncMessage, SyncMessage]) error {
				if _, err := stream.Recv(); err != nil {
					return err
				}
				return stream.Send(first)
			})
			if _, err := OpenGRPC(context.Background(), NewRelayClient(connection), manifest, GRPCClientConfig{OnChange: func(replica.Change) error { return nil }}); err == nil {
				t.Fatal("invalid remote gRPC contract connected")
			}
		})
	}

	for name, message := range map[string]*SyncMessage{
		"repeated manifest": {Payload: &SyncMessage_Hello{Hello: incompatibleHello}},
		"invalid change":    {Payload: &SyncMessage_Change{Change: []byte("bad")}},
	} {
		t.Run(name, func(t *testing.T) {
			connection := grpcScriptedConnection(t, func(stream grpc.BidiStreamingServer[SyncMessage, SyncMessage]) error {
				if _, err := stream.Recv(); err != nil {
					return err
				}
				hello, err := marshalHello(manifest)
				if err != nil {
					return err
				}
				if err := stream.Send(&SyncMessage{Payload: &SyncMessage_Hello{Hello: hello}}); err != nil {
					return err
				}
				return stream.Send(message)
			})
			client, err := OpenGRPC(context.Background(), NewRelayClient(connection), manifest, GRPCClientConfig{OnChange: func(replica.Change) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			eventually(t, func() bool {
				select {
				case <-client.Done():
					return errors.Is(client.Err(), errInvalidWireMessage)
				default:
					return false
				}
			})
		})
	}
}

func TestGRPCClientPublishHonorsWaitingContext(t *testing.T) {
	group, manifest, _ := newGRPCCounterGroup(t)
	server, _, err := NewGRPCServer(GRPCConfig{
		Groups:                []*Group{group},
		Authenticate:          func(context.Context) (Peer, error) { return Peer{ID: "writer"}, nil },
		Authorize:             benchmarkAuthorize,
		AuthorizeSubscription: func(Peer, replica.Manifest) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := OpenGRPC(context.Background(), NewRelayClient(grpcBufconn(t, server)), manifest, GRPCClientConfig{OnChange: func(replica.Change) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	state, _ := newCounterInbox(t, manifest, "writer")
	change := incrementChange(t, state, manifest, "writer", 1, 1)
	<-client.send
	defer func() { client.send <- struct{}{} }()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Publish(cancelled, change); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled gRPC publish error = %v", err)
	}
}

func TestGRPCClientOpenAndReceiveFailurePaths(t *testing.T) {
	manifest := testManifest(t, "grpc-client-failures")
	if _, err := OpenGRPC(context.Background(), failingRelayClient{err: errors.New("open failed")}, manifest, GRPCClientConfig{OnChange: func(replica.Change) error { return nil }}); err == nil {
		t.Fatal("failed gRPC stream opened")
	}
	if _, err := OpenGRPC(context.Background(), failingRelayClient{stream: failingGRPCStream{sendErr: errors.New("send failed")}}, manifest, GRPCClientConfig{OnChange: func(replica.Change) error { return nil }}); err == nil {
		t.Fatal("failed gRPC hello send succeeded")
	}
	large, err := replica.NewManifest(strings.Repeat("g", 1024), manifest.SchemaID, manifest.Epoch, manifest.Protocol, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenGRPC(context.Background(), failingRelayClient{}, large, GRPCClientConfig{MaxMessageBytes: 1024, OnChange: func(replica.Change) error { return nil }}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("oversized gRPC hello error = %v", err)
	}

	connection := grpcScriptedConnection(t, func(stream grpc.BidiStreamingServer[SyncMessage, SyncMessage]) error {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		hello, err := marshalHello(manifest)
		if err != nil {
			return err
		}
		if err := stream.Send(&SyncMessage{Payload: &SyncMessage_Hello{Hello: hello}}); err != nil {
			return err
		}
		return status.Error(codes.Unavailable, "scripted receive failure")
	})
	client, err := OpenGRPC(context.Background(), NewRelayClient(connection), manifest, GRPCClientConfig{OnChange: func(replica.Change) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	eventually(t, func() bool {
		select {
		case <-client.Done():
			return status.Code(client.Err()) == codes.Unavailable
		default:
			return false
		}
	})
}

func TestGRPCClientStopsOnPublishFailureAndEnforcesActorLimit(t *testing.T) {
	manifest := testManifest(t, "grpc-client-publish-failure")
	hello, err := marshalHello(manifest)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	stream := &publishFailGRPCStream{hello: hello, release: release}
	client, err := OpenGRPC(context.Background(), failingRelayClient{stream: stream}, manifest, GRPCClientConfig{OnChange: func(replica.Change) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := newCounterInbox(t, manifest, "writer")
	change := incrementChange(t, state, manifest, "writer", 1, 1)
	if err := client.Publish(context.Background(), change); err == nil {
		t.Fatal("gRPC publish send failure succeeded")
	}
	close(release)
	eventually(t, func() bool {
		select {
		case <-client.Done():
			return client.Err() != nil
		default:
			return false
		}
	})

	releaseLimited := make(chan struct{})
	limited, err := OpenGRPC(context.Background(), failingRelayClient{stream: &publishFailGRPCStream{hello: hello, release: releaseLimited}}, manifest, GRPCClientConfig{
		MaxActorBytes: 1,
		OnChange:      func(replica.Change) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limited.Close() }()
	defer close(releaseLimited)
	if err := limited.Publish(context.Background(), change); !errors.Is(err, errInvalidWireMessage) {
		t.Fatalf("oversized gRPC actor error = %v", err)
	}
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
	if status.Code((*GRPCRelay)(nil).Sync(nil)) != codes.Unavailable {
		t.Fatal("nil gRPC relay did not report unavailable")
	}
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
	if grpcReceiveStatus(nil) != nil || grpcReceiveStatus(io.EOF) != nil || status.Code(grpcReceiveStatus(errors.New("read"))) != codes.Unavailable {
		t.Fatal("receive status mapping mismatch")
	}
	if grpcSendStatus(nil) != nil || status.Code(grpcSendStatus(errors.New("write"))) != codes.Unknown {
		t.Fatal("send status mapping mismatch")
	}
	if status.Code(grpcReceiveStatus(status.Error(codes.InvalidArgument, "status"))) != codes.InvalidArgument || status.Code(grpcSendStatus(status.Error(codes.InvalidArgument, "status"))) != codes.InvalidArgument {
		t.Fatal("status preserving mapping mismatch")
	}
	var nilSubscriber *grpcSubscriber
	if nilSubscriber.enqueue(nil) || nilSubscriber.enqueueAll(nil) {
		t.Fatal("nil gRPC subscriber accepted data")
	}
	if _, ok := nilSubscriber.dequeueContext(context.Background()); ok {
		t.Fatal("nil gRPC subscriber dequeued data")
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
	connection, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

type scriptedGRPCRelay struct {
	UnimplementedRelayServer
	sync func(grpc.BidiStreamingServer[SyncMessage, SyncMessage]) error
}

func (relay scriptedGRPCRelay) Sync(stream grpc.BidiStreamingServer[SyncMessage, SyncMessage]) error {
	return relay.sync(stream)
}

type failingRelayClient struct {
	stream grpc.BidiStreamingClient[SyncMessage, SyncMessage]
	err    error
}

func (client failingRelayClient) Sync(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[SyncMessage, SyncMessage], error) {
	return client.stream, client.err
}

type failingGRPCStream struct {
	grpc.ClientStream
	sendErr error
}

func (stream failingGRPCStream) Send(*SyncMessage) error { return stream.sendErr }

func (failingGRPCStream) Recv() (*SyncMessage, error) { return nil, errors.New("receive failed") }

type publishFailGRPCStream struct {
	grpc.ClientStream
	mu       sync.Mutex
	hello    []byte
	received bool
	sends    int
	release  <-chan struct{}
}

func (stream *publishFailGRPCStream) Send(*SyncMessage) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	stream.sends++
	if stream.sends == 1 {
		return nil
	}
	return errors.New("publish send failed")
}

func (stream *publishFailGRPCStream) Recv() (*SyncMessage, error) {
	stream.mu.Lock()
	if !stream.received {
		stream.received = true
		stream.mu.Unlock()
		return &SyncMessage{Payload: &SyncMessage_Hello{Hello: stream.hello}}, nil
	}
	stream.mu.Unlock()
	<-stream.release
	return nil, io.EOF
}

func grpcScriptedConnection(t testing.TB, script func(grpc.BidiStreamingServer[SyncMessage, SyncMessage]) error) *grpc.ClientConn {
	t.Helper()
	server := grpc.NewServer()
	RegisterRelayServer(server, scriptedGRPCRelay{sync: script})
	return grpcBufconn(t, server)
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
	return status.Code(err).String()
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

// BenchmarkGRPCDeltaEquality isolates the payload check used after a real
// loopback relay receive. Both slices contain the same 5 KiB delta payload.
func BenchmarkGRPCDeltaEquality(b *testing.B) {
	payload := bytes.Repeat([]byte("crdt-delta"), 512)
	want := append([]byte(nil), payload...)
	b.Run("bytes_equal", func(b *testing.B) {
		b.ReportAllocs()
		for index := 0; index < b.N; index++ {
			if !bytes.Equal(payload, want) {
				b.Fatal("payload mismatch")
			}
		}
	})
	b.Run("string_conversion", func(b *testing.B) {
		b.ReportAllocs()
		for index := 0; index < b.N; index++ {
			if string(payload) != string(want) {
				b.Fatal("payload mismatch")
			}
		}
	})
}
