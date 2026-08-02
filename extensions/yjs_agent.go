package extensions

import (
	"context"
	"errors"
	"strings"
)

var (
	// ErrYJSAgentPeerUnavailable means the configured handler has no room with
	// the requested name. Callers must select the room from trusted application
	// configuration, never from model output.
	ErrYJSAgentPeerUnavailable = errors.New("crdt extensions: Yjs agent peer unavailable")
	// ErrYJSAgentPeerUnsupported means the room is an opaque live relay. A
	// server-side agent needs a Level 1 semantic store for bounded recovery; an
	// in-process relay history is not a complete Yjs document.
	ErrYJSAgentPeerUnsupported = errors.New("crdt extensions: Yjs agent peer requires a store-backed room")
)

// YJSAgentPeer is a trusted server-side CRDT participant for one configured,
// store-backed Yjs room. It deliberately exposes only snapshot/diff reads and
// standard binary Yjs update publication. A maintained Yjs runtime owns the
// document semantics; this package neither parses updates nor turns model text
// into CRDT mutations.
//
// The application supplies Peer from its service identity and invokes its own
// authorization callbacks on every read or write. Yjs client IDs, model
// prompts, and tool arguments must never be used as this identity or as a
// room selector.
type YJSAgentPeer struct {
	handler *YJSHandler
	room    *YJSRoom
	peer    Peer
}

// OpenYJSAgentPeer opens one authorized server-side participant. Only an
// explicitly configured Level 1 room can be opened: YJSStore supplies the
// semantic snapshot and state-vector recovery needed by a process that is not
// a long-lived WebSocket client. The returned peer revalidates authorization
// for each operation so a revoked service account cannot keep using a stale
// handle.
func (handler *YJSHandler) OpenYJSAgentPeer(peer Peer, roomName string) (*YJSAgentPeer, error) {
	if handler == nil {
		return nil, ErrYJSAgentPeerUnavailable
	}
	if strings.TrimSpace(peer.ID) == "" {
		return nil, ErrUnauthorized
	}
	room, ok := handler.rooms[roomName]
	if !ok {
		return nil, ErrYJSAgentPeerUnavailable
	}
	if room.store == nil {
		return nil, ErrYJSAgentPeerUnsupported
	}
	if err := handler.authorizeSubscription(peer, room.name); err != nil {
		return nil, err
	}
	return &YJSAgentPeer{handler: handler, room: room, peer: peer}, nil
}

// Snapshot returns one atomic durable Yjs recovery unit for an authorized
// agent. It is appropriate for a new task runner with no local Y.Doc. A
// runner that retains a bounded local Y.Doc should use Diff instead to avoid
// repeatedly moving a full document into its trusted tool runtime.
func (agent *YJSAgentPeer) Snapshot(ctx context.Context) (YJSSnapshot, error) {
	if err := agent.authorizeSubscription(); err != nil {
		return YJSSnapshot{}, err
	}
	parent, err := agent.storeParentContext(ctx)
	if err != nil {
		return YJSSnapshot{}, err
	}
	requestContext, cancel := context.WithTimeout(parent, agent.handler.storeTimeout)
	defer cancel()
	return agent.room.snapshotStore(requestContext)
}

// Diff returns the format-pinned Yjs update missing from remoteStateVector.
// It is the direct server-peer equivalent of SyncStep1/SyncStep2, but it does
// not expose a WebSocket connection or presence. Callers must apply it with
// the matching Yjs V1 or V2 API selected by the configured room.
func (agent *YJSAgentPeer) Diff(ctx context.Context, remoteStateVector []byte) ([]byte, error) {
	if agent == nil || agent.room == nil {
		return nil, ErrYJSAgentPeerUnavailable
	}
	if len(remoteStateVector) == 0 || len(remoteStateVector) > agent.room.maxStateVectorBytes {
		return nil, ErrYJSStoreLimit
	}
	if err := agent.authorizeSubscription(); err != nil {
		return nil, err
	}
	parent, err := agent.storeParentContext(ctx)
	if err != nil {
		return nil, err
	}
	requestContext, cancel := context.WithTimeout(parent, agent.handler.storeTimeout)
	defer cancel()
	return agent.room.diffStore(requestContext, remoteStateVector)
}

// Publish applies one standard Yjs update as the server-side peer. The update
// is authorized, durably applied, and only then fanned out to live clients.
// A false Applied result is an idempotent replay and is intentionally not
// fanned out again. It is not a receipt from individual browser peers.
func (agent *YJSAgentPeer) Publish(ctx context.Context, update []byte) (YJSApplyResult, error) {
	if agent == nil || agent.room == nil {
		return YJSApplyResult{}, ErrYJSAgentPeerUnavailable
	}
	if len(update) == 0 || len(update) > agent.room.maxUpdateBytes {
		return YJSApplyResult{}, ErrYJSStoreLimit
	}
	if err := agent.authorizePublish(); err != nil {
		return YJSApplyResult{}, err
	}
	parent, err := agent.storeParentContext(ctx)
	if err != nil {
		return YJSApplyResult{}, err
	}
	requestContext, cancel := context.WithTimeout(parent, agent.handler.storeTimeout)
	defer cancel()
	return agent.room.applyStoreUpdate(requestContext, update)
}

func (agent *YJSAgentPeer) authorizeSubscription() error {
	if agent == nil || agent.handler == nil || agent.room == nil {
		return ErrYJSAgentPeerUnavailable
	}
	return agent.handler.authorizeSubscription(agent.peer, agent.room.name)
}

func (agent *YJSAgentPeer) authorizePublish() error {
	if agent == nil || agent.handler == nil || agent.room == nil {
		return ErrYJSAgentPeerUnavailable
	}
	return agent.handler.authorize(agent.peer, agent.room.name, YJSUpdate)
}

func (agent *YJSAgentPeer) storeParentContext(ctx context.Context) (context.Context, error) {
	if agent == nil || agent.handler == nil {
		return nil, ErrYJSAgentPeerUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return ctx, nil
}

func (room *YJSRoom) snapshotStore(ctx context.Context) (YJSSnapshot, error) {
	if room == nil || room.store == nil {
		return YJSSnapshot{}, ErrYJSAgentPeerUnsupported
	}
	room.storeMu.Lock()
	defer room.storeMu.Unlock()
	snapshot, err := room.store.Snapshot(ctx, room.document)
	if err != nil {
		return YJSSnapshot{}, err
	}
	if len(snapshot.Update) == 0 || len(snapshot.Update) > room.maxSyncBytes || len(snapshot.StateVector) == 0 || len(snapshot.StateVector) > room.maxStateVectorBytes {
		return YJSSnapshot{}, ErrYJSStoreUnavailable
	}
	return snapshot, nil
}

func (room *YJSRoom) diffStore(ctx context.Context, remoteStateVector []byte) ([]byte, error) {
	if room == nil || room.store == nil {
		return nil, ErrYJSAgentPeerUnsupported
	}
	if len(remoteStateVector) == 0 || len(remoteStateVector) > room.maxStateVectorBytes {
		return nil, ErrYJSStoreLimit
	}
	room.storeMu.Lock()
	defer room.storeMu.Unlock()
	delta, err := room.store.Diff(ctx, room.document, remoteStateVector)
	if err != nil {
		return nil, err
	}
	if len(delta) == 0 || len(delta) > room.maxSyncBytes {
		return nil, ErrYJSStoreUnavailable
	}
	return delta, nil
}
