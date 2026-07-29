package tree

import (
	"sort"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

// MarshalBinary returns a deterministic, bounded framed OR-Tree state.
func (t *ORTree) MarshalBinary() ([]byte, error) {
	if t == nil {
		return nil, ErrNilTree
	}
	t.mu.RLock()
	nodes, tombstones := cloneNodes(t.nodes), cloneTombstones(t.tombstones)
	t.mu.RUnlock()
	return marshalTree(crdt.TypeIDORTreeState, nodes, tombstones)
}
func (d Delta) MarshalBinary() ([]byte, error) {
	if err := validate(d); err != nil {
		return nil, err
	}
	return marshalTree(crdt.TypeIDORTreeDelta, d.nodes, d.tombstones)
}
func marshalTree(typeID uint64, nodes map[NodeID]storedNode, tombstones map[NodeID]struct{}) ([]byte, error) {
	limits := frame.DefaultLimits()
	if err := validate(Delta{nodes: nodes, tombstones: tombstones}); err != nil {
		return nil, err
	}
	if typeID == crdt.TypeIDORTreeState && !hasCompleteTreeParents(nodes) {
		return nil, ErrIncompleteState
	}
	if len(nodes) > limits.MaxElements || len(nodes) > limits.MaxTags || len(tombstones) > limits.MaxTags-len(nodes) {
		return nil, frame.ErrFrameLimit
	}
	nodeIDs := sortedTreeNodeIDs(nodes)
	tombstoneIDs := sortedTreeTombstoneIDs(tombstones)
	payloadSize := frame.UvarintSize(uint64(len(nodeIDs)))
	for _, id := range nodeIDs {
		n := nodes[id]
		if err := addTreeIDSize(&payloadSize, id, limits); err != nil {
			return nil, err
		}
		additional := frame.UvarintSize(uint64(len(n.value)))
		if n.parent.Valid() {
			if len(n.parent.ReplicaID) > limits.MaxStringBytes {
				return nil, frame.ErrFrameLimit
			}
			additional += 1 + frame.TagSize(n.parent)
		} else {
			additional++
		}
		if len(n.value) > limits.MaxStringBytes || len(n.value) > limits.MaxPayload-additional || additional > limits.MaxPayload-payloadSize-len(n.value) {
			return nil, frame.ErrFrameLimit
		}
		payloadSize += additional + len(n.value)
	}
	additional := frame.UvarintSize(uint64(len(tombstoneIDs)))
	if additional > limits.MaxPayload-payloadSize {
		return nil, frame.ErrFrameLimit
	}
	payloadSize += additional
	for _, id := range tombstoneIDs {
		if err := addTreeIDSize(&payloadSize, id, limits); err != nil {
			return nil, err
		}
	}
	return frame.MarshalFrameWithPayload(typeID, "", payloadSize, func(payload []byte) error {
		output := frame.AppendUvarint(payload[:0], uint64(len(nodeIDs)))
		for _, id := range nodeIDs {
			n := nodes[id]
			output = frame.AppendTag(output, id)
			if n.parent.Valid() {
				output = frame.AppendUvarint(output, 1)
				output = frame.AppendTag(output, n.parent)
			} else {
				output = frame.AppendUvarint(output, 0)
			}
			output = frame.AppendUvarint(output, uint64(len(n.value)))
			output = append(output, n.value...)
		}
		output = frame.AppendUvarint(output, uint64(len(tombstoneIDs)))
		for _, id := range tombstoneIDs {
			output = frame.AppendTag(output, id)
		}
		if len(output) != payloadSize {
			return frame.ErrInvalidFrame
		}
		return nil
	})
}
func addTreeIDSize(payloadSize *int, id NodeID, limits frame.DecoderLimits) error {
	if len(id.ReplicaID) > limits.MaxStringBytes {
		return frame.ErrFrameLimit
	}
	additional := frame.TagSize(id)
	if additional > limits.MaxPayload-*payloadSize {
		return frame.ErrFrameLimit
	}
	*payloadSize += additional
	return nil
}

func sortedTreeNodeIDs(nodes map[NodeID]storedNode) []NodeID {
	ids := make([]NodeID, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Compare(ids[j]) < 0 })
	return ids
}

