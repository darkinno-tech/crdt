package durable

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/replica"
	"github.com/coder/websocket"
)

const (
	defaultClientQueuedChanges      = 64
	defaultMinBackoff               = 100 * time.Millisecond
	defaultMaxBackoff               = 5 * time.Second
	defaultClientStateVectorEntries = 256
	defaultClientPingInterval       = 30 * time.Second
	defaultClientPingTimeout        = 10 * time.Second
)

// ClientConfig configures a reconnecting durable WebSocket client. OnEvent
// must durably install the concrete CRDT state and delivery frontier before it
// returns nil. Its transaction must also record event.Sequence as the resume
// cursor and settle any matching application outbox row.
type ClientConfig struct {
	Header                http.Header
	HTTPClient            *http.Client
	Policy                crdt.ProtocolPolicy
	MaxMessageBytes       int
	MaxActorBytes         int
	MaxQueuedChanges      int
	MaxStateVectorEntries int
	MaxMerkleLeaves       int
	MaxMerkleBytes        int
	HandshakeTimeout      time.Duration
	WriteTimeout          time.Duration
	PingInterval          time.Duration
	PingTimeout           time.Duration
	MinReconnectBackoff   time.Duration
	MaxReconnectBackoff   time.Duration
	Cursor                uint64
	// StateVector returns the contiguous frontier from the same durable
	// application checkpoint as the CRDT state. When set, the client requests
	// the v2 bounded missing-Dot catch-up protocol on every reconnect.
	StateVector func() replica.Frontier
	// OnCatchUp persists the state/frontier after all v2 catch-up events and
	// records highWater as the durable cursor in the same transaction. It is
	// required with StateVector because a skipped log event is never proof that
	// its payload was installed.
	OnCatchUp func(highWater uint64) error
	// MerkleRoot returns the root reconstructed from the same durable local
	// event inventory as the concrete CRDT checkpoint. When all three Merkle
	// callbacks are supplied, the client requests v3 without sending a state
	// vector.
	MerkleRoot func() [32]byte
	// ReconcileMerkle compares the complete, bounded remote inventory against
	// that durable local inventory and returns the sorted HLC identities absent
	// locally. It must reject an unexpected local-only or differently-digested
	// leaf instead of silently accepting a divergent history.
	ReconcileMerkle func([]MerkleLeaf) ([]crdt.Tag, error)
	// OnMerkleCatchUp atomically records the completed checkpoint boundary
	// after all requested events have been installed and MerkleRoot equals the
	// remote root. It is required for a v3 client.
	OnMerkleCatchUp func(MerkleBoundary) error
	OnEvent         func(Event) error
}

type clientLimits struct {
	maxMessageBytes       int
	maxActorBytes         int
	maxQueued             int
	maxStateVectorEntries int
	maxMerkleLeaves       uint64
	maxMerkleBytes        uint64
	handshake             time.Duration
	write                 time.Duration
	pingInterval          time.Duration
	pingTimeout           time.Duration
	minBackoff            time.Duration
	maxBackoff            time.Duration
}

// ReconnectClient reconnects with an application-provided durable cursor. Its
// in-memory queue is deliberately bounded and is not a replacement for an
// application outbox; persist an outgoing change before calling Publish.
type ReconnectClient struct {
	endpoint string
	manifest replica.Manifest
	policy   crdt.ProtocolPolicy
	config   ClientConfig
	limits   clientLimits

	outbound chan replica.Change

	mu      sync.Mutex
	cursor  uint64
	pending []replica.Change
	err     error
	running atomic.Bool
}

