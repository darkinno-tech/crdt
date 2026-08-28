// Package replica defines the transport-independent boundary around one
// framed CRDT replication group.
//
// It deliberately does not open connections, authenticate peers, retain an
// operation log, or compact CRDT tombstones. Its job is narrower: make the
// agreement required before a transport exchanges frames explicit, keep a
// contiguous per-actor delivery frontier, and prevent an acknowledgement from
// being emitted before a checkpoint has been durably installed.
package replica

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/clock"
	frame "github.com/im10furry/crdt/encoding"
)

var (
	ErrInvalidManifest   = errors.New("replica: invalid manifest")
	ErrManifestMismatch  = errors.New("replica: manifest mismatch")
	ErrProtocolMismatch  = errors.New("replica: protocol mismatch")
	ErrInvalidDot        = errors.New("replica: invalid dot")
	ErrFrontierGap       = errors.New("replica: non-contiguous frontier advance")
	ErrInvalidCheckpoint = errors.New("replica: invalid checkpoint")
	ErrNilValidator      = errors.New("replica: nil state validator")
	ErrNilStore          = errors.New("replica: nil checkpoint store")
	ErrNotInstalled      = errors.New("replica: checkpoint is not installed")
	ErrCheckpointChanged = errors.New("replica: a different checkpoint is already installed")
	ErrInvalidChange     = errors.New("replica: invalid change")
	ErrDotConflict       = errors.New("replica: conflicting payload for one dot")
	ErrPendingLimit      = errors.New("replica: pending change limit exceeded")
	ErrNilApply          = errors.New("replica: nil delta apply function")
)

// Protocol identifies exactly one framed CRDT protocol in a replication
// group. SemanticsVersion is independent of encoding.FormatVersion: changing
// conflict semantics, snapshot meaning, or tombstone lifecycle requires a new
// semantic version (and, when frame compatibility is broken, new type IDs).
//
// CodecID is the schema/element codec identifier carried by every frame in
// this group. It may be empty for CRDTs whose canonical frames have no codec.
// WireFormatVersion selects the outer frame representation. Zero retains the
// v1 default for source and JSON compatibility; v2 is an explicit capability
// that must match across the authenticated manifest.
type Protocol struct {
	StateID           uint64
	DeltaID           uint64
	CodecID           string
	SemanticsVersion  uint64
	WireFormatVersion uint64
}

// FrameFormatVersion returns the negotiated outer encoding version. A zero
// field is the legacy spelling of encoding.FormatVersion.
func (p Protocol) FrameFormatVersion() uint64 {
	if p.WireFormatVersion == 0 {
		return frame.FormatVersion
	}
	return p.WireFormatVersion
}

// Manifest is the authenticated agreement for one CRDT replication group.
// GroupID and SchemaID are application-defined stable names. A group carries
// one concrete CRDT protocol; applications that replicate multiple objects
// create multiple manifests rather than treating unrelated frames as one
// atomic document.
type Manifest struct {
	GroupID  string
	SchemaID string
	Epoch    uint64
	Protocol Protocol
}

