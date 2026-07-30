package provider

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/awareness"
	"github.com/DarkInno/crdt/replica"
	"github.com/coder/websocket"
)

// ClientConfig configures one reference-provider client. OnChange must pass
// each change to an application-owned, manifest-compatible replica.Inbox (or
// another equally durable delivery boundary).
type ClientConfig struct {
	Header          http.Header
	Policy          crdt.ProtocolPolicy
	MaxMessageBytes int
	MaxActorBytes   int
	// EnableBatches negotiates BatchSubprotocol and allows PublishBatch. Each
	// contained change retains its own Dot; v1 remains the default.
	EnableBatches   bool
	MaxBatchChanges int
	// EnableAwareness requires the opt-in crdt-sync-v3 subprotocol. OnAwareness
	// receives only authenticated, bounded, transient messages; applications
	// should apply them to their own awareness.Store rather than a CRDT inbox.
	EnableAwareness  bool
	AwarenessOptions awareness.Options
	OnAwareness      func(awareness.Update) error
	HandshakeTimeout time.Duration
	WriteTimeout     time.Duration
	OnChange         func(replica.Change) error
}

// Client maintains one reference-provider WebSocket connection. It does not
// persist an outbox or automatically reconnect; callers decide retry and
// recovery policy.
type Client struct {
	manifest    replica.Manifest
	policy      crdt.ProtocolPolicy
	onChange    func(replica.Change) error
	onAwareness func(awareness.Update) error

	maxMessageBytes  int
	maxActorBytes    int
	maxBatchChanges  int
	writeTimeout     time.Duration
	batchEnabled     bool
	awarenessEnabled bool
	awarenessOptions awareness.Options
	connection       *websocket.Conn
	context          context.Context
	cancel           context.CancelFunc
	done             chan struct{}
	writeMu          sync.Mutex
	closeOnce        sync.Once
	errMu            sync.RWMutex
	err              error
}