// NewReconnectClient validates configuration without making a network call.
func NewReconnectClient(endpoint string, manifest replica.Manifest, config ClientConfig) (*ReconnectClient, error) {
	if config.OnEvent == nil {
		return nil, invalidConfig("durable.new_reconnect_client", ErrInvalidConfig)
	}
	if config.StateVector != nil && config.OnCatchUp == nil {
		return nil, invalidConfig("durable.new_reconnect_client", ErrInvalidConfig)
	}
	usesMerkle := config.MerkleRoot != nil || config.ReconcileMerkle != nil || config.OnMerkleCatchUp != nil
	if usesMerkle && (config.MerkleRoot == nil || config.ReconcileMerkle == nil || config.OnMerkleCatchUp == nil) {
		return nil, invalidConfig("durable.new_reconnect_client", ErrInvalidConfig)
	}
	if usesMerkle && config.StateVector != nil {
		return nil, invalidConfig("durable.new_reconnect_client", ErrInvalidConfig)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, invalidConfig("durable.new_reconnect_client", ErrInvalidConfig)
	}
	if _, err := replica.NewSessionWithPolicy(manifest, config.Policy); err != nil {
		return nil, crdt.WrapError(crdt.ErrorCodeInvalidConfig, "durable.new_reconnect_client", fmt.Errorf("validate durable manifest: %w", err))
	}
	limits, err := normalizeClientLimits(config)
	if err != nil {
		return nil, invalidConfig("durable.new_reconnect_client", err)
	}
	return &ReconnectClient{
		endpoint: endpoint,
		manifest: manifest,
		policy:   config.Policy,
		config:   config,
		limits:   limits,
		outbound: make(chan replica.Change, limits.maxQueued),
		cursor:   config.Cursor,
	}, nil
}

