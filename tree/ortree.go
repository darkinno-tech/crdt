// Package tree implements an observed-remove rooted tree CRDT.
package tree

import (
	"errors"
	"sort"
	"sync"

	"github.com/darkinno/crdt"
	"github.com/darkinno/crdt/clock"
)

var (
	ErrInvalidReplicaID = errors.New("tree: invalid replica ID")
	ErrNilTree          = errors.New("tree: nil OR-Tree")
	ErrUnknownParent    = errors.New("tree: unknown live parent")
	ErrUnknownNode      = errors.New("tree: unknown live node")
	ErrInvalidDelta     = errors.New("tree: invalid delta")
	ErrIncompleteState  = errors.New("tree: incomplete OR-Tree state")
	ErrNodeConflict     = errors.New("tree: conflicting node identity")
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

// ORTree supports add and observed-remove. Moving a node is deliberately not
// an in-place operation: remove it and add a new instance under the new parent.
type ORTree struct {
	mu         sync.RWMutex
	replicaID  string
	clock      *clock.HLC
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

func New(replicaID string) (*ORTree, error) { return NewFromClock(clock.State{ReplicaID: replicaID}) }
func NewFromClock(state clock.State) (*ORTree, error) {
	if !(crdt.Tag{ReplicaID: state.ReplicaID}).Valid() {
		return nil, ErrInvalidReplicaID
	}
	hlc, err := clock.NewHLCFromState(state)
	if err != nil {
		return nil, err
	}
	return &ORTree{replicaID: state.ReplicaID, clock: hlc, nodes: make(map[NodeID]storedNode), tombstones: make(map[NodeID]struct{})}, nil
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
	id, err := t.clock.Now()
	if err != nil {
		return NodeID{}, Delta{}, err
	}
	node := storedNode{parent: parent, value: append([]byte(nil), value...)}
	t.mu.Lock()
	defer t.mu.Unlock()
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
	children := make(map[NodeID][]NodeID)
	for id, node := range nodes {
		children[node.parent] = append(children[node.parent], id)
	}
	for parent := range children {
		sort.Slice(children[parent], func(i, j int) bool { return children[parent][i].Compare(children[parent][j]) > 0 })
	}
	stack := append([]NodeID(nil), children[NodeID{}]...)
	reverse(stack)
	result := []Node{}
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
