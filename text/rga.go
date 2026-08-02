// Package text implements a state-based Replicated Growable Array (RGA).
//
// Positions are stable mutation tags, not offsets. Offsets are resolved only
// for a local edit, which makes duplicate and out-of-order deltas safe.
package text

import (
	"errors"
	"sort"
	"sync"
	"unicode/utf8"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
)

var (
	ErrNilText          = errors.New("text: nil RGA")
	ErrInvalidReplicaID = errors.New("text: invalid replica ID")
	ErrInvalidText      = errors.New("text: invalid UTF-8 text")
	ErrRange            = errors.New("text: range outside visible text")
	ErrInvalidDelta     = errors.New("text: invalid RGA delta")
	// ErrIncompleteState indicates a state snapshot contains an unresolved
	// parent reference. Deltas may be partial for out-of-order delivery, but a
	// recoverable state frame must include the complete parent closure.
	ErrIncompleteState = errors.New("text: incomplete RGA state")
	ErrTagConflict     = errors.New("text: conflicting node for one tag")
	// ErrUnsafeCompaction means a tombstone still anchors local or unresolved
	// descendants, so removing it would change RGA ordering or permit a stale
	// insertion to become visible.
	ErrUnsafeCompaction = errors.New("text: unsafe RGA tombstone compaction")
	// ErrResourceLimit indicates that accepting a delta would exceed the
	// receiver's configured in-memory safety limits.
	ErrResourceLimit = errors.New("text: RGA resource limit exceeded")
	// ErrUndoAnchorGone indicates that a local undo/redo operation can no
	// longer preserve its original insertion intent because the structural
	// predecessor was compacted. Callers must clear local history after a
	// compaction boundary rather than silently placing content elsewhere.
	ErrUndoAnchorGone = errors.New("text: undo anchor is no longer retained")
)

const (
	// LegacySemanticsVersion is the immutable scalar RGA v1 contract for
	// TypeIDs 11/12. It is stable but remains a distinct protocol from run-v2.
	LegacySemanticsVersion uint64 = crdt.SemanticsVersionRGA
	// RunV2SemanticsVersion is the immutable semantics version for the stable
	// run-v2 text protocol (TypeIDs 19/20). It belongs in every run-v2 replica
	// manifest. Scalar-v1 frames must never be substituted for run-v2 frames.
	RunV2SemanticsVersion uint64 = crdt.SemanticsVersionRGARun
	// PackedV3SemanticsVersion is the separately negotiated compact RGA
	// protocol. It retains every scalar Position while packing dense local HLC
	// runs; v1 and run-v2 frames remain distinct protocols.
	PackedV3SemanticsVersion uint64 = crdt.SemanticsVersionRGAPacked
)

// Position is a stable, opaque identifier for one Unicode scalar value.
// It remains valid after inserts before it and after it has been deleted.
type Position = crdt.Tag

type node struct {
	parent Position
	rune   rune
}

// deltaPlan separates validation-time reachability from mutation. A fully
// resolved plan can bypass the pending indexes altogether; an incomplete plan
// still uses the bounded out-of-order path below.
type deltaPlan struct {
	nodes        map[Position]node
	children     map[Position][]Position
	roots        []Position
	pendingNodes int
	pendingBytes int
}

// resolvedRunFastPathMinNodes avoids sorting a small interactive edit only to
// save planning allocations that are already tiny. Larger text pastes and
// run-v2 deltas take the dedicated parent-before-child path below.
const resolvedRunFastPathMinNodes = 16

// childIndex stores the overwhelmingly common one-child RGA parent directly
// on its already-retained sequencePair. Only genuinely concurrent sibling
// sets allocate a map entry and slice. This keeps the ordering rule unchanged
// without adding a second per-character map to a locally inserted run.
type childIndex struct {
	branches map[Position][]*sequencePair
}

func newChildIndex() childIndex {
	return childIndex{
		branches: make(map[Position][]*sequencePair),
	}
}

func (index *childIndex) count(parent *sequencePair) int {
	if parent == nil {
		return 0
	}
	if siblings, exists := index.branches[parent.position]; exists {
		return len(siblings)
	}
	if parent.singleChild != nil {
		return 1
	}
	return 0
}

// insert records child in RGA's descending Position sibling order and returns
// its preceding sibling, if any. Callers use that sibling's exit marker as the
// insertion anchor for RGA depth-first order.
func (index *childIndex) insert(parent, child *sequencePair) (*sequencePair, bool) {
	if siblings, exists := index.branches[parent.position]; exists {
		insertAt := sort.Search(len(siblings), func(position int) bool {
			return siblings[position].position.Compare(child.position) < 0
		})
		if insertAt < len(siblings) && siblings[insertAt].position == child.position {
			if insertAt == 0 {
				return nil, false
			}
			return siblings[insertAt-1], true
		}
		var previous *sequencePair
		if insertAt > 0 {
			previous = siblings[insertAt-1]
		}
		siblings = append(siblings, nil)
		copy(siblings[insertAt+1:], siblings[insertAt:])
		siblings[insertAt] = child
		index.branches[parent.position] = siblings
		return previous, insertAt > 0
	}

	current := parent.singleChild
	if current == nil {
		parent.singleChild = child
		return nil, false
	}
	if current.position == child.position {
		return nil, false
	}
	parent.singleChild = nil
	if current.position.Compare(child.position) < 0 {
		index.branches[parent.position] = []*sequencePair{child, current}
		return nil, false
	}
	index.branches[parent.position] = []*sequencePair{current, child}
	return current, true
}

func (index *childIndex) remove(parent, child *sequencePair) bool {
	if siblings, exists := index.branches[parent.position]; exists {
		removeAt := sort.Search(len(siblings), func(position int) bool {
			return siblings[position].position.Compare(child.position) <= 0
		})
		if removeAt == len(siblings) || siblings[removeAt].position != child.position {
			return false
		}
		copy(siblings[removeAt:], siblings[removeAt+1:])
		siblings = siblings[:len(siblings)-1]
		switch len(siblings) {
		case 0:
			delete(index.branches, parent.position)
		case 1:
			delete(index.branches, parent.position)
			parent.singleChild = siblings[0]
		default:
			index.branches[parent.position] = siblings
		}
		return true
	}
	if parent.singleChild != nil && parent.singleChild.position == child.position {
		parent.singleChild = nil
		return true
	}
	return false
}

// removeSelected removes exactly candidates whose removed bit is true. It is
// used by batched tombstone compaction to keep wide sibling-set filtering
// linear without forcing singleton parents into slice allocations.
func (index *childIndex) removeSelected(parent *sequencePair, candidates map[Position]int, removed []bool) {
	if siblings, exists := index.branches[parent.position]; exists {
		retained := siblings[:0]
		for _, child := range siblings {
			candidateIndex, selected := candidates[child.position]
			if selected && removed[candidateIndex] {
				continue
			}
			retained = append(retained, child)
		}
		switch len(retained) {
		case 0:
			delete(index.branches, parent.position)
		case 1:
			delete(index.branches, parent.position)
			parent.singleChild = retained[0]
		default:
			index.branches[parent.position] = retained
		}
		return
	}
	child := parent.singleChild
	if child == nil {
		return
	}
	if candidateIndex, selected := candidates[child.position]; selected && removed[candidateIndex] {
		parent.singleChild = nil
	}
}