// Cursor reports the highest event whose OnEvent callback succeeded during the
// current process. On restart, supply the cursor loaded from the same durable
// application transaction as the CRDT state/frontier.
func (client *ReconnectClient) Cursor() uint64 {
	if client == nil {
		return 0
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.cursor
}

// Err returns the last transient session error observed by Run. A successful
// handshake clears it; callers still own logging and operational policy.
func (client *ReconnectClient) Err() error {
	if client == nil {
		return ErrClosed
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.err
}

// Publish validates and queues one change for the next connected session. It
// only confirms bounded in-memory acceptance. The caller must retain its own
// durable outbox until it observes the echoed committed Event in OnEvent.
func (client *ReconnectClient) Publish(ctx context.Context, change replica.Change) error {
	if client == nil {
		return ErrClosed
	}
	verified, err := replica.NewChangeWithPolicy(client.manifest, change.Dot, change.Delta(), client.policy)
	if err != nil {
		return fmt.Errorf("validate durable publish: %w", err)
	}
	encoded, err := marshalChange(verified)
	if err != nil {
		return err
	}
	if len(encoded) > client.limits.maxMessageBytes || len(verified.Dot.Actor) > client.limits.maxActorBytes {
		return errInvalidWire
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case client.outbound <- verified:
		return nil
	default:
		return ErrQueueFull
	}
}

// Run maintains sessions until ctx is cancelled. It returns
// ErrReplayUnavailable without retrying because accepting a partial replay is
// unsafe; the caller must bootstrap a validated checkpoint first.
func (client *ReconnectClient) Run(ctx context.Context) error {
	if client == nil || !client.running.CompareAndSwap(false, true) {
		return ErrClosed
	}
	defer client.running.Store(false)
	backoff := client.limits.minBackoff
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		err := client.runSession(ctx)
		if errors.Is(err, ErrReplayUnavailable) {
			client.setErr(err)
			return err
		}
		if errors.Is(err, errInvalidWire) || errors.Is(err, ErrInvalidConfig) || errors.Is(err, ErrStateVectorUnavailable) || errors.Is(err, ErrAntiEntropyUnavailable) {
			client.setErr(err)
			return err
		}
		if err == nil {
			backoff = client.limits.minBackoff
			continue
		}
		client.setErr(err)
		if !waitContext(ctx, backoff) {
			return nil
		}
		backoff = nextBackoff(backoff, client.limits.maxBackoff)
	}
}

func (client *ReconnectClient) runSession(parent context.Context) error {
	vector := replica.Frontier{}
	useStateVector := client.config.StateVector != nil
	useMerkle := client.config.MerkleRoot != nil
	var merkleRoot [32]byte
	if useStateVector {
		vector = client.config.StateVector()
		if _, err := stateVectorEntries(vector, client.limits.maxStateVectorEntries, client.limits.maxActorBytes); err != nil {
			return invalidConfig("durable.reconnect_state_vector", err)
		}
	}
	protocols := []string{Subprotocol}
	if useMerkle {
		merkleRoot = client.config.MerkleRoot()
		// A v3 checkpoint is not interchangeable with the legacy cursor
		// protocol. Offering v1 here would let a server silently replace the
		// caller's root-equality recovery proof with a relay-local sequence.
		// v1/v2 callers keep their established compatibility paths below.
		protocols = []string{MerkleSubprotocol}
	} else if useStateVector {
		protocols = []string{StateVectorSubprotocol, Subprotocol}
	}
	handshakeContext, cancelHandshake := context.WithTimeout(parent, client.limits.handshake)
	defer cancelHandshake()
	connection, _, err := websocket.Dial(handshakeContext, client.endpoint, &websocket.DialOptions{
		HTTPClient:      client.config.HTTPClient,
		HTTPHeader:      cloneHeader(client.config.Header),
		Subprotocols:    protocols,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return err
	}
	defer func() { _ = connection.CloseNow() }()
	if connection.Subprotocol() != Subprotocol && connection.Subprotocol() != StateVectorSubprotocol && connection.Subprotocol() != MerkleSubprotocol {
		return errInvalidWire
	}
	connection.SetReadLimit(int64(controlLimit(client.limits.maxMessageBytes)))
	hello, err := client.marshalHello(connection.Subprotocol(), vector, merkleRoot)
	if err != nil || len(hello) > controlLimit(client.limits.maxMessageBytes) {
		return errInvalidWire
	}
	if err := connection.Write(handshakeContext, websocket.MessageText, hello); err != nil {
		return err
	}
	messageType, data, err := connection.Read(handshakeContext)
	if err != nil || messageType != websocket.MessageText {
		return errInvalidWire
	}
	var merkleBoundary MerkleBoundary
	remote, highWater, welcomeErr := unmarshalWelcomeForSubprotocol(connection.Subprotocol(), data)
	if connection.Subprotocol() == MerkleSubprotocol {
		remote, merkleBoundary, welcomeErr = unmarshalMerkleWelcome(data, client.limits.maxActorBytes)
		highWater = merkleBoundary.HighWater
	}
	if welcomeErr != nil {
		return unmarshalError(data)
	}
	if err := client.manifest.Compatible(remote); err != nil {
		return errInvalidWire
	}
	if client.Cursor() > highWater {
		return ErrReplayUnavailable
	}
	if connection.Subprotocol() == MerkleSubprotocol {
		return client.runMerkleSession(parent, handshakeContext, connection, merkleRoot, merkleBoundary)
	}
	connection.SetReadLimit(int64(client.limits.maxMessageBytes))
	sessionContext, cancelSession := context.WithCancel(parent)
	defer cancelSession()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		client.heartbeat(sessionContext, connection)
	}()
	writerDone := make(chan error, 1)
	writerStarted := false
	startWriter := func() {
		if writerStarted {
			return
		}
		writerStarted = true
		go func() { writerDone <- client.writeLoop(sessionContext, connection) }()
	}
	if client.Cursor() == highWater {
		startWriter()
	}
	defer func() {
		cancelSession()
		_ = connection.CloseNow()
		<-heartbeatDone
		if writerStarted {
			<-writerDone
		}
	}()
	client.setErr(nil)
	catchUpComplete := connection.Subprotocol() != StateVectorSubprotocol
	for {
		messageType, data, err := connection.Read(sessionContext)
		if err != nil {
			return err
		}
		if messageType == websocket.MessageText && connection.Subprotocol() == StateVectorSubprotocol && !catchUpComplete {
			completedHighWater, err := unmarshalCatchUpComplete(data)
			if err != nil || completedHighWater != highWater {
				return errInvalidWire
			}
			if err := client.config.OnCatchUp(highWater); err != nil {
				return fmt.Errorf("durably checkpoint state-vector catch-up at %d: %w", highWater, err)
			}
			client.setCursor(highWater)
			catchUpComplete = true
			startWriter()
			continue
		}
		if messageType != websocket.MessageBinary {
			return errInvalidWire
		}
		sequence, dot, delta, err := unmarshalEvent(data, client.limits.maxMessageBytes, client.limits.maxActorBytes)
		if err != nil {
			return err
		}
		event, err := newEventFromWire(client.manifest, client.policy, sequence, dot, delta)
		if err != nil {
			return errInvalidWire
		}
		if catchUpComplete {
			if want := client.Cursor() + 1; sequence != want {
				return ErrReplayUnavailable
			}
		}
		if err := client.config.OnEvent(event); err != nil {
			return fmt.Errorf("durably install event %d: %w", sequence, err)
		}
		if catchUpComplete {
			client.setCursor(sequence)
			if connection.Subprotocol() == Subprotocol && sequence == highWater {
				startWriter()
			}
		}
	}
}

func (client *ReconnectClient) runMerkleSession(parent, handshakeContext context.Context, connection *websocket.Conn, localRoot [32]byte, boundary MerkleBoundary) error {
	if client.config.MerkleRoot == nil || client.config.ReconcileMerkle == nil || client.config.OnMerkleCatchUp == nil {
		return ErrInvalidConfig
	}
	if localRoot != boundary.Root {
		leaves := make([]MerkleLeaf, 0)
		for {
			messageType, data, err := connection.Read(handshakeContext)
			if err != nil || messageType != websocket.MessageText {
				return errInvalidWire
			}
			part, done, err := unmarshalMerkleInventory(data, client.limits.maxActorBytes)
			if err != nil || uint64(len(part)) > client.limits.maxMerkleLeaves-uint64(len(leaves)) {
				return errInvalidWire
			}
			leaves = append(leaves, part...)
			if done {
				break
			}
		}
		if err := validateMerkleLeaves(leaves, client.limits.maxMerkleLeaves, client.limits.maxMerkleBytes, client.limits.maxActorBytes); err != nil {
			return errInvalidWire
		}
		identities, err := client.config.ReconcileMerkle(leaves)
		if err != nil {
			return fmt.Errorf("reconcile HLC/Merkle inventory: %w", err)
		}
		if err := validateMerkleIdentityRequest(identities, client.limits.maxMerkleLeaves, client.limits.maxMerkleBytes, client.limits.maxActorBytes); err != nil {
			return errInvalidWire
		}
		chunks, err := marshalMerkleRequestChunks(identities, controlLimit(client.limits.maxMessageBytes))
		if err != nil {
			return errInvalidWire
		}
		for _, chunk := range chunks {
			if err := connection.Write(handshakeContext, websocket.MessageText, chunk); err != nil {
				return err
			}
		}
		expected := make(map[string]struct{}, len(identities))
		for _, identity := range identities {
			expected[merkleLeafKey(identity)] = struct{}{}
		}
		for len(expected) > 0 {
			messageType, data, err := connection.Read(handshakeContext)
			if err != nil || messageType != websocket.MessageBinary {
				return errInvalidWire
			}
			sequence, tag, dot, delta, err := unmarshalMerkleEvent(data, client.limits.maxMessageBytes, client.limits.maxActorBytes)
			if err != nil || sequence > boundary.HighWater {
				return errInvalidWire
			}
			key := merkleLeafKey(tag)
			if _, requested := expected[key]; !requested {
				return errInvalidWire
			}
			event, err := newEventFromWire(client.manifest, client.policy, sequence, dot, delta)
			if err != nil {
				return errInvalidWire
			}
			event.HLC = tag
			if err := client.config.OnEvent(event); err != nil {
				return fmt.Errorf("durably install HLC/Merkle event %d: %w", sequence, err)
			}
			delete(expected, key)
		}
	}
	messageType, data, err := connection.Read(handshakeContext)
	if err != nil || messageType != websocket.MessageText {
		return errInvalidWire
	}
	completed, err := unmarshalMerkleComplete(data, client.limits.maxActorBytes)
	if err != nil || completed != boundary {
		return errInvalidWire
	}
	if client.config.MerkleRoot() != boundary.Root {
		return ErrAntiEntropyUnavailable
	}
	if err := client.config.OnMerkleCatchUp(boundary); err != nil {
		return fmt.Errorf("durably checkpoint HLC/Merkle catch-up at %d: %w", boundary.HighWater, err)
	}
	client.setCursor(boundary.HighWater)
	connection.SetReadLimit(int64(client.limits.maxMessageBytes))
	sessionContext, cancelSession := context.WithCancel(parent)
	defer cancelSession()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		client.heartbeat(sessionContext, connection)
	}()
	writerDone := make(chan error, 1)
	go func() { writerDone <- client.writeLoop(sessionContext, connection) }()
	defer func() {
		cancelSession()
		_ = connection.CloseNow()
		<-heartbeatDone
		<-writerDone
	}()
	client.setErr(nil)
	for {
		messageType, data, err := connection.Read(sessionContext)
		if err != nil {
			return err
		}
		if messageType != websocket.MessageBinary {
			return errInvalidWire
		}
		sequence, tag, dot, delta, err := unmarshalMerkleEvent(data, client.limits.maxMessageBytes, client.limits.maxActorBytes)
		if err != nil {
			return err
		}
		if want := client.Cursor() + 1; sequence != want {
			return ErrReplayUnavailable
		}
		event, err := newEventFromWire(client.manifest, client.policy, sequence, dot, delta)
		if err != nil {
			return errInvalidWire
		}
		event.HLC = tag
		if err := client.config.OnEvent(event); err != nil {
			return fmt.Errorf("durably install live HLC/Merkle event %d: %w", sequence, err)
		}
		client.setCursor(sequence)
	}
}

