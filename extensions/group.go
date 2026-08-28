package extensions

import (
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/replica"
)

var (
	// ErrInvalidConfig reports a missing or unsafe extensions configuration.
	ErrInvalidConfig = errors.New("crdt extensions: invalid configuration")
	// ErrUnauthorized reports authentication or authorization failure.
	ErrUnauthorized = errors.New("crdt extensions: unauthorized")
	// ErrClosed reports use of a closed transport client.
	ErrClosed = errors.New("crdt extensions: client is closed")
	// ErrBatchUnsupported reports an unavailable batch subprotocol.
	ErrBatchUnsupported = errors.New("crdt extensions: websocket batch subprotocol is not enabled")
	// ErrBatchLimit reports a local batch that exceeds a configured boundary.
	ErrBatchLimit = errors.New("crdt extensions: websocket batch limit exceeded")
)

// Feature selects an optional transport surface. The zero value intentionally
// enables no transport.
type Feature uint8

const (
	// FeatureWebSocket enables the manifest-bound WebSocket endpoint.
	FeatureWebSocket Feature = 1 << iota
	// FeatureHTTP enables the HTTP publication and SSE live-event endpoints.
	FeatureHTTP
	// FeatureWebSocketBatch enables the opt-in crdt-sync-v2 WebSocket
	// subprotocol. It requires FeatureWebSocket and leaves HTTP/SSE on their
	// single-change v1 envelope.
	FeatureWebSocketBatch
)

const knownFeatures = FeatureWebSocket | FeatureHTTP | FeatureWebSocketBatch

// Enabled reports whether f includes feature.
func (f Feature) Enabled(feature Feature) bool {
	return f&feature == feature
}

// Peer is an authenticated application identity. ID must be stable and must
// not be taken from a client-supplied CRDT actor identifier.
type Peer struct {
	ID string
}

// Authenticate authenticates one transport request before it is upgraded or
// its body is read. Returning an error rejects the request.
type Authenticate func(*http.Request) (Peer, error)

// Authorize binds one proposed CRDT change to an authenticated peer and its
// negotiated manifest. At minimum it should prevent a peer from publishing as
// another logical replica actor.
type Authorize func(Peer, replica.Manifest, replica.Dot) error

// AuthorizeSubscription decides whether peer may receive live changes for the
// manifest. Read permission is deliberately separate from write authorization.
type AuthorizeSubscription func(Peer, replica.Manifest) error

// GroupConfig describes one bounded, manifest-bound receiver. Frontier must
// come from the same durable transaction as the application CRDT state when a
// production application restores a group.
type GroupConfig struct {
	Manifest          replica.Manifest
	Frontier          replica.Frontier
	Policy            crdt.ProtocolPolicy
	MaxPendingChanges int
	MaxPendingBytes   int
	Apply             replica.ApplyDelta
}

// Group owns a manifest-bound replica inbox and its currently connected live
// peers. It has no operation log, snapshot store, or replay history.
type Group struct {
	manifest replica.Manifest
	policy   crdt.ProtocolPolicy
	inbox    *replica.Inbox

	receiveMu sync.Mutex
	peersMu   sync.Mutex
	peers     map[subscriber]struct{}
}

// NewGroup creates one manifest-bound in-memory receiver. Apply runs while
// delivery ordering is held, so it must use the concrete CRDT decoder with
// application limits, leave the CRDT unchanged on an error, and not re-enter
// Group or block on a transport callback.
func NewGroup(config GroupConfig) (*Group, error) {
	if config.MaxPendingChanges <= 0 || config.MaxPendingBytes <= 0 || config.Apply == nil {
		return nil, invalidConfig("extensions.new_group", ErrInvalidConfig)
	}
	hello, err := marshalHello(config.Manifest)
	if err != nil || len(hello) > maxControlBytes {
		return nil, invalidConfig("extensions.new_group", ErrInvalidConfig)
	}
	inbox, err := replica.NewInboxWithPolicy(
		config.Manifest,
		config.Frontier,
		config.MaxPendingChanges,
		config.MaxPendingBytes,
		config.Apply,
		config.Policy,
	)
	if err != nil {
		return nil, crdt.WrapError(crdt.ErrorCodeInvalidConfig, "extensions.new_group", fmt.Errorf("create replica inbox: %w", err))
	}
	return &Group{
		manifest: config.Manifest,
		policy:   config.Policy,
		inbox:    inbox,
		peers:    make(map[subscriber]struct{}),
	}, nil
}

func invalidConfig(operation string, cause error) error {
	return crdt.WrapError(crdt.ErrorCodeInvalidConfig, operation, cause)
}

// Manifest returns the immutable-by-convention manifest negotiated by g.
func (g *Group) Manifest() replica.Manifest {
	if g == nil {
		return replica.Manifest{}
	}
	return g.manifest
}

// Frontier returns a copy of g's installed contiguous delivery frontier.
func (g *Group) Frontier() replica.Frontier {
	if g == nil {
		return replica.Frontier{}
	}
	return g.inbox.Frontier()
}

// Pending reports the number and bytes of out-of-order changes retained by g.
func (g *Group) Pending() (changes, bytes int) {
	if g == nil {
		return 0, 0
	}
	return g.inbox.Pending()
}