// eligibleCompactionPlan stores a child-before-parent deletion order and the
// membership index needed to rebuild affected child slices without one map per
// tombstone.
type eligibleCompactionPlan struct {
	tags             []Position
	candidateIndexes map[Position]int
}

// Delta is a joinable partial RGA state. Nodes and tombstones are deliberately
// opaque so a malformed delta cannot be assembled by direct field mutation.
type Delta struct {
	nodes      map[Position]node
	tombstones map[Position]struct{}
	// canonicalNodeIDs is populated only while constructing a large local
	// parent-before-child run. It is never decoded from peer input. ApplyDelta
	// rechecks its membership and ordering before using it, then falls back to
	// sorting the opaque map for every other delta.
	canonicalNodeIDs []Position
}

// Options bounds retained RGA metadata. Values must be positive. The defaults
// match the maximum element count of one default framed payload while keeping
// unresolved dependency state substantially smaller than a full document.
// Applications handling untrusted peers should choose limits appropriate to a
// replication group instead of relying on process-wide memory availability.
type Options struct {
	MaxNodes        int
	MaxTombstones   int
	MaxPendingNodes int
	MaxPendingBytes int
}

// DefaultOptions returns conservative per-RGA retention limits.
func DefaultOptions() Options {
	return Options{
		MaxNodes:        1 << 20,
		MaxTombstones:   1 << 20,
		MaxPendingNodes: 1 << 16,
		MaxPendingBytes: 4 << 20,
	}
}

func (o Options) valid() bool {
	return o.MaxNodes > 0 && o.MaxTombstones > 0 && o.MaxPendingNodes > 0 && o.MaxPendingBytes > 0
}

// RGA is a collaborative text CRDT. A tombstone retained for a deleted
// position wins even if it arrives before the corresponding insertion.
type RGA struct {
	mu        sync.RWMutex
	replicaID string
	clock     *clock.HLC
	options   Options
	// nodes contains only integrated nodes: every non-root parent is present
	// in nodes. pending holds nodes whose parent has not arrived. Splitting the
	// two makes delayed integration explicit, bounded, and snapshot-safe.
	nodes           map[Position]node
	pending         map[Position]node
	waitingByParent map[Position]map[Position]struct{}
	pendingBytes    int
	tombstones      map[Position]struct{}
	version         uint64
	sequence        *sequenceIndex
	children        childIndex
}

var _ crdt.CRDT[*RGA] = (*RGA)(nil)
var _ crdt.DeltaCapable[*RGA, Delta] = (*RGA)(nil)

// StableFrameType returns the stable run-v2 state/delta pair for new text
// replication groups. It is equivalent to crdt.DefaultRGAFrameType and is
// provided here so callers can bind the text package's semantic version and
// frame pair without treating legacy scalar-v1 helpers as the default.
func StableFrameType() crdt.FrameType { return crdt.DefaultRGAFrameType() }

// PackedFrameType returns the explicitly negotiated compact RGA v3 pair. It
// is not a fallback for stable run-v2: a replication group must bind this
// exact pair and semantics version in its authenticated manifest before using
// the packed encoder or decoder.
func PackedFrameType() crdt.FrameType {
	return crdt.FrameType{StateID: crdt.TypeIDRGAPackedState, DeltaID: crdt.TypeIDRGAPackedDelta, SemanticsVersion: PackedV3SemanticsVersion, UsesHLC: true}
}

// LegacyFrameType returns the stable scalar RGA v1 state/delta pair. It is a
// migration-compatible contract, not the default for new text groups.
func LegacyFrameType() crdt.FrameType {
	return crdt.FrameType{StateID: crdt.TypeIDRGAState, DeltaID: crdt.TypeIDRGADelta, SemanticsVersion: LegacySemanticsVersion, UsesHLC: true}
}

func New(replicaID string) (*RGA, error) { return NewWithOptions(replicaID, DefaultOptions()) }

// NewWithOptions constructs an RGA with explicit retention limits.
func NewWithOptions(replicaID string, options Options) (*RGA, error) {
	return NewFromClockWithOptions(clock.State{ReplicaID: replicaID}, options)
}

func NewFromClock(state clock.State) (*RGA, error) {
	return NewFromClockWithOptions(state, DefaultOptions())
}

// NewFromClockWithOptions restores an RGA clock with explicit retention
// limits. Clock state must be persisted atomically with a complete snapshot
// before reusing its replica ID.
func NewFromClockWithOptions(state clock.State, options Options) (*RGA, error) {
	if !(crdt.Tag{ReplicaID: state.ReplicaID}).Valid() {
		return nil, ErrInvalidReplicaID
	}
	if !options.valid() {
		return nil, ErrResourceLimit
	}
	hlc, err := clock.NewHLCFromState(state)
	if err != nil {
		return nil, err
	}
	return &RGA{
		replicaID:       state.ReplicaID,
		clock:           hlc,
		options:         options,
		nodes:           make(map[Position]node),
		pending:         make(map[Position]node),
		waitingByParent: make(map[Position]map[Position]struct{}),
		tombstones:      make(map[Position]struct{}),
		sequence:        newSequenceIndex(),
		children:        newChildIndex(),
	}, nil
}

func (r *RGA) ClockState() clock.State {
	if r == nil || r.clock == nil {
		return clock.State{}
	}
	return r.clock.Snapshot()
}