func (client *ReconnectClient) marshalHello(subprotocol string, vector replica.Frontier, roots ...[32]byte) ([]byte, error) {
	if subprotocol == MerkleSubprotocol {
		if len(roots) != 1 {
			return nil, errInvalidWire
		}
		root := roots[0]
		return marshalMerkleHello(client.manifest, root)
	}
	if subprotocol == StateVectorSubprotocol {
		return marshalStateVectorHello(client.manifest, vector, client.limits.maxStateVectorEntries, client.limits.maxActorBytes)
	}
	if subprotocol == Subprotocol {
		return marshalHello(client.manifest, client.Cursor())
	}
	return nil, errInvalidWire
}

func unmarshalWelcomeForSubprotocol(subprotocol string, data []byte) (replica.Manifest, uint64, error) {
	if subprotocol == StateVectorSubprotocol {
		return unmarshalStateVectorWelcome(data)
	}
	if subprotocol == Subprotocol {
		return unmarshalWelcome(data)
	}
	return replica.Manifest{}, 0, errInvalidWire
}

func (client *ReconnectClient) heartbeat(ctx context.Context, connection *websocket.Conn) {
	ticker := time.NewTicker(client.limits.pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingContext, cancel := context.WithTimeout(ctx, client.limits.pingTimeout)
			err := connection.Ping(pingContext)
			cancel()
			if err != nil {
				_ = connection.CloseNow()
				return
			}
		}
	}
}

