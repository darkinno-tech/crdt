// Package tree implements an observed-remove rooted tree CRDT.
package tree

import (
	"bytes"
	"errors"
	"sort"
	"sync"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/clock"
)

var (
	ErrInvalidReplicaID = errors.New("tree: invalid replica ID")
	ErrNilTree          = errors.New("tree: nil OR-Tree")
	ErrUnknownParent    = errors.New("tree: unknown live parent")
	ErrUnknownNode      = errors.New("tree: unknown live node")
	ErrInvalidDelta     = errors.New("tree: invalid delta")
	ErrIncompleteState  = errors.New("tree: incomplete OR-Tree state")
	ErrNodeConflict     = errors.New("tree: conflicting node identity")
	ErrResourceLimit    = errors.New("tree: OR-Tree resource limit exceeded")
	ErrUnsafeCompaction = errors.New("tree: unsafe OR-Tree tombstone compaction")
)

// SemanticsVersion is the immutable observed-remove tree v1 contract. It must
// match the value negotiated in a replica manifest.
const SemanticsVersion uint64 = crdt.SemanticsVersionORTree

// NodeID is an immutable node-instance identity. The zero ID is the synthetic root.
type NodeID = crdt.Tag

// Node is an immutable visible tree node. Value is always caller-owned.
type Node struct {
	ID     NodeID
	Parent NodeID
	Value  []byte
}
type storedNode struct {
	parent NodeID
	value  []byte
}
type Delta struct {
	nodes      map[NodeID]storedNode
	tombstones map[NodeID]struct{}
}

// Options bounds retained OR-Tree state. Applications handling untrusted
// peers should set limits for the replication group rather than rely on
// process-wide memory availability.
type Options struct {
	MaxNodes      int
	MaxTombstones int
	MaxValueBytes int
}

// DefaultOptions returns conservative retention limits that align with one
// default frame's element and value limits.
func DefaultOptions() Options {
	return Options{MaxNodes: 1 << 20, MaxTombstones: 1 << 20, MaxValueBytes: 1 << 20}
}

func (o Options) valid() bool {
	return o.MaxNodes > 0 && o.MaxTombstones > 0 && o.MaxValueBytes > 0
}

// ORTree supports add and observed-remove. Moving a node is deliberately not
// an in-place operation: remove it and add a new instance under the new parent.
type ORTree struct {
	mu         sync.RWMutex
	replicaID  string
	clock      *clock.HLC
	options    Options
	nodes      map[NodeID]storedNode
	tombstones map[NodeID]struct{}
	version    uint64

	// visibleCount is a versioned cache. Computing visibility requires walking
	// the rooted projection, so State avoids repeating that work between writes.
	visibleCount        int
	visibleCountVersion uint64

	// visibleIDs is the canonical visible projection without caller-owned
	// values. Nodes rebuilds it only after a write, then copies values for each
	// caller so reads remain isolated.
	visibleIDs        []NodeID
	visibleIDsVersion uint64
}

var _ crdt.CRDT[*ORTree] = (*ORTree)(nil)
var _ crdt.DeltaCapable[*ORTree, Delta] = (*ORTree)(nil)

// StableFrameType returns the stable observed-remove tree state/delta pair.
// Tree v1 supports immutable parent links with add and observed-remove only;
// a future move protocol requires a distinct frame pair and semantic version.
func StableFrameType() crdt.FrameType {
	return crdt.FrameType{StateID: crdt.TypeIDORTreeState, DeltaID: crdt.TypeIDORTreeDelta, SemanticsVersion: SemanticsVersion, UsesHLC: true}
}

func New(replicaID string) (*ORTree, error) { return NewWithOptions(replicaID, DefaultOptions()) }

// NewWithOptions constructs an OR-Tree with explicit retained-state limits.
func NewWithOptions(replicaID string, options Options) (*ORTree, error) {
	return NewFromClockWithOptions(clock.State{ReplicaID: replicaID}, options)
}

func NewFromClock(state clock.State) (*ORTree, error) {
	return NewFromClockWithOptions(state, DefaultOptions())
}

// NewFromClockWithOptions restores an OR-Tree clock with explicit
// retained-state limits. Persist the clock atomically with a complete state
// before reusing its replica ID.
func NewFromClockWithOptions(state clock.State, options Options) (*ORTree, error) {
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
	return &ORTree{replicaID: state.ReplicaID, clock: hlc, options: options, nodes: make(map[NodeID]storedNode), tombstones: make(map[NodeID]struct{})}, nil
}
func (t *ORTree) ClockState() clock.State {
	if t == nil || t.clock == nil {
		return clock.State{}
	}
	return t.clock.Snapshot()
}