// RetainsPosition reports whether position is still retained as an integrated
// node or an out-of-order pending node. It intentionally differs from
// visibility: a deleted position remains an ordering anchor until a safe
// tombstone compaction removes it. Callers can use it to retire metadata that
// is attached to a position only after that compaction boundary.
func (r *RGA) RetainsPosition(position Position) bool {
	if r == nil || !position.Valid() {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.nodes[position]; ok {
		return true
	}
	_, ok := r.pending[position]
	return ok
}

type deltaEncoder func(Delta, frame.DecoderLimits) ([]byte, error)

// Insert inserts valid UTF-8 text before visible rune offset. It creates one
// node per Unicode scalar, so offset/count are rune based rather than byte
// based and can never split UTF-8.
func (r *RGA) Insert(offset int, value string) (Delta, error) {
	delta, _, err := r.insert(offset, value, nil, Delta.MarshalBinaryWithLimits)
	return delta, err
}

// insertAfter inserts value after an exact retained predecessor. It is used by
// the local undo manager to replay editing intent without treating a CRDT
// tombstone as reversible state. The predecessor may be deleted, because a
// retained tombstone remains an RGA ordering anchor; a compacted predecessor
// fails closed instead of falling back to an unrelated current offset.
func (r *RGA) insertAfter(predecessor Position, value string) (Delta, error) {
	if r == nil || r.clock == nil {
		return Delta{}, ErrNilText
	}
	if predecessor.Valid() {
		r.mu.RLock()
		_, retained := r.nodes[predecessor]
		r.mu.RUnlock()
		if !retained {
			return Delta{}, ErrUndoAnchorGone
		}
	}
	if !utf8.ValidString(value) {
		return Delta{}, ErrInvalidText
	}
	runes := []rune(value)
	delta := Delta{nodes: make(map[Position]node, len(runes)), tombstones: make(map[Position]struct{})}
	if len(runes) >= resolvedRunFastPathMinNodes {
		delta.canonicalNodeIDs = make([]Position, 0, len(runes))
	}
	parent := predecessor
	for _, valueRune := range runes {
		id, err := r.clock.Now()
		if err != nil {
			return Delta{}, err
		}
		delta.nodes[id] = node{parent: parent, rune: valueRune}
		if delta.canonicalNodeIDs != nil {
			delta.canonicalNodeIDs = append(delta.canonicalNodeIDs, id)
		}
		parent = id
	}
	if len(delta.nodes) == 0 {
		return delta, nil
	}
	if err := r.ApplyDelta(delta); err != nil {
		return Delta{}, err
	}
	delta.canonicalNodeIDs = nil
	return delta, nil
}

// InsertWithLimits inserts text only when its complete canonical delta fits
// limits. A rejected output frame does not add nodes or tombstones to the RGA,
// which lets a transport-facing caller fail the local edit before it becomes
// state that cannot be replicated under its own budget. The HLC may still
// advance while reserving local tags; those un-emitted tags are safe to skip.
func (r *RGA) InsertWithLimits(offset int, value string, limits frame.DecoderLimits) (Delta, error) {
	delta, _, err := r.insert(offset, value, &limits, Delta.MarshalBinaryWithLimits)
	return delta, err
}

// InsertBinaryWithLimits inserts text and returns the same preflighted
// canonical delta frame used to establish the local output budget.
func (r *RGA) InsertBinaryWithLimits(offset int, value string, limits frame.DecoderLimits) ([]byte, error) {
	_, encoded, err := r.insert(offset, value, &limits, Delta.MarshalBinaryWithLimits)
	return encoded, err
}

// InsertRunBinaryWithLimits inserts text and returns the same preflighted
// run-v2 delta frame used to establish the local output budget. Callers must
// have separately negotiated the run-v2 RGA protocol before using this frame.
func (r *RGA) InsertRunBinaryWithLimits(offset int, value string, limits frame.DecoderLimits) ([]byte, error) {
	_, encoded, err := r.insert(offset, value, &limits, Delta.MarshalRunBinaryWithLimits)
	return encoded, err
}

// InsertPackedBinaryWithLimits inserts text and returns a preflighted compact
// RGA v3 delta. Callers must negotiate PackedFrameType before publishing it.
func (r *RGA) InsertPackedBinaryWithLimits(offset int, value string, limits frame.DecoderLimits) ([]byte, error) {
	_, encoded, err := r.insert(offset, value, &limits, Delta.MarshalPackedBinaryWithLimits)
	return encoded, err
}

// InsertRunFrameV2WithLimits inserts text and returns the same preflighted
// run-v2 delta in a separately negotiated outer frame v2. The RGA payload is
// unchanged; the outer representation may use DEFLATE only when it reduces
// the complete bounded frame.
func (r *RGA) InsertRunFrameV2WithLimits(offset int, value string, limits frame.DecoderLimits) ([]byte, error) {
	_, encoded, err := r.insert(offset, value, &limits, Delta.MarshalRunFrameV2WithLimits)
	return encoded, err
}

// PrepareInsertRunBinaryWithLimits returns a canonical run-v2 insertion delta
// without adding its nodes to r. It reserves HLC tags, so a failed enclosing
// transaction may leave safely skipped tags in ClockState. Callers that need
// an atomic composed CRDT operation can encode and validate their outer frame
// before applying the returned delta with ApplyDelta.
func (r *RGA) PrepareInsertRunBinaryWithLimits(offset int, value string, limits frame.DecoderLimits) (Delta, []byte, error) {
	return r.prepareInsert(offset, value, &limits, Delta.MarshalRunBinaryWithLimits)
}

// PrepareInsertPackedBinaryWithLimits returns a preflighted compact RGA v3
// insertion without mutating r. Reserved HLC tags remain safe to skip if the
// caller abandons the surrounding transaction.
func (r *RGA) PrepareInsertPackedBinaryWithLimits(offset int, value string, limits frame.DecoderLimits) (Delta, []byte, error) {
	return r.prepareInsert(offset, value, &limits, Delta.MarshalPackedBinaryWithLimits)
}

// PrepareInsertRunFrameV2WithLimits returns a preflighted run-v2 insertion
// delta in a separately negotiated outer frame v2 without applying it.
func (r *RGA) PrepareInsertRunFrameV2WithLimits(offset int, value string, limits frame.DecoderLimits) (Delta, []byte, error) {
	return r.prepareInsert(offset, value, &limits, Delta.MarshalRunFrameV2WithLimits)
}

func (r *RGA) insert(offset int, value string, limits *frame.DecoderLimits, encode deltaEncoder) (Delta, []byte, error) {
	delta, encoded, err := r.prepareInsert(offset, value, limits, encode)
	if err != nil {
		return Delta{}, nil, err
	}
	if err := r.ApplyDelta(delta); err != nil {
		return Delta{}, nil, err
	}
	delta.canonicalNodeIDs = nil
	return delta, encoded, nil
}

func (r *RGA) prepareInsert(offset int, value string, limits *frame.DecoderLimits, encode deltaEncoder) (Delta, []byte, error) {
	if r == nil || r.clock == nil {
		return Delta{}, nil, ErrNilText
	}
	if offset < 0 {
		return Delta{}, nil, ErrRange
	}
	if !utf8.ValidString(value) {
		return Delta{}, nil, ErrInvalidText
	}
	runes := []rune(value)
	r.mu.RLock()
	previous, hasPrevious := r.sequence.visibleAt(offset - 1)
	visibleCount := visibleCount(r.sequence.root)
	r.mu.RUnlock()
	if offset > visibleCount {
		return Delta{}, nil, ErrRange
	}
	if len(runes) == 0 {
		empty := Delta{nodes: make(map[Position]node), tombstones: make(map[Position]struct{})}
		if limits == nil {
			return empty, nil, nil
		}
		encoded, err := encode(empty, *limits)
		if err != nil {
			return Delta{}, nil, err
		}
		return empty, encoded, nil
	}
	parent := Position{}
	if hasPrevious {
		parent = previous
	}
	delta := Delta{nodes: make(map[Position]node, len(runes)), tombstones: make(map[Position]struct{})}
	if len(runes) >= resolvedRunFastPathMinNodes {
		delta.canonicalNodeIDs = make([]Position, 0, len(runes))
	}
	for _, valueRune := range runes {
		id, err := r.clock.Now()
		if err != nil {
			return Delta{}, nil, err
		}
		delta.nodes[id] = node{parent: parent, rune: valueRune}
		if delta.canonicalNodeIDs != nil {
			delta.canonicalNodeIDs = append(delta.canonicalNodeIDs, id)
		}
		parent = id
	}
	var encoded []byte
	if limits != nil {
		var err error
		encoded, err = encode(delta, *limits)
		if err != nil {
			return Delta{}, nil, err
		}
	}
	return delta, encoded, nil
}

// Delete marks count visible runes starting at offset as removed. The delta
// carries only tombstones; replicas that have not received the inserts yet
// retain those tombstones until the matching nodes arrive.
func (r *RGA) Delete(offset, count int) (Delta, error) {
	delta, _, err := r.delete(offset, count, nil, Delta.MarshalBinaryWithLimits)
	return delta, err
}

// DeleteWithLimits deletes visible runes only when the canonical tombstone
// delta fits limits. A rejected output frame leaves the RGA content and
// tombstone set unchanged.
func (r *RGA) DeleteWithLimits(offset, count int, limits frame.DecoderLimits) (Delta, error) {
	delta, _, err := r.delete(offset, count, &limits, Delta.MarshalBinaryWithLimits)
	return delta, err
}

// DeleteBinaryWithLimits deletes visible text and returns the same preflighted
// canonical tombstone frame used to establish the local output budget.
func (r *RGA) DeleteBinaryWithLimits(offset, count int, limits frame.DecoderLimits) ([]byte, error) {
	_, encoded, err := r.delete(offset, count, &limits, Delta.MarshalBinaryWithLimits)
	return encoded, err
}

// DeleteRunBinaryWithLimits deletes visible text and returns the same
// preflighted run-v2 tombstone frame used to establish the local output
// budget. Callers must have separately negotiated the run-v2 RGA protocol.
func (r *RGA) DeleteRunBinaryWithLimits(offset, count int, limits frame.DecoderLimits) ([]byte, error) {
	_, encoded, err := r.delete(offset, count, &limits, Delta.MarshalRunBinaryWithLimits)
	return encoded, err
}

// DeletePackedBinaryWithLimits deletes visible text and returns the same
// preflighted compact RGA v3 tombstone delta used for the output budget.
func (r *RGA) DeletePackedBinaryWithLimits(offset, count int, limits frame.DecoderLimits) ([]byte, error) {
	_, encoded, err := r.delete(offset, count, &limits, Delta.MarshalPackedBinaryWithLimits)
	return encoded, err
}

// DeleteRunFrameV2WithLimits deletes visible text and returns the same
// preflighted run-v2 tombstone delta in a separately negotiated outer frame
// v2. Callers must bind frame.FormatVersionV2 in their manifest.
func (r *RGA) DeleteRunFrameV2WithLimits(offset, count int, limits frame.DecoderLimits) ([]byte, error) {
	_, encoded, err := r.delete(offset, count, &limits, Delta.MarshalRunFrameV2WithLimits)
	return encoded, err
}

// ReplaceBinaryWithLimits atomically replaces count visible runes at offset
// with value and returns one preflighted scalar-v1 delta. A rejected frame or
// state-limit check leaves both visible text and tombstones unchanged. The HLC
// may reserve un-emitted insertion tags, which are safe to skip.
func (r *RGA) ReplaceBinaryWithLimits(offset, count int, value string, limits frame.DecoderLimits) ([]byte, error) {
	encoded, err := r.replace(offset, count, value, limits, Delta.MarshalBinaryWithLimits)
	return encoded, err
}

// ReplaceRunBinaryWithLimits atomically replaces count visible runes at offset
// with value and returns one preflighted run-v2 delta. It is the editor-facing
// operation: a text binding must never publish a delete and insert half-pair
// when a local frame or retention bound rejects the replacement.
func (r *RGA) ReplaceRunBinaryWithLimits(offset, count int, value string, limits frame.DecoderLimits) ([]byte, error) {
	encoded, err := r.replace(offset, count, value, limits, Delta.MarshalRunBinaryWithLimits)
	return encoded, err
}

// ReplacePackedBinaryWithLimits atomically replaces visible text with one
// compact RGA v3 delta. Frame or retention rejection leaves r unchanged.
func (r *RGA) ReplacePackedBinaryWithLimits(offset, count int, value string, limits frame.DecoderLimits) ([]byte, error) {
	encoded, err := r.replace(offset, count, value, limits, Delta.MarshalPackedBinaryWithLimits)
	return encoded, err
}

// ReplaceRunFrameV2WithLimits atomically replaces visible text and returns
// one preflighted run-v2 delta in a separately negotiated outer frame v2.
func (r *RGA) ReplaceRunFrameV2WithLimits(offset, count int, value string, limits frame.DecoderLimits) ([]byte, error) {
	encoded, err := r.replace(offset, count, value, limits, Delta.MarshalRunFrameV2WithLimits)
	return encoded, err
}

func (r *RGA) replace(offset, count int, value string, limits frame.DecoderLimits, encode deltaEncoder) ([]byte, error) {
	if r == nil || r.clock == nil {
		return nil, ErrNilText
	}
	if !utf8.ValidString(value) {
		return nil, ErrInvalidText
	}
	deleted, _, err := r.prepareDelete(offset, count, nil, encode)
	if err != nil {
		return nil, err
	}
	inserted, _, err := r.prepareInsert(offset, value, nil, encode)
	if err != nil {
		return nil, err
	}
	delta := Delta{nodes: inserted.nodes, tombstones: deleted.tombstones, canonicalNodeIDs: inserted.canonicalNodeIDs}
	encoded, err := encode(delta, limits)
	if err != nil {
		return nil, err
	}
	if err := r.ApplyDelta(delta); err != nil {
		return nil, err
	}
	return encoded, nil
}

// PrepareDeleteRunBinaryWithLimits returns a canonical run-v2 deletion delta
// without applying its tombstones. It is intended for a composed CRDT that
// must preflight an enclosing frame before committing local text changes.
func (r *RGA) PrepareDeleteRunBinaryWithLimits(offset, count int, limits frame.DecoderLimits) (Delta, []byte, error) {
	return r.prepareDelete(offset, count, &limits, Delta.MarshalRunBinaryWithLimits)
}

// PrepareDeletePackedBinaryWithLimits returns a compact RGA v3 tombstone
// delta without mutating r.
func (r *RGA) PrepareDeletePackedBinaryWithLimits(offset, count int, limits frame.DecoderLimits) (Delta, []byte, error) {
	return r.prepareDelete(offset, count, &limits, Delta.MarshalPackedBinaryWithLimits)
}

// PrepareDeleteRunFrameV2WithLimits returns a preflighted run-v2 deletion
// delta in a separately negotiated outer frame v2 without applying it.
func (r *RGA) PrepareDeleteRunFrameV2WithLimits(offset, count int, limits frame.DecoderLimits) (Delta, []byte, error) {
	return r.prepareDelete(offset, count, &limits, Delta.MarshalRunFrameV2WithLimits)
}

func (r *RGA) delete(offset, count int, limits *frame.DecoderLimits, encode deltaEncoder) (Delta, []byte, error) {
	delta, encoded, err := r.prepareDelete(offset, count, limits, encode)
	if err != nil {
		return Delta{}, nil, err
	}
	if err := r.ApplyDelta(delta); err != nil {
		return Delta{}, nil, err
	}
	return delta, encoded, nil
}

func (r *RGA) prepareDelete(offset, count int, limits *frame.DecoderLimits, encode deltaEncoder) (Delta, []byte, error) {
	if r == nil {
		return Delta{}, nil, ErrNilText
	}
	if offset < 0 || count < 0 {
		return Delta{}, nil, ErrRange
	}
	r.mu.RLock()
	visibleCount := visibleCount(r.sequence.root)
	if offset > visibleCount || count > visibleCount-offset {
		r.mu.RUnlock()
		return Delta{}, nil, ErrRange
	}
	delta := Delta{nodes: make(map[Position]node), tombstones: make(map[Position]struct{}, count)}
	for index := 0; index < count; index++ {
		id, ok := r.sequence.visibleAt(offset + index)
		if !ok {
			r.mu.RUnlock()
			return Delta{}, nil, ErrRange
		}
		delta.tombstones[id] = struct{}{}
	}
	r.mu.RUnlock()
	var encoded []byte
	if limits != nil {
		var err error
		encoded, err = encode(delta, *limits)
		if err != nil {
			return Delta{}, nil, err
		}
	}
	return delta, encoded, nil
}

// NextTag reserves a local HLC tag without creating an RGA node. It exists
// for composed CRDTs that persist their formatting metadata with this RGA's
// clock state. A reserved but un-emitted tag is safe to skip.
func (r *RGA) NextTag() (Position, error) {
	if r == nil || r.clock == nil {
		return Position{}, ErrNilText
	}
	return r.clock.Now()
}

// WitnessTag advances the RGA clock beyond a remote composed-CRDT tag without
// changing text content. Callers must persist the resulting ClockState with
// their enclosing state before reusing the replica ID.
func (r *RGA) WitnessTag(tag Position) error {
	if r == nil || r.clock == nil {
		return ErrNilText
	}
	return r.clock.Witness(tag)
}

// NodePositions returns sorted stable IDs for the nodes carried by d. It does
// not expose their text or parent links, and is useful when an enclosing CRDT
// needs to attach metadata to an insertion before it applies the delta.
func (d Delta) NodePositions() []Position {
	positions := make([]Position, 0, len(d.nodes))
	for position := range d.nodes {
		positions = append(positions, position)
	}
	sortPositions(positions)
	return positions
}

func (r *RGA) String() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]rune, 0, visibleCount(r.sequence.root))
	for current := r.sequence.entry(Position{}).next; current != nil; current = current.next {
		if current.visible {
			id := current.pair.position
			result = append(result, r.nodes[id].rune)
		}
	}
	return string(result)
}