func (client *ReconnectClient) writeLoop(ctx context.Context, connection *websocket.Conn) error {
	for {
		change, err := client.nextChange(ctx)
		if err != nil {
			return err
		}
		encoded, err := marshalChange(change)
		if err != nil {
			return err
		}
		writeContext, cancel := context.WithTimeout(ctx, client.limits.write)
		err = connection.Write(writeContext, websocket.MessageBinary, encoded)
		cancel()
		if err != nil {
			client.restore(change)
			_ = connection.CloseNow()
			return err
		}
	}
}

func (client *ReconnectClient) nextChange(ctx context.Context) (replica.Change, error) {
	client.mu.Lock()
	if len(client.pending) > 0 {
		change := client.pending[0]
		client.pending = client.pending[1:]
		client.mu.Unlock()
		return change, nil
	}
	client.mu.Unlock()
	select {
	case <-ctx.Done():
		return replica.Change{}, ctx.Err()
	case change := <-client.outbound:
		return change, nil
	}
}

func (client *ReconnectClient) restore(change replica.Change) {
	client.mu.Lock()
	client.pending = append([]replica.Change{change}, client.pending...)
	client.mu.Unlock()
}

func (client *ReconnectClient) setCursor(cursor uint64) {
	client.mu.Lock()
	client.cursor = cursor
	client.mu.Unlock()
}

