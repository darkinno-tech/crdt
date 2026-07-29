package extensions

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/replica"
	"github.com/coder/websocket"
)

// ClientConfig configures either optional transport client. OnChange must pass
// received changes to an application-owned, manifest-compatible replica.Inbox
// or an equivalently durable boundary. HTTPClient is used for HTTP/SSE and, if
// non-nil, as the underlying client for WebSocket dialing.
type ClientConfig struct {
	Header           http.Header
	HTTPClient       *http.Client
	Policy           crdt.ProtocolPolicy
	MaxMessageBytes  int
	MaxActorBytes    int
	HandshakeTimeout time.Duration
	WriteTimeout     time.Duration
	OnChange         func(replica.Change) error
}

// WebSocketClient maintains one manifest-bound WebSocket live connection. It
// has no persistent outbox and does not reconnect automatically.
type WebSocketClient struct {
	manifest replica.Manifest
	policy   crdt.ProtocolPolicy
	onChange func(replica.Change) error

	maxMessageBytes int
	maxActorBytes   int
	writeTimeout    time.Duration
	connection      *websocket.Conn
	context         context.Context
	cancel          context.CancelFunc
	done            chan struct{}
	writeMu         sync.Mutex
	closeOnce       sync.Once
	errMu           sync.RWMutex
	err             error
}

// DialWebSocket authenticates the WebSocket handshake through config.Header,
// verifies the exact manifest returned by the relay, then receives live binary
// changes. Use wss after the host application has configured TLS.
func DialWebSocket(ctx context.Context, endpoint string, manifest replica.Manifest, config ClientConfig) (*WebSocketClient, error) {
	if config.OnChange == nil {
		return nil, ErrInvalidConfig
	}
	limits, err := normalizeClientLimits(config)
	if err != nil {
		return nil, err
	}
	if _, err := replica.NewSessionWithPolicy(manifest, config.Policy); err != nil {
		return nil, fmt.Errorf("validate manifest: %w", err)
	}
	handshakeContext, cancelHandshake := context.WithTimeout(ctx, limits.handshakeTimeout)
	defer cancelHandshake()
	connection, _, err := websocket.Dial(handshakeContext, endpoint, &websocket.DialOptions{
		HTTPClient:      config.HTTPClient,
		HTTPHeader:      cloneHeader(config.Header),
		Subprotocols:    []string{Subprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, err
	}
	if connection.Subprotocol() != Subprotocol {
		_ = connection.CloseNow()
		return nil, errInvalidWireMessage
	}
	connection.SetReadLimit(int64(controlLimit(limits.maxMessageBytes)))
	hello, err := marshalHello(manifest)
	if err != nil || len(hello) > controlLimit(limits.maxMessageBytes) {
		_ = connection.CloseNow()
		if err != nil {
			return nil, err
		}
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
	client := &WebSocketClient{
		manifest:        manifest,
		policy:          config.Policy,
		onChange:        config.OnChange,
		maxMessageBytes: limits.maxMessageBytes,
		maxActorBytes:   limits.maxActorBytes,
		writeTimeout:    limits.writeTimeout,
		connection:      connection,
		context:         connectionContext,
		cancel:          cancelConnection,
		done:            make(chan struct{}),
	}
	go client.readLoop()
	return client, nil
}

// Publish validates change against the connection manifest, then transmits its
// canonical envelope. A caller may retry after an ambiguous network result;
// durable outbox and recovery behavior remain application-owned.
func (client *WebSocketClient) Publish(ctx context.Context, change replica.Change) error {
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

// Done closes when the receive loop stops.
func (client *WebSocketClient) Done() <-chan struct{} {
	if client == nil {
		return nil
	}
	return client.done
}

// Err returns the first non-local connection or callback failure.
func (client *WebSocketClient) Err() error {
	if client == nil {
		return ErrClosed
	}
	client.errMu.RLock()
	defer client.errMu.RUnlock()
	return client.err
}

// Close terminates the live connection. It does not make a durability claim;
// persist an application checkpoint before a graceful recovery boundary.
func (client *WebSocketClient) Close() error {
	if client == nil {
		return ErrClosed
	}
	client.stop(nil)
	<-client.done
	return nil
}

func (client *WebSocketClient) readLoop() {
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
		dot, delta, err := unmarshalChange(data, client.maxMessageBytes, client.maxActorBytes)
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

func (client *WebSocketClient) stop(err error) {
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

func normalizeClientLimits(config ClientConfig) (transportLimits, error) {
	return normalizeLimits(
		config.MaxMessageBytes,
		config.MaxActorBytes,
		1,
		config.MaxMessageBytes,
		config.HandshakeTimeout,
		config.WriteTimeout,
	)
}