// Len returns the number of visible Unicode scalar values without allocating a
// projection. It is safe to call concurrently with other RGA operations.
func (r *RGA) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return visibleCount(r.sequence.root)
}

// VisibleRunes returns copies of the visible stable positions and their runes
// in display order from one consistent projection. Callers may modify either
// returned slice. It avoids the duplicated traversal required by separately
// calling Positions and String when both are needed by a renderer.
func (r *RGA) VisibleRunes() ([]Position, []rune) {
	if r == nil {
		return nil, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := visibleCount(r.sequence.root)
	positions := make([]Position, 0, count)
	runes := make([]rune, 0, count)
	for current := r.sequence.entry(Position{}).next; current != nil; current = current.next {
		if !current.visible {
			continue
		}
		positions = append(positions, current.pair.position)
		runes = append(runes, r.nodes[current.pair.position].rune)
	}
	return positions, runes
}

// Positions returns a copy of visible stable IDs in display order.
func (r *RGA) Positions() []Position {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sequence.visiblePositions()
}

func (r *RGA) ApplyDelta(delta Delta) error {
	if r == nil || r.clock == nil {
		return ErrNilText
	}
	if err := validateDelta(delta); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, incoming := range delta.nodes {
		if current, exists := r.nodes[id]; exists && current != incoming {
			return ErrTagConflict
		}
		if current, exists := r.pending[id]; exists && current != incoming {
			return ErrTagConflict
		}
	}
	// Re-delivery is common on at-least-once transports. Check under the same
	// write lock used by compaction so a retired leaf cannot be mistaken for
	// retained state while another goroutine changes the structural indexes.
	// Besides avoiding the planning work, this keeps duplicate frames from
	// needlessly advancing persisted HLC state.
	if r.subsumesLocked(delta) {
		return nil
	}
	// A local paste and a decoded run-v2 frame commonly describe one new,
	// same-replica parent chain. Once that is established against the retained
	// state, it is already acyclic and fully resolved. Avoid constructing the
	// generic dependency plan (maps for nodes, children, and reachability) for
	// this hot path. Any partial, branching, duplicate, or out-of-order delta
	// deliberately falls through to the conservative planner.
	if ids, ok := r.resolvedLinearRunLocked(delta); ok {
		return r.applyResolvedLinearRunLocked(delta, ids)
	}
	// Integrated nodes have complete, previously validated parent chains. Only
	// a pending node can still point at an ID introduced by this delta, so
	// copying or traversing the full live document is unnecessary here.
	if !acyclicAgainst(delta.nodes, r.pending) {
		return ErrInvalidDelta
	}
	plan := r.planDelta(delta)
	if len(r.nodes)+len(r.pending)+len(plan.nodes) > r.options.MaxNodes ||
		len(r.pending)+plan.pendingNodes > r.options.MaxPendingNodes ||
		r.pendingBytes+plan.pendingBytes > r.options.MaxPendingBytes ||
		len(r.tombstones)+newTombstones(delta.tombstones, r.tombstones) > r.options.MaxTombstones {
		return ErrResourceLimit
	}
	if tag, ok := greatestTag(delta); ok {
		if err := r.clock.Witness(tag); err != nil {
			return err
		}
	}
	changed := r.integrateResolved(plan)
	if plan.pendingNodes > 0 {
		for id, incoming := range plan.nodes {
			r.enqueuePending(id, incoming)
		}
		changed = len(plan.nodes) > 0
	}
	if r.integrateReady() {
		changed = true
	}
	if r.applyTombstonesLocked(delta.tombstones) {
		changed = true
	}
	if changed {
		r.version++
	}
	return nil
}

// resolvedLinearRunLocked recognizes a complete, new, same-replica chain in
// canonical tag order. It is intentionally strict: accepting only a root or
// retained initial parent means no pending edge can participate in the fast
// path. The caller must hold r.mu for writing.
func (r *RGA) resolvedLinearRunLocked(delta Delta) ([]Position, bool) {
	if len(delta.nodes) < resolvedRunFastPathMinNodes {
		return nil, false
	}
	ids, cached := delta.cachedCanonicalNodeIDs()
	if !cached {
		ids = sortedNodeIDs(delta.nodes)
	}
	first := delta.nodes[ids[0]]
	if first.parent.Valid() {
		if _, exists := r.nodes[first.parent]; !exists {
			return nil, false
		}
	}
	replicaID := ids[0].ReplicaID
	for index, id := range ids {
		if id.ReplicaID != replicaID {
			return nil, false
		}
		if _, exists := r.nodes[id]; exists {
			return nil, false
		}
		if _, exists := r.pending[id]; exists {
			return nil, false
		}
		if index > 0 && delta.nodes[id].parent != ids[index-1] {
			return nil, false
		}
	}
	return ids, true
}

// cachedCanonicalNodeIDs returns the locally recorded canonical ordering only
// after checking that it still covers exactly the immutable opaque node map.
// A malformed or stale cache cannot alter RGA ordering: the caller sorts the
// map instead.
func (d Delta) cachedCanonicalNodeIDs() ([]Position, bool) {
	ids := d.canonicalNodeIDs
	if len(ids) == 0 || len(ids) != len(d.nodes) {
		return nil, false
	}
	for index, id := range ids {
		if index > 0 && id.Compare(ids[index-1]) <= 0 {
			return nil, false
		}
		if _, exists := d.nodes[id]; !exists {
			return nil, false
		}
	}
	return ids, true
}

// applyResolvedLinearRunLocked integrates a chain accepted by
// resolvedLinearRunLocked. It preserves ApplyDelta's validation, limits, HLC
// witness, pending replay, tombstone, and version semantics while avoiding
// the transient generic delta-plan graph. The caller must hold r.mu.
func (r *RGA) applyResolvedLinearRunLocked(delta Delta, ids []Position) error {
	if len(r.nodes)+len(r.pending)+len(ids) > r.options.MaxNodes ||
		len(r.tombstones)+newTombstones(delta.tombstones, r.tombstones) > r.options.MaxTombstones {
		return ErrResourceLimit
	}
	if tag, ok := greatestTag(delta); ok {
		if err := r.clock.Witness(tag); err != nil {
			return err
		}
	}
	if len(r.nodes) == 0 {
		// The fast path is often the first state installed in a fresh replica.
		// Reserve both indexes after all rejectable checks above, avoiding the
		// repeated map growth otherwise caused by a large paste or initial sync.
		r.nodes = make(map[Position]node, len(ids))
		r.sequence.reserveInitialPairs(len(ids))
	}
	// Every pair is retained by sequence.pairs and its marker tree, so allocate
	// a resolved run as one backing array instead of one heap object per rune.
	// This path is only reached after resolvedLinearRunLocked has established
	// the parent-before-child chain and canonical order.
	pairs := make([]sequencePair, len(ids))
	markers := make([]*sequenceMarker, 0, len(ids)*2)
	for index, id := range ids {
		_, deleted := r.tombstones[id]
		initializeSequencePair(&pairs[index], id, !deleted)
		markers = append(markers, &pairs[index].entry)
	}
	for index := len(ids) - 1; index >= 0; index-- {
		markers = append(markers, &markers[index].pair.exit)
	}
	var anchor *sequenceMarker
	for index, id := range ids {
		item := delta.nodes[id]
		r.nodes[id] = item
		parent := r.sequence.pair(item.parent)
		if index > 0 {
			parent = markers[index-1].pair
		}
		if parent == nil {
			panic("text: integrating resolved linear run without integrated parent")
		}
		previous, hasPrevious := r.children.insert(parent, markers[index].pair)
		if index == 0 {
			anchor = &parent.entry
			if hasPrevious {
				anchor = &previous.exit
			}
		}
	}
	r.sequence.insertLinearMarkersAfter(anchor, markers, len(ids))
	changed := len(ids) > 0
	if r.integrateReady() {
		changed = true
	}
	if r.applyTombstonesLocked(delta.tombstones) {
		changed = true
	}
	if changed {
		r.version++
	}
	return nil
}

// applyTombstonesLocked retains each new tombstone and hides its node when it
// is already integrated. A tombstone for a missing node remains retained so a
// later out-of-order insert is hidden on integration. The caller must hold
// r.mu for writing.
func (r *RGA) applyTombstonesLocked(tombstones map[Position]struct{}) bool {
	changed := false
	for id := range tombstones {
		if _, exists := r.tombstones[id]; exists {
			continue
		}
		r.tombstones[id] = struct{}{}
		r.sequence.setVisible(id, false)
		changed = true
	}
	return changed
}

func (r *RGA) Merge(other *RGA) error {
	if r == nil || other == nil {
		return ErrNilText
	}
	if r == other {
		return nil
	}
	other.mu.RLock()
	delta := Delta{nodes: cloneNodes(other.nodes), tombstones: cloneTombstones(other.tombstones)}
	for id, item := range other.pending {
		delta.nodes[id] = item
	}
	other.mu.RUnlock()
	return r.ApplyDelta(delta)
}

// PendingCount reports the number of accepted nodes still waiting for a
// missing parent. It is useful for replication diagnostics and backpressure.
func (r *RGA) PendingCount() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.pending)
}

