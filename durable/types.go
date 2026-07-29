package durable

import (
	"errors"
	"net/http"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/replica"
)

var (
	// ErrInvalidConfig reports a missing or unsafe durable relay configuration.
	ErrInvalidConfig = errors.New("crdt durable: invalid configuration")
	// ErrUnauthorized reports authentication or authorization failure.
	ErrUnauthorized = errors.New("crdt durable: unauthorized")
	// ErrConflictingDot reports a retry that reuses an existing Dot with a
	// different canonical payload. The existing binding remains authoritative.
	ErrConflictingDot = errors.New("crdt durable: conflicting dot")
	// ErrStoreFull reports that retaining another event would exceed the
	// configured operation-log budget. The server does not evict history.
	ErrStoreFull = errors.New("crdt durable: operation log limit reached")
	// ErrReplayUnavailable reports an invalid cursor or a replay that exceeds
	// the configured bounded replay window. Callers must bootstrap from a
	// validated application checkpoint instead of accepting a partial replay.
	ErrReplayUnavailable = errors.New("crdt durable: replay unavailable")
	// ErrCorruptStore reports damaged or internally inconsistent durable data.
	// The relay fails closed rather than guessing which operation to omit.
	ErrCorruptStore = errors.New("crdt durable: corrupt store")
	// ErrClosed reports use of a closed client or store.
	ErrClosed = errors.New("crdt durable: closed")
	// ErrQueueFull reports a bounded peer or client queue that cannot accept
	// another message without unbounded memory growth.
	ErrQueueFull = errors.New("crdt durable: queue full")
)

// Subprotocol identifies the durable relay control and binary-event protocol.
// It is independent from CRDT frame versions and extensions.Subprotocol.
const Subprotocol = "crdt-durable-v1"

// Peer is an authenticated application identity. ID must be stable and must
// never be copied from a client-controlled CRDT actor identifier.
type Peer struct {
	ID string
}

// Authenticate authenticates a request before the WebSocket upgrade.
type Authenticate func(*http.Request) (Peer, error)

// Authorize binds a proposed CRDT change to the authenticated peer and exact
// manifest. At minimum it must prevent a peer from publishing another actor.
type Authorize func(Peer, replica.Manifest, replica.Dot) error

// AuthorizeSubscription controls replay/live-event access independently from
// write authorization.
type AuthorizeSubscription func(Peer, replica.Manifest) error

// Validate checks a concrete CRDT delta before it is persisted or relayed.
// It must use application-selected bounds, make no application-state change,
// and return an error for an invalid payload. A frame checksum alone is not a
// sufficient semantic validator.
type Validate func([]byte) error

// GroupConfig defines one manifest-bound durable transport group.
type GroupConfig struct {
	Manifest replica.Manifest
	Policy   crdt.ProtocolPolicy
	Validate Validate
}

// Event is one committed transport-log entry. Sequence is strictly increasing
// within its group and is the only valid durable replay cursor.
type Event struct {
	Sequence uint64
	Change   replica.Change
}