// Dial authenticates the WebSocket handshake through config.Header, verifies
// that the server returns the exact manifest, then starts receiving binary
// changes. The endpoint should use wss in production after the application has
// configured TLS.
func Dial(ctx context.Context, endpoint string, manifest replica.Manifest, config ClientConfig) (*Client, error) {
	if config.OnChange == nil {
		return nil, ErrInvalidConfig
	}
	awarenessOptions, err := normalizeAwarenessOptions(config.EnableAwareness, config.AwarenessOptions)
	if err != nil || (config.EnableAwareness && config.OnAwareness == nil) {
		return nil, ErrInvalidConfig
	}
	limits, err := normalizeLimits(
		config.MaxMessageBytes,
		config.MaxActorBytes,
		1,
		config.MaxBatchChanges,
		config.HandshakeTimeout,
		config.WriteTimeout,
	)
	if err != nil {
		return nil, err
	}
	if _, err := replica.NewSessionWithPolicy(manifest, config.Policy); err != nil {
		return nil, fmt.Errorf("validate manifest: %w", err)
	}
	handshakeContext, cancelHandshake := context.WithTimeout(ctx, limits.handshakeTimeout)
	defer cancelHandshake()
	subprotocols := []string{Subprotocol}
	if config.EnableAwareness {
		subprotocols = []string{AwarenessSubprotocol, Subprotocol}
		if config.EnableBatches {
			subprotocols = []string{AwarenessSubprotocol, BatchSubprotocol, Subprotocol}
		}
	} else if config.EnableBatches {
		// Offer v1 as a fallback so an upgraded client can still connect to a
		// legacy provider. New handlers prefer BatchSubprotocol.
		subprotocols = []string{BatchSubprotocol, Subprotocol}
	}
	connection, _, err := websocket.Dial(handshakeContext, endpoint, &websocket.DialOptions{
		HTTPHeader:      config.Header.Clone(),
		Subprotocols:    subprotocols,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, err
	}
	awarenessEnabled := connection.Subprotocol() == AwarenessSubprotocol
	batchEnabled := connection.Subprotocol() == BatchSubprotocol || awarenessEnabled
	if connection.Subprotocol() != Subprotocol && !batchEnabled {
		_ = connection.CloseNow()
		return nil, errInvalidWireMessage
	}
	if config.EnableAwareness && !awarenessEnabled {
		_ = connection.CloseNow()
		return nil, ErrAwarenessUnsupported
	}
	connection.SetReadLimit(int64(controlLimit(limits.maxMessageBytes)))
	hello, err := marshalHello(manifest)
	if err != nil {
		_ = connection.CloseNow()
		return nil, err
	}
	if len(hello) > controlLimit(limits.maxMessageBytes) {
		_ = connection.CloseNow()
		return nil, ErrInvalidConfig
	}
	if err := connection.Write(handshakeContext, websocket.MessageText, hello); err != nil {
		_ = connection.CloseNow()
		return nil, err
	}
	messageType, data, err := connection.Read(handshakeContext)
	if err != nil || messageType != websocket.MessageText {
		_ = connection.CloseNow()
		return nil, errInvalidWireMessage
	}
	remote, err := unmarshalHello(data)
	if err != nil {
		_ = connection.CloseNow()
		return nil, err
	}
	if err := manifest.Compatible(remote); err != nil {
		_ = connection.CloseNow()
		return nil, err
	}
	connection.SetReadLimit(int64(limits.maxMessageBytes))
	connectionContext, cancelConnection := context.WithCancel(context.Background())
	client := &Client{
		manifest:         manifest,
		policy:           config.Policy,
		onChange:         config.OnChange,
		onAwareness:      config.OnAwareness,
		maxMessageBytes:  limits.maxMessageBytes,
		maxActorBytes:    limits.maxActorBytes,
		maxBatchChanges:  limits.maxBatchChanges,
		writeTimeout:     limits.writeTimeout,
		batchEnabled:     batchEnabled,
		awarenessEnabled: awarenessEnabled,
		awarenessOptions: awarenessOptions,
		connection:       connection,
		context:          connectionContext,
		cancel:           cancelConnection,
		done:             make(chan struct{}),
	}
	go client.readLoop()
	return client, nil
}

// PublishAwareness transmits one ephemeral actor state after local validation.
// It does not modify a replica.Inbox or establish a durability boundary.
func (client *Client) PublishAwareness(ctx context.Context, update awareness.Update) error {
	if client == nil {
		return ErrClosed
	}
	if !client.awarenessEnabled {
		return ErrAwarenessUnsupported
	}
	select {
	case <-client.context.Done():
		return ErrClosed
	default:
	}
	encoded, err := marshalAwareness(update, client.awarenessOptions)
	if err != nil {
		return err
	}
	if len(encoded) > client.maxMessageBytes {
		return errInvalidWireMessage
	}
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	writeContext, cancel := context.WithTimeout(ctx, client.writeTimeout)
	err = client.connection.Write(writeContext, websocket.MessageBinary, encoded)
	cancel()
	if err != nil {
		client.stop(err)
	}
	return err
}

// Publish validates change against the connection manifest, then transmits its
// canonical delta envelope. A caller may safely retry a change after an
// ambiguous network result; receiver inboxes and the concrete CRDT must remain
// idempotent.
func (client *Client) Publish(ctx context.Context, change replica.Change) error {
	if client == nil {
		return ErrClosed
	}
	select {
	case <-client.context.Done():
		return ErrClosed
	default:
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
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	writeContext, cancel := context.WithTimeout(ctx, client.writeTimeout)
	err = client.connection.Write(writeContext, websocket.MessageBinary, encoded)
	cancel()
	if err != nil {
		client.stop(err)
	}
	return err
}

// PublishBatch validates and sends a bounded group of independently identified
// changes in one WebSocket message. The server admits each Dot individually,
// so retry and outbox logic must retain every original change.
func (client *Client) PublishBatch(ctx context.Context, changes []replica.Change) error {
	if client == nil {
		return ErrClosed
	}
	if !client.batchEnabled {
		return ErrBatchUnsupported
	}
	select {
	case <-client.context.Done():
		return ErrClosed
	default:
	}
	if len(changes) == 0 || len(changes) > client.maxBatchChanges {
		return ErrBatchLimit
	}
	verified := make([]replica.Change, 0, len(changes))
	for _, change := range changes {
		item, err := replica.NewChangeWithPolicy(client.manifest, change.Dot, change.Delta(), client.policy)
		if err != nil {
			return fmt.Errorf("validate change: %w", err)
		}
		if len(item.Dot.Actor) > client.maxActorBytes {
			return errInvalidWireMessage
		}
		verified = append(verified, item)
	}
	encoded, err := marshalChangeBatch(verified)
	if err != nil {
		return err
	}
	if len(encoded) > client.maxMessageBytes {
		return ErrBatchLimit
	}
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	writeContext, cancel := context.WithTimeout(ctx, client.writeTimeout)
	err = client.connection.Write(writeContext, websocket.MessageBinary, encoded)
	cancel()
	if err != nil {
		client.stop(err)
	}
	return err
}

// Done closes when the receive loop stops.
func (client *Client) Done() <-chan struct{} {
	if client == nil {
		return nil
	}
	return client.done
}

// Err returns the first non-local connection or callback failure.
func (client *Client) Err() error {
	if client == nil {
		return ErrClosed
	}
	client.errMu.RLock()
	defer client.errMu.RUnlock()
	return client.err
}

// Close terminates the connection without attempting to make a client-side
// durability claim. Callers should persist their own checkpoint first when a
// graceful recovery boundary is required.
func (client *Client) Close() error {
	if client == nil {
		return ErrClosed
	}
	client.stop(nil)
	<-client.done
	return nil
}

func (client *Client) readLoop() {
	defer close(client.done)
	for {
		messageType, data, err := client.connection.Read(client.context)
		if err != nil {
			select {
			case <-client.context.Done():
				return
			default:
				client.stop(err)
				return
			}
		}
		if messageType != websocket.MessageBinary {
			client.stop(errInvalidWireMessage)
			return
		}
		if client.awarenessEnabled && isAwareness(data) {
			update, err := unmarshalAwareness(data, client.maxMessageBytes, client.awarenessOptions)
			if err != nil {
				client.stop(err)
				return
			}
			if err := client.onAwareness(update); err != nil {
				client.stop(fmt.Errorf("apply received awareness: %w", err))
				return
			}
			continue
		}
		if client.batchEnabled && isChangeBatch(data) {
			changes, err := unmarshalChangeBatch(data, client.maxMessageBytes, client.maxActorBytes, client.maxBatchChanges)
			if err != nil {
				client.stop(err)
				return
			}
			for _, wire := range changes {
				if err := client.applyChange(wire.dot, wire.delta); err != nil {
					client.stop(err)
					return
				}
			}
			continue
		}
		dot, delta, err := unmarshalChange(data, client.maxMessageBytes, client.maxActorBytes)
		if err != nil {
			client.stop(err)
			return
		}
		if err := client.applyChange(dot, delta); err != nil {
			client.stop(err)
			return
		}
	}
}

func (client *Client) applyChange(dot replica.Dot, delta []byte) error {
	change, err := replica.NewChangeWithPolicy(client.manifest, dot, delta, client.policy)
	if err != nil {
		return fmt.Errorf("validate received change: %w", err)
	}
	if err := client.onChange(change); err != nil {
		return fmt.Errorf("apply received change: %w", err)
	}
	return nil
}

func (client *Client) stop(err error) {
	if err != nil {
		client.errMu.Lock()
		if client.err == nil {
			client.err = err
		}
		client.errMu.Unlock()
	}
	client.closeOnce.Do(func() {
		client.cancel()
		_ = client.connection.CloseNow()
	})
}

func normalizeAwarenessOptions(enabled bool, options awareness.Options) (awareness.Options, error) {
	if !enabled {
		return awareness.Options{}, nil
	}
	if options == (awareness.Options{}) {
		options = awareness.DefaultOptions()
	}
	if _, err := awareness.NewStore(options); err != nil {
		return awareness.Options{}, err
	}
	return options, nil
}
