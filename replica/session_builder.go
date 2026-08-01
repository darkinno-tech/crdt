package replica

import (
	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
)

// SessionBuilder binds one validated manifest to the local protocol policy
// selected during authenticated group negotiation. It centralizes construction
// of the replication objects that must share that same policy.
//
// A builder is not a handshake: callers must still authenticate one exact
// manifest before creating it. The policy parameter is retained for source
// compatibility and future policy extensions.
type SessionBuilder struct {
	manifest Manifest
	policy   crdt.ProtocolPolicy
}

// NewSessionBuilder creates a manifest and binds it to policy. Use this after
// authenticating the group, schema, epoch, and exact protocol agreement.
func NewSessionBuilder(groupID, schemaID string, epoch uint64, protocol Protocol, policy crdt.ProtocolPolicy) (SessionBuilder, error) {
	manifest, err := NewManifest(groupID, schemaID, epoch, protocol, policy)
	if err != nil {
		return SessionBuilder{}, err
	}
	return NewSessionBuilderFromManifest(manifest, policy)
}

// NewSessionBuilderForFrameType creates a builder from one canonical
// registered frame type, without making callers repeat its state ID, delta ID,
// and semantics version. Applications must still authenticate the resulting
// exact manifest before a transport accepts frames.
func NewSessionBuilderForFrameType(groupID, schemaID string, epoch uint64, frameType crdt.FrameType, codecID string, policy crdt.ProtocolPolicy) (SessionBuilder, error) {
	manifest, err := NewManifestForFrameType(groupID, schemaID, epoch, frameType, codecID, policy)
	if err != nil {
		return SessionBuilder{}, err
	}
	return NewSessionBuilderFromManifest(manifest, policy)
}

// NewSessionBuilderFromManifest binds an already authenticated manifest to
// policy. It rejects a manifest that is structurally invalid or names an
// unknown protocol pair.
func NewSessionBuilderFromManifest(manifest Manifest, policy crdt.ProtocolPolicy) (SessionBuilder, error) {
	if err := manifest.validate(policy); err != nil {
		return SessionBuilder{}, ErrInvalidManifest
	}
	return SessionBuilder{manifest: manifest, policy: policy}, nil
}

// Manifest returns the immutable-by-convention agreement used by b.
func (b SessionBuilder) Manifest() Manifest { return b.manifest }

// NewChange validates one canonical delta frame under b's bound policy.
func (b SessionBuilder) NewChange(dot Dot, delta []byte) (Change, error) {
	return NewChangeWithPolicy(b.manifest, dot, delta, b.policy)
}

// NewInbox creates a bounded receiver under b's bound policy.
func (b SessionBuilder) NewInbox(frontier Frontier, maxPending, maxBytes int, apply ApplyDelta) (*Inbox, error) {
	return NewInboxWithPolicy(b.manifest, frontier, maxPending, maxBytes, apply, b.policy)
}

// NewCheckpoint validates one durable recovery boundary under b's bound
// policy. HLC-backed protocols require their persisted clock state.
func (b SessionBuilder) NewCheckpoint(state []byte, frontier Frontier, clockState clock.State, validator StateValidator) (Checkpoint, error) {
	return NewCheckpointWithPolicy(b.manifest, state, frontier, clockState, validator, b.policy)
}

// NewSession creates a checkpoint session under b's bound policy.
func (b SessionBuilder) NewSession() (*Session, error) {
	return NewSessionWithPolicy(b.manifest, b.policy)
}
