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

	"github.com/darkinno/crdt"
	"github.com/darkinno/crdt/clock"
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
)

// Position is a stable, opaque identifier for one Unicode scalar value.
// It remains valid after inserts before it and after it has been deleted.
type Position = crdt.Tag

type node struct {
	parent Position
	rune   rune
}

// Delta is a joinable partial RGA state. Nodes and tombstones are deliberately
// opaque so a malformed delta cannot be assembled by direct field mutation.
type Delta struct {
	nodes      map[Position]node
	tombstones map[Position]struct{}
}

// RGA is a collaborative text CRDT. A tombstone retained for a deleted
// position wins even if it arrives before the corresponding insertion.
type RGA struct {
	mu         sync.RWMutex
	replicaID  string
	clock      *clock.HLC
	nodes      map[Position]node
	tombstones map[Position]struct{}
	version    uint64

	// projection caches the ordered visible positions for the current version.
	// It has its own lock so frequent readers do not contend with CRDT writes.
	// A projection is replaced as a whole and never mutated after publication.
	projectionMu      sync.Mutex
	projection        []Position
	projectionVersion uint64
}

var _ crdt.CRDT[*RGA] = (*RGA)(nil)
var _ crdt.DeltaCapable[*RGA, Delta] = (*RGA)(nil)

func New(replicaID string) (*RGA, error) { return NewFromClock(clock.State{ReplicaID: replicaID}) }