// MissingParents returns stable IDs that must arrive before pending nodes can
// integrate. The returned slice is sorted and safe for callers to retain.
func (r *RGA) MissingParents() []Position {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	parents := make([]Position, 0, len(r.waitingByParent))
	for parent := range r.waitingByParent {
		if parent.Valid() && len(r.waitingByParent[parent]) > 0 {
			parents = append(parents, parent)
		}
	}
	r.mu.RUnlock()
	sort.Slice(parents, func(i, j int) bool { return parents[i].Compare(parents[j]) < 0 })
	return parents
}

// TombstoneTags returns every retained deletion tag in canonical order. It is
// an acknowledgement input, not proof that a tag is safe to collect.
func (r *RGA) TombstoneTags() []Position {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedTombstoneIDs(r.tombstones)
}

// CompactTombstones physically removes exactly the requested tombstoned leaf
// nodes. It is deliberately stricter than OR-Set compaction: an RGA deletion
// remains a structural anchor while any integrated or pending child refers to
// it. Call this only after an authenticated, exact-acknowledgement epoch has
// durably checkpointed a post-compaction snapshot and retired old deltas.
//
// The operation is all-or-nothing. Unknown tags are ignored; invalid tags,
// unresolved dependencies, non-leaf nodes, or tombstones received before
// their insertion return ErrUnsafeCompaction without changing the RGA.
func (r *RGA) CompactTombstones(tags []Position) (int, error) {
	if r == nil {
		return 0, ErrNilText
	}
	for _, tag := range tags {
		if !tag.Valid() {
			return 0, ErrUnsafeCompaction
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) > 0 {
		return 0, ErrUnsafeCompaction
	}
	compact := make([]Position, 0, len(tags))
	seen := make(map[Position]struct{}, len(tags))
	for _, tag := range tags {
		if _, duplicate := seen[tag]; duplicate {
			continue
		}
		seen[tag] = struct{}{}
		if _, tombstoned := r.tombstones[tag]; !tombstoned {
			continue
		}
		if _, exists := r.nodes[tag]; !exists || r.children.count(r.sequence.pair(tag)) > 0 || len(r.waitingByParent[tag]) > 0 || !r.sequence.has(tag) {
			return 0, ErrUnsafeCompaction
		}
		compact = append(compact, tag)
	}
	for _, tag := range compact {
		item := r.nodes[tag]
		if !r.children.remove(r.sequence.pair(item.parent), r.sequence.pair(tag)) {
			return 0, ErrUnsafeCompaction
		}
		if !r.sequence.removeLeaf(tag) {
			return 0, ErrUnsafeCompaction
		}
		delete(r.nodes, tag)
		delete(r.tombstones, tag)
	}
	if len(compact) > 0 {
		r.version++
	}
	return len(compact), nil
}

// CompactEligibleTombstones makes best-effort structural progress through an
// exact-acknowledged batch. Unlike CompactTombstones, a non-leaf in tags does
// not block unrelated leaves. Deleted descendants are removed before their
// deleted ancestors, so a fully deleted chain can compact in one call.
//
// It remains deliberately fail-closed for unresolved state: any pending node
// returns ErrUnsafeCompaction without changing the RGA. Callers still need an
// authenticated exact-acknowledgement epoch, a durable post-compaction
// checkpoint, and retirement of old deltas before using this method.
func (r *RGA) CompactEligibleTombstones(tags []Position) (int, error) {
	if r == nil {
		return 0, ErrNilText
	}
	for _, tag := range tags {
		if !tag.Valid() {
			return 0, ErrUnsafeCompaction
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) > 0 {
		return 0, ErrUnsafeCompaction
	}
	plan := r.eligibleCompactionPlanLocked(tags)
	if len(plan.tags) == 0 {
		return 0, nil
	}
	if !r.applyEligibleCompactionPlanLocked(plan) {
		return 0, ErrUnsafeCompaction
	}
	r.version++
	return len(plan.tags), nil
}

// eligibleCompactionPlanLocked returns a child-before-parent removal order.
// The caller must hold r.mu and have rejected unresolved state.
func (r *RGA) eligibleCompactionPlanLocked(tags []Position) eligibleCompactionPlan {
	candidateIndexes := make(map[Position]int, len(tags))
	candidates := make([]Position, 0, len(tags))
	for _, tag := range tags {
		if _, duplicate := candidateIndexes[tag]; duplicate || !r.eligibleCompactionCandidateLocked(tag) {
			continue
		}
		candidateIndexes[tag] = len(candidates)
		candidates = append(candidates, tag)
	}
	if len(candidates) == 0 {
		return eligibleCompactionPlan{}
	}
	sortPositions(candidates)
	for index, tag := range candidates {
		candidateIndexes[tag] = index
	}

	remainingChildren := make([]int, len(candidates))
	ready := make([]int, 0, len(candidates))
	for index, tag := range candidates {
		remainingChildren[index] = r.children.count(r.sequence.pair(tag))
		if remainingChildren[index] == 0 {
			ready = append(ready, index)
		}
	}

	compact := make([]Position, 0, len(candidates))
	for len(ready) > 0 {
		index := ready[0]
		ready = ready[1:]
		tag := candidates[index]
		compact = append(compact, tag)
		parent := r.nodes[tag].parent
		parentIndex, selectedParent := candidateIndexes[parent]
		if !selectedParent {
			continue
		}
		remainingChildren[parentIndex]--
		if remainingChildren[parentIndex] == 0 {
			ready = append(ready, parentIndex)
		}
	}
	return eligibleCompactionPlan{tags: compact, candidateIndexes: candidateIndexes}
}

func (r *RGA) eligibleCompactionCandidateLocked(tag Position) bool {
	if _, tombstoned := r.tombstones[tag]; !tombstoned {
		return false
	}
	if _, exists := r.nodes[tag]; !exists || len(r.waitingByParent[tag]) > 0 {
		return false
	}
	return r.sequence.has(tag)
}

// applyEligibleCompactionPlanLocked removes a plan made by
// eligibleCompactionPlanLocked. It batches child-slice filtering so a wide
// sibling set is compacted in linear rather than repeated-shift time.
func (r *RGA) applyEligibleCompactionPlanLocked(plan eligibleCompactionPlan) bool {
	removed := make([]bool, len(plan.candidateIndexes))
	affectedParents := make(map[Position]*sequencePair, len(plan.tags))
	for _, tag := range plan.tags {
		item, exists := r.nodes[tag]
		if !exists {
			return false
		}
		removed[plan.candidateIndexes[tag]] = true
		affectedParents[item.parent] = r.sequence.pair(item.parent)
	}
	for _, tag := range plan.tags {
		if !r.sequence.removeLeaf(tag) {
			return false
		}
		delete(r.nodes, tag)
		delete(r.tombstones, tag)
	}
	for _, parent := range affectedParents {
		r.children.removeSelected(parent, plan.candidateIndexes, removed)
	}
	return true
}

func (d Delta) Merge(other Delta) (Delta, error) {
	if err := validateDelta(d); err != nil {
		return Delta{}, err
	}
	if err := validateDelta(other); err != nil {
		return Delta{}, err
	}
	merged := Delta{nodes: cloneNodes(d.nodes), tombstones: cloneTombstones(d.tombstones)}
	for id, incoming := range other.nodes {
		if current, exists := merged.nodes[id]; exists && current != incoming {
			return Delta{}, ErrTagConflict
		}
		merged.nodes[id] = incoming
	}
	for id := range other.tombstones {
		merged.tombstones[id] = struct{}{}
	}
	return merged, nil
}

func (r *RGA) State() crdt.StateSnapshot {
	if r == nil {
		return crdt.StateSnapshot{Type: "rga-text"}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return crdt.StateSnapshot{Type: "rga-text", ReplicaID: r.replicaID, ElementCount: visibleCount(r.sequence.root), TombstoneCount: len(r.tombstones)}
}

func (r *RGA) enqueuePending(id Position, item node) {
	r.pending[id] = item
	if r.waitingByParent[item.parent] == nil {
		r.waitingByParent[item.parent] = make(map[Position]struct{})
	}
	r.waitingByParent[item.parent][id] = struct{}{}
	r.pendingBytes += nodeBytes(id, item)
}

// integrateResolved adds a complete batch in parent-before-child order without
// allocating transient pending indexes. The caller must pass a plan with no
// unresolved nodes; planDelta establishes that invariant before state changes.
func (r *RGA) integrateResolved(plan deltaPlan) bool {
	if len(plan.nodes) == 0 || plan.pendingNodes != 0 {
		return false
	}
	ready := plan.roots
	for index := 0; index < len(ready); index++ {
		id := ready[index]
		item := plan.nodes[id]
		r.nodes[id] = item
		r.integrateNode(id, item)
		ready = append(ready, plan.children[id]...)
	}
	return true
}

// integrateReady drains every root-reachable pending node iteratively. Child
// IDs are sorted before scheduling so dependency replay is deterministic and
// never depends on Go map iteration order.
func (r *RGA) integrateReady() bool {
	parents := make([]Position, 0, len(r.waitingByParent))
	for parent := range r.waitingByParent {
		if !parent.Valid() {
			parents = append(parents, parent)
			continue
		}
		if _, ok := r.nodes[parent]; ok {
			parents = append(parents, parent)
		}
	}
	sort.Slice(parents, func(i, j int) bool { return parents[i].Compare(parents[j]) < 0 })
	ready := make([]Position, 0)
	for _, parent := range parents {
		ready = append(ready, sortedWaitingIDs(r.waitingByParent[parent])...)
	}
	changed := false
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		item, ok := r.pending[id]
		if !ok {
			continue
		}
		delete(r.pending, id)
		r.pendingBytes -= nodeBytes(id, item)
		delete(r.waitingByParent[item.parent], id)
		if len(r.waitingByParent[item.parent]) == 0 {
			delete(r.waitingByParent, item.parent)
		}
		r.nodes[id] = item
		r.integrateNode(id, item)
		changed = true
		ready = append(ready, sortedWaitingIDs(r.waitingByParent[id])...)
	}
	return changed
}

func (r *RGA) integrateNode(id Position, item node) {
	parent := r.sequence.pair(item.parent)
	if parent == nil {
		panic("text: integrating node without integrated parent")
	}
	_, deleted := r.tombstones[id]
	pair := newSequencePair(id, !deleted)
	previous, hasPrevious := r.children.insert(parent, pair)
	anchor := &parent.entry
	if hasPrevious {
		anchor = &previous.exit
	}
	r.sequence.insertPairAfter(anchor, pair)
}

func buildSequence(nodes map[Position]node, tombstones map[Position]struct{}) (*sequenceIndex, childIndex, error) {
	value := &RGA{
		nodes:      nodes,
		tombstones: tombstones,
		sequence:   newSequenceIndex(),
		children:   newChildIndex(),
	}
	byParent := make(map[Position][]Position, len(nodes))
	for id, item := range nodes {
		byParent[item.parent] = append(byParent[item.parent], id)
	}
	for parent := range byParent {
		sort.Slice(byParent[parent], func(i, j int) bool { return byParent[parent][i].Compare(byParent[parent][j]) < 0 })
	}
	ready := append([]Position(nil), byParent[Position{}]...)
	integrated := 0
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		item, ok := nodes[id]
		if !ok || value.sequence.entry(item.parent) == nil {
			return nil, childIndex{}, ErrInvalidDelta
		}
		value.integrateNode(id, item)
		integrated++
		ready = append(ready, byParent[id]...)
	}
	if integrated != len(nodes) {
		return nil, childIndex{}, ErrInvalidDelta
	}
	return value.sequence, value.children, nil
}

func sortedWaitingIDs(entries map[Position]struct{}) []Position {
	ids := make([]Position, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Compare(ids[j]) < 0 })
	return ids
}