type subscriber interface {
	enqueue([]byte) bool
	enqueueAll([][]byte) bool
	close()
}

func (g *Group) add(peer subscriber) {
	g.peersMu.Lock()
	defer g.peersMu.Unlock()
	g.peers[peer] = struct{}{}
}

func (g *Group) remove(peer subscriber) {
	g.peersMu.Lock()
	defer g.peersMu.Unlock()
	delete(g.peers, peer)
}

func (g *Group) receive(peer Peer, authorize Authorize, data []byte, maxMessageBytes, maxActorBytes int) (replica.Delivery, error) {
	dot, delta, err := unmarshalChange(data, maxMessageBytes, maxActorBytes)
	if err != nil {
		return replica.Delivery{}, err
	}
	change, err := replica.NewChangeWithPolicy(g.manifest, dot, delta, g.policy)
	if err != nil {
		return replica.Delivery{}, fmt.Errorf("validate change: %w", err)
	}
	if err := authorize(peer, g.manifest, change.Dot); err != nil {
		return replica.Delivery{}, ErrUnauthorized
	}
	encoded, err := marshalChange(change)
	if err != nil {
		return replica.Delivery{}, err
	}
	g.receiveMu.Lock()
	defer g.receiveMu.Unlock()
	delivery, err := g.inbox.Receive(change)
	if err != nil {
		return replica.Delivery{}, fmt.Errorf("receive change: %w", err)
	}
	// Inbox intentionally does not retain bytes for an already installed dot,
	// so this in-memory relay cannot prove a conflicting retry is identical.
	// Suppressing known dots avoids exposing that retry to other peers. A durable
	// application relay must persist the dot-to-payload binding transactionally.
	if delivery.Accepted() {
		g.broadcast(encoded)
	}
	return delivery, nil
}

// receiveBatch accepts a bounded transport batch as independent CRDT changes.
// It is deliberately not an atomic application transaction: generic Apply
// callbacks cannot be rolled back by this package. Validation and authorization
// are completed for every item before the first Inbox mutation. If a later
// bounded Inbox or application mutation fails, every earlier accepted item is
// still forwarded before this function returns the error. Callers must keep
// their own durable outbox and retry each original change independently.
func (g *Group) receiveBatch(peer Peer, authorize Authorize, data []byte, maxMessageBytes, maxActorBytes, maxBatchChanges int) error {
	incoming, err := unmarshalChangeBatch(data, maxMessageBytes, maxActorBytes, maxBatchChanges)
	if err != nil {
		return err
	}
	type preparedChange struct {
		change  replica.Change
		encoded []byte
	}
	prepared := make([]preparedChange, 0, len(incoming))
	for _, item := range incoming {
		change, err := replica.NewChangeWithPolicy(g.manifest, item.dot, item.delta, g.policy)
		if err != nil {
			return fmt.Errorf("validate batch change: %w", err)
		}
		if err := authorize(peer, g.manifest, change.Dot); err != nil {
			return ErrUnauthorized
		}
		encoded, err := marshalChange(change)
		if err != nil {
			return err
		}
		prepared = append(prepared, preparedChange{change: change, encoded: encoded})
	}

	g.receiveMu.Lock()
	defer g.receiveMu.Unlock()
	accepted := make([][]byte, 0, len(prepared))
	for _, item := range prepared {
		delivery, err := g.inbox.Receive(item.change)
		if err != nil {
			g.broadcastAcceptedBatch(accepted)
			return fmt.Errorf("receive batch change: %w", err)
		}
		if delivery.Accepted() {
			accepted = append(accepted, item.encoded)
		}
	}
	g.broadcastAcceptedBatch(accepted)
	return nil
}

func (g *Group) broadcast(data []byte) {
	g.broadcastToSubscribers(func(peer subscriber) bool {
		return peer.enqueue(data)
	})
}

func (g *Group) broadcastAcceptedBatch(accepted [][]byte) {
	switch len(accepted) {
	case 0:
		return
	case 1:
		g.broadcast(accepted[0])
	default:
		g.broadcastBatch(accepted)
	}
}

func (g *Group) broadcastBatch(changes [][]byte) {
	batch, err := marshalEncodedChangeBatch(changes)
	if err != nil {
		// Changes were created by marshalChange above, so reaching this fallback
		// would indicate an internal encoding failure. Preserve accepted-change
		// liveness for existing peers instead of silently dropping them.
		g.broadcastToSubscribers(func(peer subscriber) bool {
			return peer.enqueueAll(changes)
		})
		return
	}
	g.broadcastToSubscribers(func(peer subscriber) bool {
		if batchPeer, ok := peer.(interface{ batchesEnabled() bool }); ok && batchPeer.batchesEnabled() {
			return peer.enqueue(batch)
		}
		return peer.enqueueAll(changes)
	})
}

func (g *Group) broadcastToSubscribers(enqueue func(subscriber) bool) {
	g.peersMu.Lock()
	peers := make([]subscriber, 0, len(g.peers))
	for peer := range g.peers {
		peers = append(peers, peer)
	}
	g.peersMu.Unlock()
	for _, peer := range peers {
		if !enqueue(peer) {
			peer.close()
			g.remove(peer)
		}
	}
}