func sortedTreeTombstoneIDs(tombstones map[NodeID]struct{}) []NodeID {
	ids := make([]NodeID, 0, len(tombstones))
	for id := range tombstones {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Compare(ids[j]) < 0 })
	return ids
}

func hasCompleteTreeParents(nodes map[NodeID]storedNode) bool {
	for _, node := range nodes {
		if node.parent.Valid() {
			if _, ok := nodes[node.parent]; !ok {
				return false
			}
		}
	}
	return true
}

func UnmarshalDelta(data []byte) (Delta, error) {
	return UnmarshalDeltaWithLimits(data, frame.DefaultLimits())
}
func UnmarshalDeltaWithLimits(data []byte, limits frame.DecoderLimits) (Delta, error) {
	f, err := frame.UnmarshalFrame(data, limits)
	if err != nil {
		return Delta{}, err
	}
	if f.TypeID != crdt.TypeIDORTreeDelta || f.CodecID != "" {
		return Delta{}, frame.ErrInvalidFrame
	}
	p := 0
	count, next, ok := frame.ReadUvarint(f.Payload, p)
	if !ok || count > uint64(limits.MaxElements) || count > uint64(limits.MaxTags) {
		return Delta{}, frame.ErrInvalidFrame
	}
	p = next
	nodes := make(map[NodeID]storedNode, int(count))
	var previous NodeID
	for i := uint64(0); i < count; i++ {
		id, n, ok := frame.ReadTag(f.Payload, p, limits.MaxStringBytes)
		if !ok || (i > 0 && previous.Compare(id) >= 0) {
			return Delta{}, frame.ErrInvalidFrame
		}
		p = n
		flag, n, ok := frame.ReadUvarint(f.Payload, p)
		if !ok || flag > 1 {
			return Delta{}, frame.ErrInvalidFrame
		}
		p = n
		parent := NodeID{}
		if flag == 1 {
			parent, n, ok = frame.ReadTag(f.Payload, p, limits.MaxStringBytes)
			if !ok {
				return Delta{}, frame.ErrInvalidFrame
			}
			p = n
		}
		value, n, ok := frame.ReadBytes(f.Payload, p, limits.MaxStringBytes)
		if !ok {
			return Delta{}, frame.ErrInvalidFrame
		}
		p = n
		nodes[id] = storedNode{parent: parent, value: append([]byte(nil), value...)}
		previous = id
	}
	tombs, next, ok := frame.ReadUvarint(f.Payload, p)
	if !ok || tombs > uint64(limits.MaxTags-int(count)) {
		return Delta{}, frame.ErrInvalidFrame
	}
	p = next
	tombstones := make(map[NodeID]struct{}, int(tombs))
	var previousTomb NodeID
	for i := uint64(0); i < tombs; i++ {
		id, n, ok := frame.ReadTag(f.Payload, p, limits.MaxStringBytes)
		if !ok || (i > 0 && previousTomb.Compare(id) >= 0) {
			return Delta{}, frame.ErrInvalidFrame
		}
		p = n
		tombstones[id] = struct{}{}
		previousTomb = id
	}
	delta := Delta{nodes: nodes, tombstones: tombstones}
	if p != len(f.Payload) || validate(delta) != nil || !acyclic(nodes, nil) {
		return Delta{}, frame.ErrInvalidFrame
	}
	return delta, nil
}