func (r *RGA) planDelta(delta Delta) deltaPlan {
	plan := deltaPlan{nodes: make(map[Position]node, len(delta.nodes))}
	for id, item := range delta.nodes {
		if _, exists := r.nodes[id]; exists {
			continue
		}
		if _, exists := r.pending[id]; exists {
			continue
		}
		plan.nodes[id] = item
	}
	// Discover every new node that can integrate using only existing integrated
	// parents plus parents in this delta. This makes one large local paste a
	// single resolved batch rather than falsely charging every later character
	// against the pending limit.
	plan.children = make(map[Position][]Position, len(plan.nodes))
	plan.roots = make([]Position, 0, len(plan.nodes))
	for id, item := range plan.nodes {
		plan.children[item.parent] = append(plan.children[item.parent], id)
		if !item.parent.Valid() {
			plan.roots = append(plan.roots, id)
			continue
		}
		if _, exists := r.nodes[item.parent]; exists {
			plan.roots = append(plan.roots, id)
		}
	}
	ready := plan.roots
	resolved := make(map[Position]struct{}, len(ready))
	for index := 0; index < len(ready); index++ {
		id := ready[index]
		if _, seen := resolved[id]; seen {
			continue
		}
		resolved[id] = struct{}{}
		ready = append(ready, plan.children[id]...)
	}
	for id, item := range plan.nodes {
		if _, ok := resolved[id]; !ok {
			plan.pendingNodes++
			plan.pendingBytes += nodeBytes(id, item)
		}
	}
	if plan.pendingNodes == 0 {
		sortPositions(plan.roots)
		for parent, children := range plan.children {
			sortPositions(children)
			plan.children[parent] = children
		}
	}
	return plan
}

