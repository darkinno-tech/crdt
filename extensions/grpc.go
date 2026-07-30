package extensions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GRPCAuthenticate authenticates a gRPC stream before its manifest or change
// payload is read. It should derive the identity from transport credentials or
// trusted metadata, never from the CRDT actor supplied in a change.
type GRPCAuthenticate func(context.Context) (Peer, error)

// GRPCConfig configures the manifest-bound gRPC Relay service. It deliberately
// mirrors the live-relay security and capacity boundary without turning gRPC
// flow control into an unbounded application queue.
type GRPCConfig struct {
	Groups                []*Group
	Authenticate          GRPCAuthenticate
	Authorize             Authorize
	AuthorizeSubscription AuthorizeSubscription
	MaxMessageBytes       int
	MaxActorBytes         int
	MaxQueuedMessages     int
	MaxQueuedBytes        int
}

// GRPCRelay implements Relay over one bidirectional gRPC stream per live
// subscription. The first client message and first server response are exact
// encoded replica manifests. Later messages carry the existing canonical
// change envelope, so gRPC introduces no second CRDT frame format.
//
// A gRPC stream is live-only. It has no replay cursor, operation log,
// persistent outbox, or automatic reconnection. Applications must persist
// state/frontier and recover missed changes independently.
type GRPCRelay struct {
	UnimplementedRelayServer

	groups                map[string]*Group
	authenticate          GRPCAuthenticate
	authorize             Authorize
	authorizeSubscription AuthorizeSubscription
	maxMessageBytes       int
	maxActorBytes         int
	maxQueuedMessages     int
	maxQueuedBytes        int
}

// NewGRPCRelay validates and constructs a disabled-by-default gRPC transport
// surface. Authentication plus independent read/write authorization are
// mandatory because TLS authenticates a channel but does not decide a tenant's
// CRDT group permissions.
func NewGRPCRelay(config GRPCConfig) (*GRPCRelay, error) {
	if config.Authenticate == nil || config.Authorize == nil || config.AuthorizeSubscription == nil || len(config.Groups) == 0 {
		return nil, ErrInvalidConfig
	}
	limits, err := normalizeLimits(config.MaxMessageBytes, config.MaxActorBytes, config.MaxQueuedMessages, config.MaxQueuedBytes, 1, 1)
	if err != nil {
		return nil, err
	}
	groups := make(map[string]*Group, len(config.Groups))
	for _, group := range config.Groups {
		if group == nil || strings.TrimSpace(group.manifest.GroupID) == "" {
			return nil, ErrInvalidConfig
		}
		if _, exists := groups[group.manifest.GroupID]; exists {
			return nil, ErrInvalidConfig
		}
		hello, err := marshalHello(group.manifest)
		if err != nil || len(hello) > controlLimit(limits.maxMessageBytes) {
			return nil, ErrInvalidConfig
		}
		groups[group.manifest.GroupID] = group
	}
	return &GRPCRelay{
		groups:                groups,
		authenticate:          config.Authenticate,
		authorize:             config.Authorize,
		authorizeSubscription: config.AuthorizeSubscription,
		maxMessageBytes:       limits.maxMessageBytes,
		maxActorBytes:         limits.maxActorBytes,
		maxQueuedMessages:     limits.maxQueuedMessages,
		maxQueuedBytes:        limits.maxQueuedBytes,
	}, nil
}

// ServerOptions returns the message-size boundary required by the Relay
// protocol. Supply these options when mounting Relay in an application-owned
// grpc.Server; NewGRPCServer does so automatically.
func (relay *GRPCRelay) ServerOptions() []grpc.ServerOption {
	if relay == nil {
		return nil
	}
	// Protobuf's oneof envelope is small, but reserving 1 KiB prevents a
	// transport-level rejection from silently becoming a different limit than
	// the explicit payload check below.
	limit := relay.maxMessageBytes + 1024
	return []grpc.ServerOption{grpc.MaxRecvMsgSize(limit), grpc.MaxSendMsgSize(limit)}
}

// NewGRPCServer constructs and registers an application-ready gRPC server.
// Hosts that share a grpc.Server may instead call NewGRPCRelay, install its
// ServerOptions during server construction, then RegisterRelayServer.
func NewGRPCServer(config GRPCConfig) (*grpc.Server, *GRPCRelay, error) {
	relay, err := NewGRPCRelay(config)
	if err != nil {
		return nil, nil, err
	}
	server := grpc.NewServer(relay.ServerOptions()...)
	RegisterRelayServer(server, relay)
	return server, relay, nil
}