func (t *ORTree) Add(parent NodeID, value []byte) (NodeID, Delta, error) {
	if t == nil || t.clock == nil {
		return NodeID{}, Delta{}, ErrNilTree
	}
	if len(value) > t.options.MaxValueBytes {
		return NodeID{}, Delta{}, ErrResourceLimit
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.nodes) >= t.options.MaxNodes {
		return NodeID{}, Delta{}, ErrResourceLimit
	}
	if parent.Valid() && !liveReachable(parent, t.nodes, t.tombstones) {
		return NodeID{}, Delta{}, ErrUnknownParent
	}
	id, err := t.clock.Now()
	if err != nil {
		return NodeID{}, Delta{}, err
	}
	node := storedNode{parent: parent, value: append([]byte(nil), value...)}
	t.nodes[id] = node
	t.version++
	delta := Delta{nodes: map[NodeID]storedNode{id: node}, tombstones: map[NodeID]struct{}{}}
	return id, delta, nil
}
func (t *ORTree) Remove(id NodeID) (Delta, error) {
	if t == nil {
		return Delta{}, ErrNilTree
	}
	if !id.Valid() {
		return Delta{}, ErrUnknownNode
	}
	t.mu.RLock()
	_, exists := t.nodes[id]
	_, removed := t.tombstones[id]
	t.mu.RUnlock()
	if !exists || removed {
		return Delta{}, ErrUnknownNode
	}
	delta := Delta{nodes: map[NodeID]storedNode{}, tombstones: map[NodeID]struct{}{id: {}}}
	return delta, t.ApplyDelta(delta)
}
func (t *ORTree) ApplyDelta(delta Delta) error {
	if t == nil || t.clock == nil {
		return ErrNilTree
	}
	if err := validate(delta); err != nil {
		return err
	}
	for _, incoming := range delta.nodes {
		if len(incoming.value) > t.options.MaxValueBytes {
			return ErrResourceLimit
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, incoming := range delta.nodes {
		if current, exists := t.nodes[id]; exists && !sameTreeNode(current, incoming) {
			return ErrNodeConflict
		}
	}
	if treeDeltaSubsumed(t.nodes, t.tombstones, delta) {
		return nil
	}
	if len(t.nodes)+newTreeNodes(delta.nodes, t.nodes) > t.options.MaxNodes ||
		len(t.tombstones)+newTreeTombstones(delta.tombstones, t.tombstones) > t.options.MaxTombstones {
		return ErrResourceLimit
	}
	// A locally generated subtree commonly arrives as one complete batch where
	// every edge points to an earlier tag in the same batch (or the synthetic
	// root). That strict ordering proves the batch acyclic without allocating
	// the generic graph-walk state. Incomplete, cross-batch, or non-monotonic
	// deltas deliberately retain the conservative validator below.
	if !rootedMonotonicDeltaAcyclic(delta.nodes) && !acyclic(delta.nodes, t.nodes) {
		return ErrInvalidDelta
	}
	if tag, ok := greatest(delta); ok {
		if err := t.clock.Witness(tag); err != nil {
			return err
		}
	}
	// A first sync has no retained entries to preserve. Allocate exactly once
	// after every rejecting operation (including clock advancement) has passed,
	// rather than growing an initially empty map repeatedly for a large batch.
	// Existing maps stay untouched so incremental delivery keeps its current
	// amortized behavior and identity.
	if len(t.nodes) == 0 && len(delta.nodes) > 0 {
		t.nodes = make(map[NodeID]storedNode, len(delta.nodes))
	}
	if len(t.tombstones) == 0 && len(delta.tombstones) > 0 {
		t.tombstones = make(map[NodeID]struct{}, len(delta.tombstones))
	}
	changed := false
	for id, incoming := range delta.nodes {
		if _, exists := t.nodes[id]; !exists {
			t.nodes[id] = incoming
			changed = true
		}
	}
	for id := range delta.tombstones {
		if _, exists := t.tombstones[id]; exists {
			continue
		}
		t.tombstones[id] = struct{}{}
		changed = true
	}
	if changed {
		t.version++
	}
	return nil
}
func (t *ORTree) Merge(other *ORTree) error {
	if t == nil || other == nil {
		return ErrNilTree
	}
	if t == other {
		return nil
	}
	other.mu.RLock()
	delta := Delta{nodes: cloneNodes(other.nodes), tombstones: cloneTombstones(other.tombstones)}
	other.mu.RUnlock()
	return t.ApplyDelta(delta)
}
func (d Delta) Merge(other Delta) (Delta, error) {
	if err := validate(d); err != nil {
		return Delta{}, err
	}
	if err := validate(other); err != nil {
		return Delta{}, err
	}
	merged := Delta{nodes: cloneNodes(d.nodes), tombstones: cloneTombstones(d.tombstones)}
	for id, node := range other.nodes {
		if current, exists := merged.nodes[id]; exists && !sameTreeNode(current, node) {
			return Delta{}, ErrNodeConflict
		}
		merged.nodes[id] = node
	}
	for id := range other.tombstones {
		merged.tombstones[id] = struct{}{}
	}
	return merged, nil
}

// Nodes returns visible, root-reachable nodes in canonical preorder.
func (t *ORTree) Nodes() []Node {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	if t.visibleIDsVersion == t.version {
		result := nodesForVisibleIDs(t.visibleIDs, t.nodes)
		t.mu.RUnlock()
		return result
	}
	t.mu.RUnlock()

	t.mu.Lock()
	if t.visibleIDsVersion != t.version {
		t.visibleIDs = visibleNodeIDs(t.nodes, t.tombstones)
		t.visibleIDsVersion = t.version
	}
	result := nodesForVisibleIDs(t.visibleIDs, t.nodes)
	t.mu.Unlock()
	return result
}
func (t *ORTree) State() crdt.StateSnapshot {
	if t == nil {
		return crdt.StateSnapshot{Type: "ortree"}
	}
	t.mu.RLock()
	if t.visibleCountVersion == t.version {
		state := crdt.StateSnapshot{Type: "ortree", ReplicaID: t.replicaID, ElementCount: t.visibleCount, TombstoneCount: len(t.tombstones)}
		t.mu.RUnlock()
		return state
	}
	t.mu.RUnlock()

	t.mu.Lock()
	if t.visibleCountVersion != t.version {
		t.visibleCount = countVisible(t.nodes, t.tombstones)
		t.visibleCountVersion = t.version
	}
	state := crdt.StateSnapshot{Type: "ortree", ReplicaID: t.replicaID, ElementCount: t.visibleCount, TombstoneCount: len(t.tombstones)}
	t.mu.Unlock()
	return state
}

// TombstoneTags returns every retained deletion tag in canonical order. It is
// an exact-acknowledgement input, not proof that a tombstone is safe to
// compact.
func (t *ORTree) TombstoneTags() []NodeID {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return sortedTreeTombstoneIDs(t.tombstones)
}

// CompactTombstones removes exactly the requested tombstoned leaf nodes. For
// replicated state, call it only after the current membership epoch has
// durably recorded exact acknowledgements, a post-compaction checkpoint, and
// retirement of old deltas. tombstonegc.SimpleCollector may call it only for
// its documented local-only lifecycle. Any known child makes a deleted node a
// structural anchor, so the operation is all-or-nothing for that request.
func (t *ORTree) CompactTombstones(tags []NodeID) (int, error) {
	if t == nil {
		return 0, ErrNilTree
	}
	for _, tag := range tags {
		if !tag.Valid() {
			return 0, ErrUnsafeCompaction
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	children := make(map[NodeID]struct{}, len(t.nodes))
	for _, node := range t.nodes {
		if node.parent.Valid() {
			children[node.parent] = struct{}{}
		}
	}
	compact := make([]NodeID, 0, len(tags))
	seen := make(map[NodeID]struct{}, len(tags))
	for _, tag := range tags {
		if _, duplicate := seen[tag]; duplicate {
			continue
		}
		seen[tag] = struct{}{}
		if _, tombstoned := t.tombstones[tag]; !tombstoned {
			continue
		}
		if _, exists := t.nodes[tag]; !exists {
			return 0, ErrUnsafeCompaction
		}
		if _, hasChild := children[tag]; hasChild {
			return 0, ErrUnsafeCompaction
		}
		compact = append(compact, tag)
	}
	for _, tag := range compact {
		delete(t.nodes, tag)
		delete(t.tombstones, tag)
	}
	if len(compact) > 0 {
		t.version++
	}
	return len(compact), nil
}

// CompactEligibleTombstones makes best-effort structural progress through an
// exact-acknowledged tombstone batch. It removes deleted descendants before
// their deleted ancestors, so an entirely deleted tree branch can compact in
// one call. A retained child that is not part of the batch remains a structural
// anchor and prevents its parent from being removed.
//
// For replicated state, callers must first authenticate exact acknowledgements
// for the current membership epoch, durably persist the post-compaction
// checkpoint, and retire old-epoch frames. tombstonegc.SimpleCollector may use
// this structural operation only for its documented local-only lifecycle.
func (t *ORTree) CompactEligibleTombstones(tags []NodeID) (int, error) {
	if t == nil {
		return 0, ErrNilTree
	}
	for _, tag := range tags {
		if !tag.Valid() {
			return 0, ErrUnsafeCompaction
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	indexes := make(map[NodeID]int, len(tags))
	candidates := make([]NodeID, 0, len(tags))
	for _, tag := range tags {
		if _, duplicate := indexes[tag]; duplicate {
			continue
		}
		if _, tombstoned := t.tombstones[tag]; !tombstoned {
			continue
		}
		if _, exists := t.nodes[tag]; !exists {
			continue
		}
		indexes[tag] = len(candidates)
		candidates = append(candidates, tag)
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Compare(candidates[j]) < 0 })
	for index, tag := range candidates {
		indexes[tag] = index
	}

	remainingChildren := make([]int, len(candidates))
	for _, node := range t.nodes {
		if index, selected := indexes[node.parent]; selected {
			remainingChildren[index]++
		}
	}
	ready := make([]int, 0, len(candidates))
	for index, children := range remainingChildren {
		if children == 0 {
			ready = append(ready, index)
		}
	}
	compact := make([]NodeID, 0, len(candidates))
	for len(ready) > 0 {
		index := ready[0]
		ready = ready[1:]
		tag := candidates[index]
		compact = append(compact, tag)
		parentIndex, selectedParent := indexes[t.nodes[tag].parent]
		if !selectedParent {
			continue
		}
		remainingChildren[parentIndex]--
		if remainingChildren[parentIndex] == 0 {
			ready = append(ready, parentIndex)
		}
	}
	for _, tag := range compact {
		delete(t.nodes, tag)
		delete(t.tombstones, tag)
	}
	if len(compact) > 0 {
		t.version++
	}
	return len(compact), nil
}

func validate(delta Delta) error {
	for id, node := range delta.nodes {
		if !id.Valid() || id == node.parent || (node.parent != (NodeID{}) && !node.parent.Valid()) {
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

func sameTreeNode(left, right storedNode) bool {
	return left.parent == right.parent && bytes.Equal(left.value, right.value)
}

// treeDeltaSubsumed reports whether every node and tombstone in delta is
// already retained by the receiver. The caller must hold t.mu for writing so a
// concurrent compaction cannot retire one of the identifiers mid-check.
func treeDeltaSubsumed(nodes map[NodeID]storedNode, tombstones map[NodeID]struct{}, delta Delta) bool {
	for id, incoming := range delta.nodes {
		current, exists := nodes[id]
		if !exists || !sameTreeNode(current, incoming) {
			return false
		}
	}
	for id := range delta.tombstones {
		if _, exists := tombstones[id]; !exists {
			return false
		}
	}
	return true
}

func acyclic(incoming, existing map[NodeID]storedNode) bool {
	lookup := func(id NodeID) (storedNode, bool) {
		if node, ok := incoming[id]; ok {
			return node, true
		}
		node, ok := existing[id]
		return node, ok
	}
	const (
		visiting uint8 = 1
		complete uint8 = 2
	)
	// One state map is shared across every incoming root. In particular, a
	// long parent chain is traversed once rather than once per descendant.
	state := make(map[NodeID]uint8, len(incoming))
	path := make([]NodeID, 0)
	for id := range incoming {
		if state[id] == complete {
			continue
		}
		path = path[:0]
		for current := id; current.Valid(); {
			switch state[current] {
			case visiting:
				return false
			case complete:
				current = NodeID{}
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

// rootedMonotonicDeltaAcyclic proves one closed delta forest has no cycle.
// Every non-root parent must be in incoming and compare strictly before its
// child. Following parent edges therefore strictly decreases a finite ordered
// set and can only terminate at the synthetic root. The condition intentionally
// excludes incomplete and cross-batch deltas because those need acyclic's
// combined incoming/existing graph walk.
func rootedMonotonicDeltaAcyclic(incoming map[NodeID]storedNode) bool {
	for id, item := range incoming {
		if !item.parent.Valid() {
			continue
		}
		if item.parent.Compare(id) >= 0 {
			return false
		}
		if _, exists := incoming[item.parent]; !exists {
			return false
		}
	}
	return true
}

// liveReachable reports whether id is connected to the synthetic root through
// present, non-tombstoned ancestors. Local adds require this property so a
// successful mutation never targets a subtree that the public projection hides.
func liveReachable(id NodeID, nodes map[NodeID]storedNode, tombstones map[NodeID]struct{}) bool {
	seen := make(map[NodeID]struct{})
	for id.Valid() {
		if _, repeated := seen[id]; repeated {
			return false
		}
		seen[id] = struct{}{}
		if _, removed := tombstones[id]; removed {
			return false
		}
		node, exists := nodes[id]
		if !exists {
			return false
		}
		id = node.parent
	}
	return true
}

func greatest(delta Delta) (NodeID, bool) {
	var greatest NodeID
	ok := false
	for id, node := range delta.nodes {
		if !ok || greatest.Compare(id) < 0 {
			greatest, ok = id, true
		}
		if node.parent.Valid() && greatest.Compare(node.parent) < 0 {
			greatest = node.parent
		}
	}
	for id := range delta.tombstones {
		if !ok || greatest.Compare(id) < 0 {
			greatest, ok = id, true
		}
	}
	return greatest, ok
}
func visibleNodeIDs(nodes map[NodeID]storedNode, tombstones map[NodeID]struct{}) []NodeID {
	children := make(map[NodeID][]NodeID, len(nodes))
	for id, node := range nodes {
		children[node.parent] = append(children[node.parent], id)
	}
	for parent := range children {
		sort.Slice(children[parent], func(i, j int) bool { return children[parent][i].Compare(children[parent][j]) > 0 })
	}
	stack := append([]NodeID(nil), children[NodeID{}]...)
	reverse(stack)
	result := make([]NodeID, 0, len(nodes))
	for len(stack) > 0 {
		index := len(stack) - 1
		id := stack[index]
		stack = stack[:index]
		if _, removed := tombstones[id]; removed {
			continue
		}
		result = append(result, id)
		child := children[id]
		for index := len(child) - 1; index >= 0; index-- {
			stack = append(stack, child[index])
		}
	}
	return result
}

func nodesForVisibleIDs(ids []NodeID, nodes map[NodeID]storedNode) []Node {
	result := make([]Node, 0, len(ids))
	for _, id := range ids {
		node := nodes[id]
		result = append(result, Node{ID: id, Parent: node.parent, Value: append([]byte(nil), node.value...)})
	}
	return result
}

// countVisible follows the same reachability and tombstone rules as visible,
// but avoids sibling sorting and Node/value allocation for State().
func countVisible(nodes map[NodeID]storedNode, tombstones map[NodeID]struct{}) int {
	children := make(map[NodeID][]NodeID, len(nodes))
	for id, node := range nodes {
		children[node.parent] = append(children[node.parent], id)
	}
	stack := append([]NodeID(nil), children[NodeID{}]...)
	count := 0
	for len(stack) > 0 {
		index := len(stack) - 1
		id := stack[index]
		stack = stack[:index]
		if _, removed := tombstones[id]; removed {
			continue
		}
		count++
		stack = append(stack, children[id]...)
	}
	return count
}
func reverse(ids []NodeID) {
	for left, right := 0, len(ids)-1; left < right; left, right = left+1, right-1 {
		ids[left], ids[right] = ids[right], ids[left]
	}
}
func cloneNodes(source map[NodeID]storedNode) map[NodeID]storedNode {
	out := make(map[NodeID]storedNode, len(source))
	for id, node := range source {
		node.value = append([]byte(nil), node.value...)
		out[id] = node
	}
	return out
}
func cloneTombstones(source map[NodeID]struct{}) map[NodeID]struct{} {
	out := make(map[NodeID]struct{}, len(source))
	for id := range source {
		out[id] = struct{}{}
	}
	return out
}

func newTreeNodes(incoming, existing map[NodeID]storedNode) int {
	count := 0
	for id := range incoming {
		if _, exists := existing[id]; !exists {
			count++
		}
	}
	return count
}

func newTreeTombstones(incoming, existing map[NodeID]struct{}) int {
	count := 0
	for id := range incoming {
		if _, exists := existing[id]; !exists {
			count++
		}
	}
	return count
}
