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

// Delta is a joinable partial RGA state. Nodes and tombstones are deliberately
// opaque so a malformed delta cannot be assembled by direct field mutation.
type Delta struct {
	nodes      map[Position]node
	tombstones map[Position]struct{}
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
	children        map[Position][]Position
}

var _ crdt.CRDT[*RGA] = (*RGA)(nil)
var _ crdt.DeltaCapable[*RGA, Delta] = (*RGA)(nil)

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
		children:        make(map[Position][]Position),
	}, nil
}

func (r *RGA) ClockState() clock.State {
	if r == nil || r.clock == nil {
		return clock.State{}
	}
	return r.clock.Snapshot()
}

// Insert inserts valid UTF-8 text before visible rune offset. It creates one
// node per Unicode scalar, so offset/count are rune based rather than byte
// based and can never split UTF-8.
func (r *RGA) Insert(offset int, value string) (Delta, error) {
	if r == nil || r.clock == nil {
		return Delta{}, ErrNilText
	}
	if offset < 0 {
		return Delta{}, ErrRange
	}
	if !utf8.ValidString(value) {
		return Delta{}, ErrInvalidText
	}
	runes := []rune(value)
	r.mu.RLock()
	previous, hasPrevious := r.sequence.visibleAt(offset - 1)
	visibleCount := visibleCount(r.sequence.root)
	r.mu.RUnlock()
	if offset > visibleCount {
		return Delta{}, ErrRange
	}
	if len(runes) == 0 {
		return Delta{nodes: make(map[Position]node), tombstones: make(map[Position]struct{})}, nil
	}
	parent := Position{}
	if hasPrevious {
		parent = previous
	}
	delta := Delta{nodes: make(map[Position]node, len(runes)), tombstones: make(map[Position]struct{})}
	for _, valueRune := range runes {
		id, err := r.clock.Now()
		if err != nil {
			return Delta{}, err
		}
		delta.nodes[id] = node{parent: parent, rune: valueRune}
		parent = id
	}
	if err := r.ApplyDelta(delta); err != nil {
		return Delta{}, err
	}
	return delta, nil
}

// Delete marks count visible runes starting at offset as removed. The delta
// carries only tombstones; replicas that have not received the inserts yet
// retain those tombstones until the matching nodes arrive.
func (r *RGA) Delete(offset, count int) (Delta, error) {
	if r == nil {
		return Delta{}, ErrNilText
	}
	if offset < 0 || count < 0 {
		return Delta{}, ErrRange
	}
	r.mu.RLock()
	visibleCount := visibleCount(r.sequence.root)
	if offset > visibleCount || count > visibleCount-offset {
		r.mu.RUnlock()
		return Delta{}, ErrRange
	}
	delta := Delta{nodes: make(map[Position]node), tombstones: make(map[Position]struct{}, count)}
	for index := 0; index < count; index++ {
		id, ok := r.sequence.visibleAt(offset + index)
		if !ok {
			r.mu.RUnlock()
			return Delta{}, ErrRange
		}
		delta.tombstones[id] = struct{}{}
	}
	r.mu.RUnlock()
	if err := r.ApplyDelta(delta); err != nil {
		return Delta{}, err
	}
	return delta, nil
}

func (r *RGA) String() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]rune, 0, visibleCount(r.sequence.root))
	for current := r.sequence.entries[Position{}].next; current != nil; current = current.next {
		if current.visible {
			id := current.position
			result = append(result, r.nodes[id].rune)
		}
	}
	return string(result)
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
	for id := range delta.tombstones {
		if _, exists := r.tombstones[id]; exists {
			continue
		}
		r.tombstones[id] = struct{}{}
		r.sequence.setVisible(id, false)
		changed = true
	}
	if changed {
		r.version++
	}
	return nil
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
		if _, exists := r.nodes[tag]; !exists || len(r.children[tag]) > 0 || len(r.waitingByParent[tag]) > 0 || r.sequence.entries[tag] == nil || r.sequence.exits[tag] == nil {
			return 0, ErrUnsafeCompaction
		}
		compact = append(compact, tag)
	}
	for _, tag := range compact {
		item := r.nodes[tag]
		siblings := r.children[item.parent]
		for index, candidate := range siblings {
			if candidate == tag {
				copy(siblings[index:], siblings[index+1:])
				siblings = siblings[:len(siblings)-1]
				break
			}
		}
		if len(siblings) == 0 {
			delete(r.children, item.parent)
		} else {
			r.children[item.parent] = siblings
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
	siblings := r.children[item.parent]
	insertAt := sort.Search(len(siblings), func(index int) bool {
		return siblings[index].Compare(id) < 0
	})
	anchor := r.sequence.entries[item.parent]
	if insertAt > 0 {
		anchor = r.sequence.exits[siblings[insertAt-1]]
	}
	if anchor == nil {
		panic("text: integrating node without integrated parent")
	}
	siblings = append(siblings, Position{})
	copy(siblings[insertAt+1:], siblings[insertAt:])
	siblings[insertAt] = id
	r.children[item.parent] = siblings
	_, deleted := r.tombstones[id]
	r.sequence.insertAfter(anchor, id, !deleted)
}

func buildSequence(nodes map[Position]node, tombstones map[Position]struct{}) (*sequenceIndex, map[Position][]Position, error) {
	value := &RGA{
		nodes:      nodes,
		tombstones: tombstones,
		sequence:   newSequenceIndex(),
		children:   make(map[Position][]Position),
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
		if !ok || value.sequence.entries[item.parent] == nil {
			return nil, nil, ErrInvalidDelta
		}
		value.integrateNode(id, item)
		integrated++
		ready = append(ready, byParent[id]...)
	}
	if integrated != len(nodes) {
		return nil, nil, ErrInvalidDelta
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