// Sync accepts one manifest-bound live stream. gRPC's transport flow control
// applies underneath, while the per-peer queue remains the bounded relay
// boundary: a slow stream is disconnected rather than retaining arbitrary
// application state in memory.
func (relay *GRPCRelay) Sync(stream grpc.BidiStreamingServer[SyncMessage, SyncMessage]) error {
	if relay == nil {
		return status.Error(codes.Unavailable, "CRDT relay unavailable")
	}
	peer, err := relay.authenticate(stream.Context())
	if err != nil || strings.TrimSpace(peer.ID) == "" {
		return status.Error(codes.Unauthenticated, "unauthorized")
	}
	first, err := stream.Recv()
	if err != nil {
		return grpcReceiveStatus(err)
	}
	hello := first.GetHello()
	if len(hello) == 0 || first.GetChange() != nil {
		return status.Error(codes.InvalidArgument, "first gRPC relay message must be a manifest")
	}
	remote, err := unmarshalHello(hello)
	if err != nil {
		return status.Error(codes.InvalidArgument, "invalid manifest")
	}
	group, exists := relay.groups[remote.GroupID]
	if !exists || group.manifest.Compatible(remote) != nil {
		return status.Error(codes.PermissionDenied, "incompatible manifest")
	}
	if err := relay.authorizeSubscription(peer, group.manifest); err != nil {
		return status.Error(codes.PermissionDenied, "unauthorized")
	}
	response, err := marshalHello(group.manifest)
	if err != nil {
		return status.Error(codes.Internal, "invalid relay manifest")
	}
	subscriber := newGRPCSubscriber(relay.maxQueuedMessages, relay.maxQueuedBytes)
	group.add(subscriber)
	defer group.remove(subscriber)
	defer subscriber.close()
	// Registration precedes the confirmation. As with the WebSocket protocol,
	// a returned client handshake is therefore the live-subscription
	// linearization point.
	if err := stream.Send(&SyncMessage{Payload: &SyncMessage_Hello{Hello: response}}); err != nil {
		return grpcSendStatus(err)
	}

	receiveResult := make(chan error, 1)
	go func() {
		receiveResult <- relay.receiveGRPCChanges(stream, peer, group, subscriber)
	}()
	for {
		data, ok := subscriber.dequeueContext(stream.Context())
		if !ok {
			return <-receiveResult
		}
		if err := stream.Send(&SyncMessage{Payload: &SyncMessage_Change{Change: data}}); err != nil {
			subscriber.close()
			return grpcSendStatus(err)
		}
	}
}

func (relay *GRPCRelay) receiveGRPCChanges(stream grpc.BidiStreamingServer[SyncMessage, SyncMessage], peer Peer, group *Group, subscriber *grpcSubscriber) error {
	defer subscriber.close()
	for {
		message, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			return grpcReceiveStatus(err)
		}
		change := message.GetChange()
		if len(change) == 0 || message.GetHello() != nil {
			return status.Error(codes.InvalidArgument, "expected CRDT change")
		}
		if _, err := group.receive(peer, relay.authorize, change, relay.maxMessageBytes, relay.maxActorBytes); err != nil {
			if errors.Is(err, ErrUnauthorized) {
				return status.Error(codes.PermissionDenied, "unauthorized")
			}
			return status.Error(codes.InvalidArgument, "invalid change")
		}
	}
}

type grpcSubscriber struct{ queue peerQueue }

func newGRPCSubscriber(maxMessages, maxBytes int) *grpcSubscriber {
	return &grpcSubscriber{queue: newPeerQueue(maxMessages, maxBytes)}
}

func (subscriber *grpcSubscriber) enqueue(data []byte) bool {
	return subscriber != nil && subscriber.queue.enqueue(data)
}

func (subscriber *grpcSubscriber) enqueueAll(data [][]byte) bool {
	return subscriber != nil && subscriber.queue.enqueueAll(data)
}

func (subscriber *grpcSubscriber) dequeueContext(ctx context.Context) ([]byte, bool) {
	if subscriber == nil {
		return nil, false
	}
	return subscriber.queue.dequeueContext(ctx)
}

func (subscriber *grpcSubscriber) close() {
	if subscriber != nil {
		subscriber.queue.close()
	}
}

func grpcReceiveStatus(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	return status.Error(codes.Unavailable, "gRPC relay receive failed")
}

func grpcSendStatus(err error) error {
	if err == nil {
		return nil
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	return fmt.Errorf("gRPC relay send: %w", err)
}