// subsumesLocked reports whether every part of delta is already retained by
// this RGA. The caller must hold r.mu for writing: tombstone compaction can
// otherwise remove a node or tombstone between this check and return.
func (r *RGA) subsumesLocked(delta Delta) bool {
	for id, incoming := range delta.nodes {
		if current, exists := r.nodes[id]; exists && current == incoming {
			continue
		}
		if current, exists := r.pending[id]; exists && current == incoming {
			continue
		}
		return false
	}
	for id := range delta.tombstones {
		if _, exists := r.tombstones[id]; !exists {
			return false
		}
	}
	return true
}

func sortPositions(ids []Position) {
	sort.Slice(ids, func(i, j int) bool { return ids[i].Compare(ids[j]) < 0 })
}

func newTombstones(incoming, existing map[Position]struct{}) int {
	count := 0
	for id := range incoming {
		if _, ok := existing[id]; !ok {
			count++
		}
	}
	return count
}

func nodeBytes(id Position, item node) int {
	// Account for IDs, links, rune, and map entries conservatively enough to
	// apply backpressure without depending on Go runtime internals.
	return 64 + len(id.ReplicaID) + len(item.parent.ReplicaID)
}

func validateDelta(delta Delta) error {
	for id, item := range delta.nodes {
		if !id.Valid() || !utf8.ValidRune(item.rune) || (item.parent != (Position{}) && !item.parent.Valid()) || id == item.parent {
			return ErrInvalidDelta
		}
	}
	for id := range delta.tombstones {
		if !id.Valid() {
			return ErrInvalidDelta
		}
	}
	return nil
}

