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
	// ErrStateVectorUnavailable reports that a storage implementation cannot
	// calculate a complete bounded suffix for a durable delivery frontier.
	// Callers must fall back to a valid cursor replay or bootstrap from a
	// validated application checkpoint; accepting a partial catch-up is unsafe.
	ErrStateVectorUnavailable = errors.New("crdt durable: state-vector catch-up unavailable")
	// ErrCorruptStore reports damaged or internally inconsistent durable data.
	// The relay fails closed rather than guessing which operation to omit.
	ErrCorruptStore = errors.New("crdt durable: corrupt store")
	// ErrClosed reports use of a closed client or store.
	ErrClosed = errors.New("crdt durable: closed")
	// ErrQueueFull reports a bounded peer or client queue that cannot accept
	// another message without unbounded memory growth.
	ErrQueueFull = errors.New("crdt durable: queue full")
)

// Subprotocol identifies the stable cursor-resume durable relay protocol. It
// is independent from CRDT frame versions and extensions.Subprotocol.
const Subprotocol = "crdt-durable-v1"

// StateVectorSubprotocol identifies the optional state-vector catch-up
// protocol. A v2 peer proves only its own installed Dot prefixes; the vector
// is neither authentication, a receipt, nor permission to compact tombstones.
const StateVectorSubprotocol = "crdt-durable-v2"

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

// RevalidateSubscription is invoked periodically for an established
// subscription. Hosts use it to apply session expiry or revocation policy to
// long-lived connections; a failure closes only that connection.
type RevalidateSubscription func(Peer, replica.Manifest) error

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

// AppendResult records the outcome of one idempotent log append. A duplicate
// Dot is safe only when the store verified that its canonical payload is
// identical to the existing binding.
type AppendResult struct {
	Event     Event
	Duplicate bool
}

// Log is the durable-relay storage contract. Implementations must make Append
// atomic: a new event, its group-local sequence, the Dot-to-canonical-payload
// binding, and capacity accounting either all become durable or none do.
//
// Replay must return a contiguous complete suffix or ErrReplayUnavailable;
// returning a prefix silently would let a receiver advance its cursor past
// missing CRDT changes. Implementations validate stored bytes against the
// manifest and policy supplied by the relay before returning them.
//
// The relay never closes a Log. Its owner controls connection lifetime so a
// shared PostgreSQL or Redis client pool can serve more than one handler.
type Log interface {
	Append(groupID string, change replica.Change) (AppendResult, error)
	Replay(groupID string, after, maxEvents, maxBytes uint64, manifest replica.Manifest, policy crdt.ProtocolPolicy, maxMessageBytes, maxActorBytes int) ([]Event, uint64, error)
	Closed() bool
}

// StateVectorLog is an optional Log capability for bounded recovery from a
// replica.Frontier. CatchUp must return every event whose Dot is not covered
// by vector, ordered by durable sequence, or return an error. It must never
// return a convenient partial result.
//
// The base Log contract intentionally remains cursor-based so existing Redis,
// PostgreSQL, and host implementations continue to work with v1 clients.
type StateVectorLog interface {
	Log
	CatchUp(groupID string, vector replica.Frontier, maxEvents, maxBytes uint64, manifest replica.Manifest, policy crdt.ProtocolPolicy, maxMessageBytes, maxActorBytes int) ([]Event, uint64, error)
}

func invalidConfig(operation string, cause error) error {
	return crdt.WrapError(crdt.ErrorCodeInvalidConfig, operation, cause)
}
