package extensions

import (
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/replica"
)

var (
	// ErrInvalidConfig reports a missing or unsafe extensions configuration.
	ErrInvalidConfig = errors.New("crdt extensions: invalid configuration")
	// ErrUnauthorized reports authentication or authorization failure.
	ErrUnauthorized = errors.New("crdt extensions: unauthorized")
	// ErrClosed reports use of a closed transport client.
	ErrClosed = errors.New("crdt extensions: client is closed")
)

// Feature selects an optional transport surface. The zero value intentionally
// enables no transport.
type Feature uint8

const (
	// FeatureWebSocket enables the manifest-bound WebSocket endpoint.
	FeatureWebSocket Feature = 1 << iota
	// FeatureHTTP enables the HTTP publication and SSE live-event endpoints.
	FeatureHTTP
)

const knownFeatures = FeatureWebSocket | FeatureHTTP

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
		return nil, ErrInvalidConfig
	}
	hello, err := marshalHello(config.Manifest)
	if err != nil || len(hello) > maxControlBytes {
		return nil, ErrInvalidConfig
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
		return nil, fmt.Errorf("create replica inbox: %w", err)
	}
	return &Group{
		manifest: config.Manifest,
		policy:   config.Policy,
		inbox:    inbox,
		peers:    make(map[subscriber]struct{}),
	}, nil
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

func (g *Group) broadcast(data []byte) {
	g.peersMu.Lock()
	peers := make([]subscriber, 0, len(g.peers))
	for peer := range g.peers {
		peers = append(peers, peer)
	}
	g.peersMu.Unlock()
	for _, peer := range peers {
		if !peer.enqueue(data) {
			peer.close()
			g.remove(peer)
		}
	}
}