// acyclicAgainst permits a parent that has not arrived yet (out-of-order
// delivery), but rejects every cycle formed by incoming and already pending
// nodes. Integrated nodes are deliberately not traversed: their parent closure
// and acyclicity were established before integration, so they cannot close a
// new cycle. A tri-colour walk visits each relevant node at most once.
func acyclicAgainst(incoming, pending map[Position]node) bool {
	lookup := func(id Position) (node, bool) {
		if item, ok := incoming[id]; ok {
			return item, true
		}
		item, ok := pending[id]
		return item, ok
	}
	const (
		unseen uint8 = iota
		visiting
		complete
	)
	state := make(map[Position]uint8, len(incoming)+len(pending))
	for id := range incoming {
		if state[id] == complete {
			continue
		}
		path := make([]Position, 0)
		current := id
		for current.Valid() {
			switch state[current] {
			case visiting:
				return false
			case complete:
				current = Position{}
				continue
			}
			next, known := lookup(current)
			if !known {
				break
			}
			state[current] = visiting
			path = append(path, current)
			current = next.parent
		}
		for _, visited := range path {
			state[visited] = complete
		}
	}
	return true
}

func greatestTag(delta Delta) (Position, bool) {
	var greatest Position
	ok := false
	for id, item := range delta.nodes {
		if !ok || greatest.Compare(id) < 0 {
			greatest, ok = id, true
		}
		if item.parent.Valid() && greatest.Compare(item.parent) < 0 {
			greatest = item.parent
		}
	}
	for id := range delta.tombstones {
		if !ok || greatest.Compare(id) < 0 {
			greatest, ok = id, true
		}
	}
	return greatest, ok
}
func cloneNodes(source map[Position]node) map[Position]node {
	out := make(map[Position]node, len(source))
	for id, item := range source {
		out[id] = item
	}
	return out
}
func cloneTombstones(source map[Position]struct{}) map[Position]struct{} {
	out := make(map[Position]struct{}, len(source))
	for id := range source {
		out[id] = struct{}{}
	}
	return out
}