// UnmarshalBinary validates the full state before atomically replacing t.
// Complete states additionally require every non-root parent to be present.
func (t *ORTree) UnmarshalBinary(data []byte) error {
	return t.UnmarshalBinaryWithLimits(data, frame.DefaultLimits())
}
func (t *ORTree) UnmarshalBinaryWithLimits(data []byte, limits frame.DecoderLimits) error {
	if t == nil || t.clock == nil {
		return ErrNilTree
	}
	f, err := frame.UnmarshalFrame(data, limits)
	if err != nil {
		return err
	}
	if f.TypeID != crdt.TypeIDORTreeState || f.CodecID != "" {
		return frame.ErrInvalidFrame
	}
	// Reuse the one delta parser: the payload layout is intentionally identical.
	encoded, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDORTreeDelta, Payload: f.Payload})
	if err != nil {
		return err
	}
	delta, err := UnmarshalDeltaWithLimits(encoded, limits)
	if err != nil {
		return err
	}
	if len(delta.nodes) > t.options.MaxNodes || len(delta.tombstones) > t.options.MaxTombstones {
		return ErrResourceLimit
	}
	for _, node := range delta.nodes {
		if len(node.value) > t.options.MaxValueBytes {
			return ErrResourceLimit
		}
	}
	for _, node := range delta.nodes {
		if node.parent.Valid() {
			if _, ok := delta.nodes[node.parent]; !ok {
				return frame.ErrInvalidFrame
			}
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if tag, ok := greatest(delta); ok {
		if err := t.clock.Witness(tag); err != nil {
			return err
		}
	}
	t.nodes, t.tombstones = delta.nodes, delta.tombstones
	t.version++
	return nil
}

func (t *ORTree) MarshalBinaryWithClockState() ([]byte, clock.State, error) {
	if t == nil || t.clock == nil {
		return nil, clock.State{}, ErrNilTree
	}
	t.mu.RLock()
	nodes, tombstones, state := cloneNodes(t.nodes), cloneTombstones(t.tombstones), t.clock.Snapshot()
	t.mu.RUnlock()
	encoded, err := marshalTree(crdt.TypeIDORTreeState, nodes, tombstones)
	return encoded, state, err
}
func (t *ORTree) SnapshotCurrentState() (snapshot.Snapshot, error) {
	if t == nil || t.clock == nil {
		return snapshot.Snapshot{}, ErrNilTree
	}
	t.mu.RLock()
	nodes := cloneNodes(t.nodes)
	tombstones := cloneTombstones(t.tombstones)
	clockState := t.clock.Snapshot()
	frontier := treeFrontier(nodes, tombstones)
	t.mu.RUnlock()
	state, err := marshalTree(crdt.TypeIDORTreeState, nodes, tombstones)
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	return snapshot.NewWithClockState(state, frontier, clockState)
}
func NewFromSnapshot(saved snapshot.Snapshot) (*ORTree, error) {
	return NewFromSnapshotWithOptions(saved, DefaultOptions())
}

// NewFromSnapshotWithOptions restores an OR-Tree snapshot while retaining the
// application's local resource limits. A snapshot is not allowed to widen the
// receiver's memory budget.
func NewFromSnapshotWithOptions(saved snapshot.Snapshot, options Options) (*ORTree, error) {
	if saved.TypeID != crdt.TypeIDORTreeState {
		return nil, ErrInvalidDelta
	}
	state, ok := saved.ClockState()
	if !ok {
		return nil, ErrInvalidDelta
	}
	t, err := NewFromClockWithOptions(state, options)
	if err != nil {
		return nil, err
	}
	if err := t.UnmarshalBinary(saved.Bytes()); err != nil {
		return nil, err
	}
	return t, nil
}
func recordTreeFrontier(frontier map[string]crdt.Tag, id NodeID) {
	if current, ok := frontier[id.ReplicaID]; !ok || current.Compare(id) < 0 {
		frontier[id.ReplicaID] = id
	}
}

func treeFrontier(nodes map[NodeID]storedNode, tombstones map[NodeID]struct{}) map[string]crdt.Tag {
	frontier := make(map[string]crdt.Tag)
	for id, node := range nodes {
		recordTreeFrontier(frontier, id)
		if node.parent.Valid() {
			recordTreeFrontier(frontier, node.parent)
		}
	}
	for id := range tombstones {
		recordTreeFrontier(frontier, id)
	}
	return frontier
}
