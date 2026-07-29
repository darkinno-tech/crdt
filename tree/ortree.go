// Package tree implements an observed-remove rooted tree CRDT.
package tree

import (
	"errors"
	"sort"
	"sync"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
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
}

var _ crdt.CRDT[*ORTree] = (*ORTree)(nil)
var _ crdt.DeltaCapable[*ORTree, Delta] = (*ORTree)(nil)

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
	id, err := t.clock.Now()
	if err != nil {
		return NodeID{}, Delta{}, err
	}
	node := storedNode{parent: parent, value: append([]byte(nil), value...)}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.nodes) >= t.options.MaxNodes {
		return NodeID{}, Delta{}, ErrResourceLimit
	}
	if parent.Valid() && !liveReachable(parent, t.nodes, t.tombstones) {
		return NodeID{}, Delta{}, ErrUnknownParent
	}
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
	if !acyclic(delta.nodes, t.nodes) {
		return ErrInvalidDelta
	}
	for id, incoming := range delta.nodes {
		if current, exists := t.nodes[id]; exists && (current.parent != incoming.parent || string(current.value) != string(incoming.value)) {
			return ErrNodeConflict
		}
	}
	if len(t.nodes)+newTreeNodes(delta.nodes, t.nodes) > t.options.MaxNodes ||
		len(t.tombstones)+newTreeTombstones(delta.tombstones, t.tombstones) > t.options.MaxTombstones {
		return ErrResourceLimit
	}
	if tag, ok := greatest(delta); ok {
		if err := t.clock.Witness(tag); err != nil {
			return err
		}
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
		if current, exists := merged.nodes[id]; exists && (current.parent != node.parent || string(current.value) != string(node.value)) {
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
	defer t.mu.RUnlock()
	return visible(t.nodes, t.tombstones)
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

// CompactTombstones removes exactly the requested tombstoned leaf nodes.
// Call it only after the current membership epoch has durably recorded exact
// acknowledgements, a post-compaction checkpoint, and retirement of old
// deltas. Any known child makes a deleted node a structural anchor, so the
// operation is all-or-nothing for that request.
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
func acyclic(incoming, existing map[NodeID]storedNode) bool {
	lookup := func(id NodeID) (storedNode, bool) {
		if node, ok := incoming[id]; ok {
			return node, true
		}
		node, ok := existing[id]
		return node, ok
	}
	for id, node := range incoming {
		seen := map[NodeID]struct{}{id: {}}
		parent := node.parent
		for parent.Valid() {
			if _, repeated := seen[parent]; repeated {
				return false
			}
			seen[parent] = struct{}{}
			next, ok := lookup(parent)
			if !ok {
				break
			}
			parent = next.parent
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
func visible(nodes map[NodeID]storedNode, tombstones map[NodeID]struct{}) []Node {
	children := make(map[NodeID][]NodeID, len(nodes))
	for id, node := range nodes {
		children[node.parent] = append(children[node.parent], id)
	}
	for parent := range children {
		sort.Slice(children[parent], func(i, j int) bool { return children[parent][i].Compare(children[parent][j]) > 0 })
	}
	stack := append([]NodeID(nil), children[NodeID{}]...)
	reverse(stack)
	result := make([]Node, 0, len(nodes))
	for len(stack) > 0 {
		index := len(stack) - 1
		id := stack[index]
		stack = stack[:index]
		if _, removed := tombstones[id]; removed {
			continue
		}
		node := nodes[id]
		result = append(result, Node{ID: id, Parent: node.parent, Value: append([]byte(nil), node.value...)})
		child := children[id]
		for index := len(child) - 1; index >= 0; index-- {
			stack = append(stack, child[index])
		}
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