func NewFromClock(state clock.State) (*RGA, error) {
	if !(crdt.Tag{ReplicaID: state.ReplicaID}).Valid() {
		return nil, ErrInvalidReplicaID
	}
	hlc, err := clock.NewHLCFromState(state)
	if err != nil {
		return nil, err
	}
	return &RGA{
		replicaID:         state.ReplicaID,
		clock:             hlc,
		nodes:             make(map[Position]node),
		tombstones:        make(map[Position]struct{}),
		projection:        []Position{},
		projectionVersion: 0,
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
	visible, _ := r.visibleProjection()
	if offset > len(visible) {
		return Delta{}, ErrRange
	}
	if len(runes) == 0 {
		return Delta{nodes: make(map[Position]node), tombstones: make(map[Position]struct{})}, nil
	}
	parent := Position{}
	if offset > 0 {
		parent = visible[offset-1]
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
	visible, _ := r.visibleProjection()
	if offset > len(visible) || count > len(visible)-offset {
		return Delta{}, ErrRange
	}
	delta := Delta{nodes: make(map[Position]node), tombstones: make(map[Position]struct{}, count)}
	for _, id := range visible[offset : offset+count] {
		delta.tombstones[id] = struct{}{}
	}
	if err := r.ApplyDelta(delta); err != nil {
		return Delta{}, err
	}
	return delta, nil
}

func (r *RGA) String() string {
	if r == nil {
		return ""
	}
	for {
		visible, version := r.visibleProjection()
		r.mu.RLock()
		if r.version != version {
			r.mu.RUnlock()
			continue
		}
		result := make([]rune, 0, len(visible))
		for _, id := range visible {
			result = append(result, r.nodes[id].rune)
		}
		r.mu.RUnlock()
		return string(result)
	}
}

// Positions returns a copy of visible stable IDs in display order.
func (r *RGA) Positions() []Position {
	if r == nil {
		return nil
	}
	for {
		visible, version := r.visibleProjection()
		r.mu.RLock()
		current := r.version
		r.mu.RUnlock()
		if current == version {
			return append([]Position(nil), visible...)
		}
	}
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
	if !acyclicAgainst(delta.nodes, r.nodes) {
		return ErrInvalidDelta
	}
	for id, incoming := range delta.nodes {
		current, exists := r.nodes[id]
		if exists && current != incoming {
			return ErrTagConflict
		}
	}
	if tag, ok := greatestTag(delta); ok {
		if err := r.clock.Witness(tag); err != nil {
			return err
		}
	}
	if len(r.nodes) == 0 && len(delta.nodes) > 0 {
		r.nodes = make(map[Position]node, len(delta.nodes))
	}
	if len(r.tombstones) == 0 && len(delta.tombstones) > 0 {
		r.tombstones = make(map[Position]struct{}, len(delta.tombstones))
	}
	changed := false
	for id, incoming := range delta.nodes {
		if _, exists := r.nodes[id]; exists {
			continue
		}
		r.nodes[id] = incoming
		changed = true
	}
	for id := range delta.tombstones {
		if _, exists := r.tombstones[id]; exists {
			continue
		}
		r.tombstones[id] = struct{}{}
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
	other.mu.RUnlock()
	return r.ApplyDelta(delta)
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
	for {
		visible, version := r.visibleProjection()
		r.mu.RLock()
		if r.version == version {
			state := crdt.StateSnapshot{Type: "rga-text", ReplicaID: r.replicaID, ElementCount: len(visible), TombstoneCount: len(r.tombstones)}
			r.mu.RUnlock()
			return state
		}
		r.mu.RUnlock()
	}
}

// visibleProjection returns an immutable ordered slice paired with the state
// version it represents. The slow adjacency/sort pass runs once after a write,
// not once per reader.
func (r *RGA) visibleProjection() ([]Position, uint64) {
	r.mu.RLock()
	version := r.version
	r.mu.RUnlock()
	r.projectionMu.Lock()
	defer r.projectionMu.Unlock()
	if r.projectionVersion == version {
		return r.projection, version
	}
	r.mu.RLock()
	version = r.version
	nodes := cloneNodes(r.nodes)
	tombstones := cloneTombstones(r.tombstones)
	r.mu.RUnlock()
	projection := buildVisible(nodes, tombstones)
	r.projection = projection
	r.projectionVersion = version
	return projection, version
}

func buildVisible(nodes map[Position]node, tombstones map[Position]struct{}) []Position {
	children := make(map[Position][]Position, len(nodes))
	for id, item := range nodes {
		children[item.parent] = append(children[item.parent], id)
	}
	for parent := range children {
		// Descending sibling tags make a later insertion immediately after its
		// predecessor appear before older siblings (including deleted ones and
		// their descendants). This preserves the public "insert before offset"
		// contract while retaining a deterministic concurrent tie-break.
		sort.Slice(children[parent], func(i, j int) bool { return children[parent][i].Compare(children[parent][j]) > 0 })
	}
	visibleCapacity := len(nodes) - len(tombstones)
	if visibleCapacity < 0 {
		visibleCapacity = 0
	}
	visible := make([]Position, 0, visibleCapacity)
	// An explicit stack avoids recursion proportional to a pasted-text length.
	stack := append([]Position(nil), children[Position{}]...)
	for left, right := 0, len(stack)-1; left < right; left, right = left+1, right-1 {
		stack[left], stack[right] = stack[right], stack[left]
	}
	for len(stack) > 0 {
		index := len(stack) - 1
		id := stack[index]
		stack = stack[:index]
		if _, deleted := tombstones[id]; !deleted {
			visible = append(visible, id)
		}
		childIDs := children[id]
		for index := len(childIDs) - 1; index >= 0; index-- {
			stack = append(stack, childIDs[index])
		}
	}
	return visible
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
// delivery), but rejects every cycle whose links are currently known. This is
// essential because visibleLocked recursively traverses the parent graph.
func acyclicAgainst(incoming, existing map[Position]node) bool {
	lookup := func(id Position) (node, bool) {
		if item, ok := incoming[id]; ok {
			return item, true
		}
		item, ok := existing[id]
		return item, ok
	}
	for id, item := range incoming {
		seen := map[Position]struct{}{id: {}}
		parent := item.parent
		for parent.Valid() {
			if _, repeated := seen[parent]; repeated {
				return false
			}
			seen[parent] = struct{}{}
			next, known := lookup(parent)
			if !known {
				break
			}
			parent = next.parent
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