// NewManifest validates an immutable-by-convention manifest. policy is
// checked here so a caller cannot accidentally construct a group for a
// reserved or unknown frame type.
func NewManifest(groupID, schemaID string, epoch uint64, protocol Protocol, policy crdt.ProtocolPolicy) (Manifest, error) {
	manifest := Manifest{GroupID: groupID, SchemaID: schemaID, Epoch: epoch, Protocol: protocol}
	if err := manifest.validate(policy); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// ProtocolFromFrameType converts one canonical registered frame type into the
// protocol fields required by a manifest. It exists to keep applications from
// copying state IDs, delta IDs, and semantics versions by hand.
//
// frameType must exactly match a type returned by crdt.FrameTypeForState,
// crdt.RegisteredFrameTypes, or a ReplicationProfile. This helper does not
// choose a codec, authenticate a manifest, authorize a peer, or select input
// limits.
func ProtocolFromFrameType(frameType crdt.FrameType, codecID string) (Protocol, error) {
	registered, ok := crdt.FrameTypeForState(frameType.StateID)
	if !ok || registered != frameType || len(codecID) > frame.DefaultLimits().MaxCodecID {
		return Protocol{}, ErrInvalidManifest
	}
	return Protocol{
		StateID:          frameType.StateID,
		DeltaID:          frameType.DeltaID,
		CodecID:          codecID,
		SemanticsVersion: frameType.SemanticsVersion,
	}, nil
}

// NewManifestForFrameType builds a manifest for one canonical registered
// frame type. It is equivalent to ProtocolFromFrameType followed by
// NewManifest, and retains all manifest validation and policy checks.
func NewManifestForFrameType(groupID, schemaID string, epoch uint64, frameType crdt.FrameType, codecID string, policy crdt.ProtocolPolicy) (Manifest, error) {
	protocol, err := ProtocolFromFrameType(frameType, codecID)
	if err != nil {
		return Manifest{}, err
	}
	return NewManifest(groupID, schemaID, epoch, protocol, policy)
}

// Compatible reports whether local and remote describe the same replication
// group and exact CRDT semantics. It intentionally rejects a semantic-version
// mismatch rather than guessing that two versions can read one another.
func (m Manifest) Compatible(remote Manifest) error {
	if err := m.validate(crdt.ProtocolPolicy{}); err != nil {
		return ErrInvalidManifest
	}
	if err := remote.validate(crdt.ProtocolPolicy{}); err != nil {
		return ErrInvalidManifest
	}
	if m.GroupID != remote.GroupID || m.SchemaID != remote.SchemaID || m.Epoch != remote.Epoch {
		return ErrManifestMismatch
	}
	if m.Protocol.StateID != remote.Protocol.StateID || m.Protocol.DeltaID != remote.Protocol.DeltaID ||
		m.Protocol.CodecID != remote.Protocol.CodecID || m.Protocol.SemanticsVersion != remote.Protocol.SemanticsVersion ||
		m.Protocol.FrameFormatVersion() != remote.Protocol.FrameFormatVersion() {
		return ErrProtocolMismatch
	}
	return nil
}

func (m Manifest) validate(policy crdt.ProtocolPolicy) error {
	if strings.TrimSpace(m.GroupID) == "" || strings.TrimSpace(m.SchemaID) == "" || m.Epoch == 0 ||
		m.Protocol.SemanticsVersion == 0 || len(m.Protocol.CodecID) > frame.DefaultLimits().MaxCodecID ||
		(m.Protocol.FrameFormatVersion() != frame.FormatVersion && m.Protocol.FrameFormatVersion() != frame.FormatVersionV2) {
		return ErrInvalidManifest
	}
	kind, ok := crdt.FrameTypeForState(m.Protocol.StateID)
	if !ok || kind.DeltaID != m.Protocol.DeltaID || kind.SemanticsVersion != m.Protocol.SemanticsVersion ||
		!policy.SupportsFrame(m.Protocol.StateID) || !policy.SupportsFrame(m.Protocol.DeltaID) {
		return ErrInvalidManifest
	}
	return nil
}

// Dot is a persistently assigned, per-group operation sequence number. It is
// deliberately not an HLC tag: an HLC timestamp is ordered, but is not proof
// that every earlier timestamped mutation was received. A Frontier advances
// only over contiguous dots and is therefore safe for missing-update queries.
type Dot struct {
	Actor   string
	Counter uint64
}

func (d Dot) valid() bool { return strings.TrimSpace(d.Actor) != "" && d.Counter != 0 }

// Frontier records the greatest contiguous dot durably installed for each
// actor. Its internals are private so callers cannot create a false prefix by
// mutating a returned map.
type Frontier struct{ entries map[string]uint64 }

// NewFrontier returns an immutable-by-convention frontier from contiguous
// sequence values. Zero entries are rejected because absence already denotes
// counter zero.
func NewFrontier(entries map[string]uint64) (Frontier, error) {
	cloned := make(map[string]uint64, len(entries))
	for actor, counter := range entries {
		if strings.TrimSpace(actor) == "" || counter == 0 {
			return Frontier{}, ErrInvalidDot
		}
		cloned[actor] = counter
	}
	return Frontier{entries: cloned}, nil
}

// Counter returns the contiguous prefix installed for actor.
func (f Frontier) Counter(actor string) uint64 { return f.entries[actor] }

// Entries returns a copy of the complete frontier.
func (f Frontier) Entries() map[string]uint64 {
	entries := make(map[string]uint64, len(f.entries))
	for actor, counter := range f.entries {
		entries[actor] = counter
	}
	return entries
}

// Covers reports whether dot is included in f's installed prefix.
func (f Frontier) Covers(dot Dot) bool { return dot.valid() && f.Counter(dot.Actor) >= dot.Counter }

// Advance returns a frontier that includes dot. Duplicates are idempotent;
// receiving a future dot before its predecessor is rejected instead of making
// the frontier claim knowledge it cannot prove.
func (f Frontier) Advance(dot Dot) (Frontier, error) {
	if !dot.valid() {
		return Frontier{}, ErrInvalidDot
	}
	current := f.Counter(dot.Actor)
	if dot.Counter <= current {
		return f.clone(), nil
	}
	if dot.Counter != current+1 {
		return Frontier{}, ErrFrontierGap
	}
	next := f.clone()
	if next.entries == nil {
		next.entries = make(map[string]uint64, 1)
	}
	next.entries[dot.Actor] = dot.Counter
	return next, nil
}

func (f Frontier) clone() Frontier {
	cloned := make(map[string]uint64, len(f.entries))
	for actor, counter := range f.entries {
		cloned[actor] = counter
	}
	return Frontier{entries: cloned}
}

func (f Frontier) valid() bool {
	for actor, counter := range f.entries {
		if strings.TrimSpace(actor) == "" || counter == 0 {
			return false
		}
	}
	return true
}

// Change binds exactly one canonical delta frame to a persistently assigned
// dot. The dot is a delivery/accounting identity, not a replacement for the
// CRDT's own mutation tags inside the delta payload.
type Change struct {
	Dot       Dot
	manifest  Manifest
	delta     []byte
	validated bool
}

// NewChange validates that delta belongs to manifest's exact delta protocol
// and returns a copy. It does not decode the CRDT-specific payload; that work
// remains the concrete ApplyDelta implementation's responsibility.
func NewChange(manifest Manifest, dot Dot, delta []byte) (Change, error) {
	return NewChangeWithPolicy(manifest, dot, delta, crdt.ProtocolPolicy{})
}

// NewChangeWithPolicy validates one change under policy. The policy parameter
// remains for source compatibility and future policy extensions.
func NewChangeWithPolicy(manifest Manifest, dot Dot, delta []byte, policy crdt.ProtocolPolicy) (Change, error) {
	if manifest.validate(policy) != nil || !dot.valid() {
		return Change{}, ErrInvalidChange
	}
	decoded, err := frame.UnmarshalFrame(delta, frame.DefaultLimits())
	if err != nil || decoded.Version() != manifest.Protocol.FrameFormatVersion() || decoded.TypeID != manifest.Protocol.DeltaID || decoded.CodecID != manifest.Protocol.CodecID {
		return Change{}, ErrInvalidChange
	}
	// Change owns this copy and its private fields cannot be modified by callers
	// outside this package. Receive still validates the dot and manifest on every
	// delivery, but can avoid reparsing this already checked frame.
	return Change{Dot: dot, manifest: manifest, delta: append([]byte(nil), delta...), validated: true}, nil
}

// Delta returns an owned copy of the canonical delta frame.
func (c Change) Delta() []byte { return append([]byte(nil), c.delta...) }

func (c Change) validate(manifest Manifest) error {
	if !c.Dot.valid() || len(c.delta) == 0 {
		return ErrInvalidChange
	}
	if err := manifest.Compatible(c.manifest); err != nil {
		return err
	}
	if c.validated {
		return nil
	}
	decoded, err := frame.UnmarshalFrame(c.delta, frame.DefaultLimits())
	if err != nil || decoded.Version() != manifest.Protocol.FrameFormatVersion() || decoded.TypeID != manifest.Protocol.DeltaID || decoded.CodecID != manifest.Protocol.CodecID {
		return ErrInvalidChange
	}
	return nil
}

// ApplyDelta applies one already validated canonical delta frame. It must
// leave the concrete CRDT unchanged on an error. Inbox advances its frontier
// only after this function succeeds.
type ApplyDelta func([]byte) error

// Delivery describes the result of receiving one change. Buffered reports an
// out-of-order change retained for its missing per-actor prefix; Applied lists
// every dot installed by this call, including any now-unblocked buffered
// changes. Duplicate reports that this call did not retain or install its
// change because the same dot was already known.
type Delivery struct {
	Buffered  bool
	Duplicate bool
	Applied   []Dot
}

// Accepted reports whether this call added a change to the receiver's pending
// queue or installed at least one dot. Relays should forward only accepted
// changes: once a dot is already installed, an Inbox no longer retains its
// payload bytes and therefore cannot prove a later same-dot payload is
// identical. Durable relays must additionally bind actor/counter to payload
// identity in their application-owned operation store.
func (d Delivery) Accepted() bool {
	return !d.Duplicate && (d.Buffered || len(d.Applied) > 0)
}

// Inbox is a bounded, transport-independent receiver for one manifest. It
// accepts duplicate and out-of-order deliveries, but its frontier advances
// only across contiguous persisted actor counters. Applications must persist
// the concrete CRDT state and the resulting frontier atomically before using
// it as a recovery/checkpoint boundary.
type Inbox struct {
	mu           sync.Mutex
	manifest     Manifest
	frontier     Frontier
	apply        ApplyDelta
	maxPending   int
	maxBytes     int
	pending      map[string]map[uint64]Change
	pendingCount int
	pendingSize  int
}

// NewInbox creates a bounded receiver. maxPending and maxBytes cover only
// deferred out-of-order frames; immediately applicable frames are bounded by
// encoding.DefaultLimits and the concrete CRDT decoder.
func NewInbox(manifest Manifest, frontier Frontier, maxPending, maxBytes int, apply ApplyDelta) (*Inbox, error) {
	return NewInboxWithPolicy(manifest, frontier, maxPending, maxBytes, apply, crdt.ProtocolPolicy{})
}

// NewInboxWithPolicy creates a bounded receiver under policy. The policy
// parameter remains for source compatibility and future policy extensions.
func NewInboxWithPolicy(manifest Manifest, frontier Frontier, maxPending, maxBytes int, apply ApplyDelta, policy crdt.ProtocolPolicy) (*Inbox, error) {
	if manifest.validate(policy) != nil || !frontier.valid() || maxPending <= 0 || maxBytes <= 0 {
		return nil, ErrInvalidChange
	}
	if apply == nil {
		return nil, ErrNilApply
	}
	return &Inbox{
		manifest:   manifest,
		frontier:   frontier.clone(),
		apply:      apply,
		maxPending: maxPending,
		maxBytes:   maxBytes,
		pending:    make(map[string]map[uint64]Change),
	}, nil
}

// Receive applies change immediately when it is the next expected dot for its
// actor, otherwise it buffers it. A conflicting duplicate dot is rejected;
// accepting either payload would make recovery depend on arrival order.
func (i *Inbox) Receive(change Change) (Delivery, error) {
	if i == nil {
		return Delivery{}, ErrInvalidChange
	}
	if err := change.validate(i.manifest); err != nil {
		return Delivery{}, err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	current := i.frontier.Counter(change.Dot.Actor)
	if change.Dot.Counter <= current {
		return Delivery{Duplicate: true}, nil
	}
	if change.Dot.Counter > current+1 {
		return i.buffer(change)
	}
	if existing, ok := i.pending[change.Dot.Actor][change.Dot.Counter]; ok && !bytes.Equal(existing.delta, change.delta) {
		return Delivery{}, ErrDotConflict
	}
	if err := i.apply(change.Delta()); err != nil {
		return Delivery{}, err
	}
	if pending := i.pending[change.Dot.Actor]; pending != nil {
		if existing, ok := pending[change.Dot.Counter]; ok {
			delete(pending, change.Dot.Counter)
			i.pendingCount--
			i.pendingSize -= len(existing.delta)
			if len(pending) == 0 {
				delete(i.pending, change.Dot.Actor)
			}
		}
	}
	if err := i.advance(change.Dot); err != nil {
		return Delivery{}, err
	}
	delivery := Delivery{Applied: []Dot{change.Dot}}
	for {
		nextCounter := i.frontier.Counter(change.Dot.Actor) + 1
		next, ok := i.pending[change.Dot.Actor][nextCounter]
		if !ok {
			return delivery, nil
		}
		if err := i.apply(next.Delta()); err != nil {
			return delivery, err
		}
		delete(i.pending[change.Dot.Actor], nextCounter)
		i.pendingCount--
		if len(i.pending[change.Dot.Actor]) == 0 {
			delete(i.pending, change.Dot.Actor)
		}
		i.pendingSize -= len(next.delta)
		if err := i.advance(next.Dot); err != nil {
			return delivery, err
		}
		delivery.Applied = append(delivery.Applied, next.Dot)
	}
}

func (i *Inbox) buffer(change Change) (Delivery, error) {
	byCounter := i.pending[change.Dot.Actor]
	if existing, ok := byCounter[change.Dot.Counter]; ok {
		if !bytes.Equal(existing.delta, change.delta) {
			return Delivery{}, ErrDotConflict
		}
		return Delivery{Buffered: true, Duplicate: true}, nil
	}
	if i.pendingCount >= i.maxPending || len(change.delta) > i.maxBytes-i.pendingSize {
		return Delivery{}, ErrPendingLimit
	}
	if byCounter == nil {
		byCounter = make(map[uint64]Change)
		i.pending[change.Dot.Actor] = byCounter
	}
	byCounter[change.Dot.Counter] = Change{Dot: change.Dot, delta: append([]byte(nil), change.delta...)}
	i.pendingCount++
	i.pendingSize += len(change.delta)
	return Delivery{Buffered: true}, nil
}

func (i *Inbox) advance(dot Dot) error {
	frontier, err := i.frontier.Advance(dot)
	if err != nil {
		return err
	}
	i.frontier = frontier
	return nil
}

// Frontier returns a copy of the installed contiguous delivery frontier.
func (i *Inbox) Frontier() Frontier {
	if i == nil {
		return Frontier{}
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.frontier.clone()
}

// Pending reports the current bounded deferred queue size.
func (i *Inbox) Pending() (changes, bytes int) {
	if i == nil {
		return 0, 0
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.pendingCount, i.pendingSize
}

// StateValidator must perform concrete CRDT validation of one complete state
// frame. A frame-envelope checksum is not sufficient evidence that a snapshot
// is recoverable. Validators receive owned bytes, and a panic is treated as a
// rejected checkpoint.
type StateValidator func([]byte) error

// Checkpoint is a validated, immutable recovery boundary for one replication
// group. HLC-backed CRDTs include their local clock state so the local replica
// cannot reuse an earlier tag after restoring the checkpoint.
type Checkpoint struct {
	manifest   Manifest
	state      []byte
	frontier   Frontier
	clockState *clock.State
	id         [sha256.Size]byte
}

// NewCheckpoint validates the manifest/frame agreement and invokes validator
// before a checkpoint may be persisted. For HLC-backed protocols, clockState
// must be valid; for non-HLC protocols it must be the zero value.
func NewCheckpoint(manifest Manifest, state []byte, frontier Frontier, clockState clock.State, validator StateValidator) (Checkpoint, error) {
	return NewCheckpointWithPolicy(manifest, state, frontier, clockState, validator, crdt.ProtocolPolicy{})
}

// NewCheckpointWithPolicy validates a checkpoint under policy. The policy
// parameter remains for source compatibility and future policy extensions.
func NewCheckpointWithPolicy(manifest Manifest, state []byte, frontier Frontier, clockState clock.State, validator StateValidator, policy crdt.ProtocolPolicy) (Checkpoint, error) {
	if validator == nil {
		return Checkpoint{}, ErrNilValidator
	}
	if manifest.validate(policy) != nil || !frontier.valid() {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	decoded, err := frame.UnmarshalFrame(state, frame.DefaultLimits())
	if err != nil || decoded.Version() != manifest.Protocol.FrameFormatVersion() || decoded.TypeID != manifest.Protocol.StateID || decoded.CodecID != manifest.Protocol.CodecID || !validateState(validator, state) {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	checkpoint := Checkpoint{manifest: manifest, state: append([]byte(nil), state...), frontier: frontier.clone()}
	kind, _ := crdt.FrameTypeForState(manifest.Protocol.StateID)
	if kind.UsesHLC {
		if _, err := clock.NewHLCFromState(clockState); err != nil {
			return Checkpoint{}, ErrInvalidCheckpoint
		}
		copied := clockState
		checkpoint.clockState = &copied
	} else if clockState != (clock.State{}) {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	checkpoint.id = checkpoint.digest()
	return checkpoint, nil
}

// Manifest returns the checkpoint's manifest by value.
func (c Checkpoint) Manifest() Manifest { return c.manifest }

// State returns a copy of the canonical concrete state frame.
func (c Checkpoint) State() []byte { return append([]byte(nil), c.state...) }

// Frontier returns a copy of the durable delivery frontier.
func (c Checkpoint) Frontier() Frontier { return c.frontier.clone() }

// ClockState reports the HLC state included in c.
func (c Checkpoint) ClockState() (clock.State, bool) {
	if c.clockState == nil {
		return clock.State{}, false
	}
	return *c.clockState, true
}

// ID is a stable digest over all checkpoint meaning, including epoch, protocol
// semantics, state bytes, frontier, and HLC state.
func (c Checkpoint) ID() [sha256.Size]byte { return c.id }

func (c Checkpoint) valid() bool {
	if err := c.manifest.validate(crdt.ProtocolPolicy{}); err != nil || !c.frontier.valid() || len(c.state) == 0 {
		return false
	}
	decoded, err := frame.UnmarshalFrame(c.state, frame.DefaultLimits())
	if err != nil || decoded.Version() != c.manifest.Protocol.FrameFormatVersion() || decoded.TypeID != c.manifest.Protocol.StateID || decoded.CodecID != c.manifest.Protocol.CodecID {
		return false
	}
	kind, _ := crdt.FrameTypeForState(c.manifest.Protocol.StateID)
	if kind.UsesHLC {
		if c.clockState == nil {
			return false
		}
		if _, err := clock.NewHLCFromState(*c.clockState); err != nil {
			return false
		}
	} else if c.clockState != nil {
		return false
	}
	return c.id == c.digest()
}

func (c Checkpoint) digest() [sha256.Size]byte {
	encoded := make([]byte, 0, len(c.manifest.GroupID)+len(c.manifest.SchemaID)+len(c.manifest.Protocol.CodecID)+len(c.state)+128)
	encoded = append(encoded, "crdt/checkpoint/v1"...)
	encoded = appendString(encoded, c.manifest.GroupID)
	encoded = appendString(encoded, c.manifest.SchemaID)
	encoded = binary.AppendUvarint(encoded, c.manifest.Epoch)
	encoded = binary.AppendUvarint(encoded, c.manifest.Protocol.StateID)
	encoded = binary.AppendUvarint(encoded, c.manifest.Protocol.DeltaID)
	encoded = appendString(encoded, c.manifest.Protocol.CodecID)
	encoded = binary.AppendUvarint(encoded, c.manifest.Protocol.SemanticsVersion)
	encoded = binary.AppendUvarint(encoded, c.manifest.Protocol.FrameFormatVersion())
	encoded = appendBytes(encoded, c.state)
	actors := make([]string, 0, len(c.frontier.entries))
	for actor := range c.frontier.entries {
		actors = append(actors, actor)
	}
	sort.Strings(actors)
	encoded = binary.AppendUvarint(encoded, uint64(len(actors)))
	for _, actor := range actors {
		encoded = appendString(encoded, actor)
		encoded = binary.AppendUvarint(encoded, c.frontier.entries[actor])
	}
	if c.clockState == nil {
		return sha256.Sum256(append(encoded, 0))
	}
	encoded = append(encoded, 1)
	encoded = appendString(encoded, c.clockState.ReplicaID)
	encoded = binary.AppendUvarint(encoded, c.clockState.WallTime)
	encoded = binary.AppendUvarint(encoded, c.clockState.Logical)
	return sha256.Sum256(encoded)
}

func appendString(dst []byte, value string) []byte { return appendBytes(dst, []byte(value)) }

func appendBytes(dst, value []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func validateState(validator StateValidator, state []byte) (valid bool) {
	defer func() {
		if recover() != nil {
			valid = false
		}
	}()
	return validator(append([]byte(nil), state...)) == nil
}

// CheckpointStore atomically records the state frame, HLC state (when any),
// frontier, checkpoint ID, and epoch. Its success result is the durability
// boundary used by Session before it emits an acknowledgement.
type CheckpointStore interface {
	SaveCheckpoint(Checkpoint) error
}

// Acknowledgement proves that one installed checkpoint was durably recorded.
// It is intentionally not a tombstone-GC acknowledgement: eligibility to
// compact a tombstone remains type-specific, especially for RGA and trees.
type Acknowledgement struct {
	GroupID      string
	Epoch        uint64
	CheckpointID [sha256.Size]byte
	Frontier     Frontier
}

// Session binds a manifest to an optional checkpoint. It is safe for
// concurrent callers. A session permits only one checkpoint ID: a rebase must
// create a new epoch/manifest instead of silently replacing a recovery base.
type Session struct {
	mu         sync.RWMutex
	manifest   Manifest
	checkpoint *Checkpoint
}

// NewSession creates a checkpoint session for one implemented-protocol manifest.
func NewSession(manifest Manifest) (*Session, error) {
	return NewSessionWithPolicy(manifest, crdt.ProtocolPolicy{})
}

// NewSessionWithPolicy creates a checkpoint session under policy. The policy
// parameter remains for source compatibility and future policy extensions.
func NewSessionWithPolicy(manifest Manifest, policy crdt.ProtocolPolicy) (*Session, error) {
	if manifest.validate(policy) != nil {
		return nil, ErrInvalidManifest
	}
	return &Session{manifest: manifest}, nil
}

// Install persists checkpoint before publishing it to the session. If storage
// fails, the session remains unable to acknowledge the checkpoint.
func (s *Session) Install(checkpoint Checkpoint, store CheckpointStore) error {
	if s == nil || store == nil {
		return ErrNilStore
	}
	if !checkpoint.valid() {
		return ErrInvalidCheckpoint
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.manifest.Compatible(checkpoint.manifest); err != nil {
		return err
	}
	if s.checkpoint != nil {
		if s.checkpoint.id == checkpoint.id {
			return nil
		}
		return ErrCheckpointChanged
	}
	if err := store.SaveCheckpoint(checkpoint); err != nil {
		return err
	}
	copied := checkpoint
	s.checkpoint = &copied
	return nil
}

// Acknowledge returns a proof of durable checkpoint installation. The returned
// frontier is a copy and cannot mutate Session state.
func (s *Session) Acknowledge() (Acknowledgement, error) {
	if s == nil {
		return Acknowledgement{}, ErrNotInstalled
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.checkpoint == nil {
		return Acknowledgement{}, ErrNotInstalled
	}
	return Acknowledgement{
		GroupID:      s.manifest.GroupID,
		Epoch:        s.manifest.Epoch,
		CheckpointID: s.checkpoint.id,
		Frontier:     s.checkpoint.frontier.clone(),
	}, nil
}
