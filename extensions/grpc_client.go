package extensions

import (
	"context"
	"fmt"
	"sync"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/replica"
	"google.golang.org/grpc"
)

// GRPCClientConfig configures a managed, manifest-bound Relay.Sync client.
// OnChange must hand every received change to an application-owned,
// manifest-compatible replica.Inbox or an equivalently durable boundary.
//
// The context passed to OpenGRPC owns the lifetime of the live stream. It must
// have a realistic stream deadline (or be cancelled during shutdown); do not
// use a short handshake-only deadline because gRPC applies it to the entire
// RPC. Publish checks its context before waiting to send, while a blocked gRPC
// Send is interrupted by the stream context.
type GRPCClientConfig struct {
	Policy          crdt.ProtocolPolicy
	MaxMessageBytes int
	MaxActorBytes   int
	OnChange        func(replica.Change) error
}

// GRPCClient maintains one manifest-bound gRPC live subscription. It neither
// owns the ClientConn nor persists an outbox, and it does not reconnect
// automatically. The application owns credentials, TLS, connection reuse,
// durable recovery, and reconnect policy.
type GRPCClient struct {
	manifest replica.Manifest
	policy   crdt.ProtocolPolicy
	onChange func(replica.Change) error

	maxMessageBytes int
	maxActorBytes   int
	stream          grpc.BidiStreamingClient[SyncMessage, SyncMessage]
	context         context.Context
	cancel          context.CancelFunc
	done            chan struct{}
	send            chan struct{}
	closeOnce       sync.Once
	errMu           sync.RWMutex
	err             error
}

// OpenGRPC opens Relay.Sync, sends the local exact manifest, and verifies the
// exact manifest returned by the relay before it starts receiving changes. A
// successful return means the relay registered the live subscription before
// its manifest confirmation. relay is normally NewRelayClient(connection).
func OpenGRPC(ctx context.Context, relay RelayClient, manifest replica.Manifest, config GRPCClientConfig) (*GRPCClient, error) {
	if relay == nil || config.OnChange == nil {
		return nil, ErrInvalidConfig
	}
	limits, err := normalizeGRPCClientLimits(config)
	if err != nil {
		return nil, err
	}
	if _, err := replica.NewSessionWithPolicy(manifest, config.Policy); err != nil {
		return nil, fmt.Errorf("validate manifest: %w", err)
	}
	hello, err := marshalHello(manifest)
	if err != nil {
		return nil, err
	}
	if len(hello) > controlLimit(limits.maxMessageBytes) {
		return nil, ErrInvalidConfig
	}
	streamContext, cancel := context.WithCancel(ctx)
	messageLimit := limits.maxMessageBytes + 1024
	stream, err := relay.Sync(streamContext, grpc.MaxCallRecvMsgSize(messageLimit), grpc.MaxCallSendMsgSize(messageLimit))
	if err != nil {
		cancel()
		return nil, err
	}
	if err := stream.Send(&SyncMessage{Payload: &SyncMessage_Hello{Hello: hello}}); err != nil {
		cancel()
		return nil, err
	}
	response, err := stream.Recv()
	if err != nil {
		cancel()
		return nil, err
	}
	if response == nil || len(response.GetHello()) == 0 || response.GetChange() != nil {
		cancel()
		return nil, errInvalidWireMessage
	}
	remote, err := unmarshalHello(response.GetHello())
	if err != nil {
		cancel()
		return nil, err
	}
	if err := manifest.Compatible(remote); err != nil {
		cancel()
		return nil, err
	}
	client := &GRPCClient{
		manifest:        manifest,
		policy:          config.Policy,
		onChange:        config.OnChange,
		maxMessageBytes: limits.maxMessageBytes,
		maxActorBytes:   limits.maxActorBytes,
		stream:          stream,
		context:         streamContext,
		cancel:          cancel,
		done:            make(chan struct{}),
		send:            make(chan struct{}, 1),
	}
	client.send <- struct{}{}
	go client.readLoop()
	return client, nil
}

// Publish validates a change against the negotiated manifest and sends its
// canonical envelope. A successful return means only that gRPC accepted the
// message for transport; callers retain durable outbox and retry decisions.
func (client *GRPCClient) Publish(ctx context.Context, change replica.Change) error {
	if client == nil {
		return ErrClosed
	}
	verified, err := replica.NewChangeWithPolicy(client.manifest, change.Dot, change.Delta(), client.policy)
	if err != nil {
		return fmt.Errorf("validate change: %w", err)
	}
	encoded, err := marshalChange(verified)
	if err != nil {
		return err
	}
	if len(verified.Dot.Actor) > client.maxActorBytes || len(encoded) > client.maxMessageBytes {
		return errInvalidWireMessage
	}
	select {
	case <-client.context.Done():
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	case <-client.send:
	}
	defer func() { client.send <- struct{}{} }()
	select {
	case <-client.context.Done():
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if err := client.stream.Send(&SyncMessage{Payload: &SyncMessage_Change{Change: encoded}}); err != nil {
		client.stop(err)
		return err
	}
	return nil
}

// Done closes when the receive loop ends.
func (client *GRPCClient) Done() <-chan struct{} {
	if client == nil {
		return nil
	}
	return client.done
}

// Err returns the first non-local stream or callback failure.
func (client *GRPCClient) Err() error {
	if client == nil {
		return ErrClosed
	}
	client.errMu.RLock()
	defer client.errMu.RUnlock()
	return client.err
}

// Close cancels the live stream but does not close the application-owned gRPC
// ClientConn or claim durable delivery.
func (client *GRPCClient) Close() error {
	if client == nil {
		return ErrClosed
	}
	client.stop(nil)
	<-client.done
	return nil
}

func (client *GRPCClient) readLoop() {
	defer close(client.done)
	for {
		message, err := client.stream.Recv()
		if err != nil {
			select {
			case <-client.context.Done():
				return
			default:
				client.stop(err)
				return
			}
		}
		if message == nil || len(message.GetChange()) == 0 || message.GetHello() != nil {
			client.stop(errInvalidWireMessage)
			return
		}
		dot, delta, err := unmarshalChange(message.GetChange(), client.maxMessageBytes, client.maxActorBytes)
		if err != nil {
			client.stop(err)
			return
		}
		change, err := replica.NewChangeWithPolicy(client.manifest, dot, delta, client.policy)
		if err != nil {
			client.stop(fmt.Errorf("validate received change: %w", err))
			return
		}
		if err := client.onChange(change); err != nil {
			client.stop(fmt.Errorf("apply received change: %w", err))
			return
		}
	}
}

func (client *GRPCClient) stop(err error) {
	if err != nil && client != nil {
		client.errMu.Lock()
		if client.err == nil {
			client.err = err
		}
		client.errMu.Unlock()
	}
	if client != nil {
		client.closeOnce.Do(client.cancel)
	}
}

func normalizeGRPCClientLimits(config GRPCClientConfig) (transportLimits, error) {
	return normalizeLimits(
		config.MaxMessageBytes,
		config.MaxActorBytes,
		1,
		config.MaxMessageBytes,
		defaultHandshakeTimeout,
		defaultWriteTimeout,
	)
}