func (client *ReconnectClient) setErr(err error) {
	client.mu.Lock()
	client.err = err
	client.mu.Unlock()
}

func normalizeClientLimits(config ClientConfig) (clientLimits, error) {
	if config.MaxMerkleLeaves < 0 || config.MaxMerkleBytes < 0 {
		return clientLimits{}, ErrInvalidConfig
	}
	result := clientLimits{
		maxMessageBytes:       config.MaxMessageBytes,
		maxActorBytes:         config.MaxActorBytes,
		maxQueued:             config.MaxQueuedChanges,
		maxStateVectorEntries: config.MaxStateVectorEntries,
		maxMerkleLeaves:       uint64(config.MaxMerkleLeaves),
		maxMerkleBytes:        uint64(config.MaxMerkleBytes),
		handshake:             config.HandshakeTimeout,
		write:                 config.WriteTimeout,
		pingInterval:          config.PingInterval,
		pingTimeout:           config.PingTimeout,
		minBackoff:            config.MinReconnectBackoff,
		maxBackoff:            config.MaxReconnectBackoff,
	}
	if result.maxMessageBytes == 0 {
		result.maxMessageBytes = defaultMaxMessageBytes
	}
	if result.maxActorBytes == 0 {
		result.maxActorBytes = defaultMaxActorBytes
	}
	if result.maxQueued == 0 {
		result.maxQueued = defaultClientQueuedChanges
	}
	if result.maxStateVectorEntries == 0 {
		result.maxStateVectorEntries = defaultClientStateVectorEntries
	}
	if result.maxMerkleLeaves == 0 {
		result.maxMerkleLeaves = defaultMerkleLeaves
	}
	if result.maxMerkleBytes == 0 {
		result.maxMerkleBytes = defaultMerkleBytes
	}
	if result.handshake == 0 {
		result.handshake = defaultHandshakeTimeout
	}
	if result.write == 0 {
		result.write = defaultWriteTimeout
	}
	if result.pingInterval == 0 {
		result.pingInterval = defaultClientPingInterval
	}
	if result.pingTimeout == 0 {
		result.pingTimeout = defaultClientPingTimeout
	}
	if result.minBackoff == 0 {
		result.minBackoff = defaultMinBackoff
	}
	if result.maxBackoff == 0 {
		result.maxBackoff = defaultMaxBackoff
	}
	if result.maxMessageBytes < 1024 || result.maxActorBytes <= 0 || result.maxActorBytes > frame.DefaultLimits().MaxStringBytes || result.maxQueued <= 0 || result.maxStateVectorEntries <= 0 || result.maxMerkleLeaves == 0 || result.maxMerkleBytes < uint64(result.maxActorBytes+32) || result.handshake <= 0 || result.write <= 0 || result.pingInterval <= 0 || result.pingTimeout <= 0 || result.minBackoff <= 0 || result.maxBackoff < result.minBackoff {
		return clientLimits{}, ErrInvalidConfig
	}
	return result, nil
}

func cloneHeader(header http.Header) http.Header {
	if header == nil {
		return make(http.Header)
	}
	return header.Clone()
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(current, maximum time.Duration) time.Duration {
	if current >= maximum/2 {
		return maximum
	}
	return current * 2
}
