package text

import (
	"sort"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

// MarshalBinary returns the canonical framed RGA state.
func (r *RGA) MarshalBinary() ([]byte, error) {
	if r == nil {
		return nil, ErrNilText
	}
	r.mu.RLock()
	if len(r.pending) > 0 {
		r.mu.RUnlock()
		return nil, ErrIncompleteState
	}
	nodes, tombstones := cloneNodes(r.nodes), cloneTombstones(r.tombstones)
	r.mu.RUnlock()
	return marshalRGA(crdt.TypeIDRGAState, nodes, tombstones)
}

// MarshalBinary returns the canonical framed RGA delta.
func (d Delta) MarshalBinary() ([]byte, error) {
	if err := validateDelta(d); err != nil {
		return nil, err
	}
	return marshalRGA(crdt.TypeIDRGADelta, d.nodes, d.tombstones)
}

func marshalRGA(typeID uint64, nodes map[Position]node, tombstones map[Position]struct{}) ([]byte, error) {
	return marshalRGAWithLimits(typeID, nodes, tombstones, frame.DefaultLimits())
}

func marshalRGAWithLimits(typeID uint64, nodes map[Position]node, tombstones map[Position]struct{}, limits frame.DecoderLimits) ([]byte, error) {
	delta := Delta{nodes: nodes, tombstones: tombstones}
	if err := validateDelta(delta); err != nil {
		return nil, err
	}
	if typeID == crdt.TypeIDRGAState && !hasCompleteParents(nodes) {
		return nil, ErrIncompleteState
	}
	if len(nodes) > limits.MaxElements || len(nodes) > limits.MaxTags || len(tombstones) > limits.MaxTags-len(nodes) {
		return nil, frame.ErrFrameLimit
	}
	ids := sortedNodeIDs(nodes)
	tombIDs := sortedTombstoneIDs(tombstones)
	payloadSize := frame.UvarintSize(uint64(len(ids)))
	for _, id := range ids {
		item := nodes[id]
		if err := addWireTagSize(&payloadSize, id, limits); err != nil {
			return nil, err
		}
		additional := frame.UvarintSize(uint64(item.rune))
		if item.parent.Valid() {
			additional += 1 + frame.TagSize(item.parent)
		} else {
			additional++
		}
		if item.parent.Valid() && len(item.parent.ReplicaID) > limits.MaxStringBytes {
			return nil, frame.ErrFrameLimit
		}
		if additional > limits.MaxPayload-payloadSize {
			return nil, frame.ErrFrameLimit
		}
		payloadSize += additional
	}
	additional := frame.UvarintSize(uint64(len(tombIDs)))
	if additional > limits.MaxPayload-payloadSize {
		return nil, frame.ErrFrameLimit
	}
	payloadSize += additional
	for _, id := range tombIDs {
		if err := addWireTagSize(&payloadSize, id, limits); err != nil {
			return nil, frame.ErrFrameLimit
		}
	}
	return frame.MarshalFrameWithPayload(typeID, "", payloadSize, func(payload []byte) error {
		output := frame.AppendUvarint(payload[:0], uint64(len(ids)))
		for _, id := range ids {
			item := nodes[id]
			output = frame.AppendTag(output, id)
			if item.parent.Valid() {
				output = frame.AppendUvarint(output, 1)
				output = frame.AppendTag(output, item.parent)
			} else {
				output = frame.AppendUvarint(output, 0)
			}
			output = frame.AppendUvarint(output, uint64(item.rune))
		}
		output = frame.AppendUvarint(output, uint64(len(tombIDs)))
		for _, id := range tombIDs {
			output = frame.AppendTag(output, id)
		}
		if len(output) != payloadSize {
			return frame.ErrInvalidFrame
		}
		return nil
	})
}

// UnmarshalRGADelta decodes one bounded canonical RGA delta frame.
func UnmarshalRGADelta(data []byte) (Delta, error) {
	return UnmarshalRGADeltaWithLimits(data, frame.DefaultLimits())
}
func UnmarshalRGADeltaWithLimits(data []byte, limits frame.DecoderLimits) (Delta, error) {
	nodes, tombstones, err := unmarshalRGA(data, crdt.TypeIDRGADelta, limits, false)
	if err != nil {
		return Delta{}, err
	}
	return Delta{nodes: nodes, tombstones: tombstones}, nil
}

func (r *RGA) UnmarshalBinary(data []byte) error {
	return r.UnmarshalBinaryWithLimits(data, frame.DefaultLimits())
}
func (r *RGA) UnmarshalBinaryWithLimits(data []byte, limits frame.DecoderLimits) error {
	if r == nil || r.clock == nil {
		return ErrNilText
	}
	nodes, tombstones, err := unmarshalRGA(data, crdt.TypeIDRGAState, limits, true)
	if err != nil {
		return err
	}
	if len(nodes) > r.options.MaxNodes || len(tombstones) > r.options.MaxTombstones {
		return ErrResourceLimit
	}
	sequence, children, err := buildSequence(nodes, tombstones)
	if err != nil {
		return err
	}
	delta := Delta{nodes: nodes, tombstones: tombstones}
	if tag, ok := greatestTag(delta); ok {
		if err := r.clock.Witness(tag); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.nodes, r.tombstones = nodes, tombstones
	r.pending = make(map[Position]node)
	r.waitingByParent = make(map[Position]map[Position]struct{})
	r.pendingBytes = 0
	r.sequence, r.children = sequence, children
	r.version++
	r.mu.Unlock()
	return nil
}

func (r *RGA) MarshalBinaryWithClockState() ([]byte, clock.State, error) {
	if r == nil || r.clock == nil {
		return nil, clock.State{}, ErrNilText
	}
	r.mu.RLock()
	if len(r.pending) > 0 {
		r.mu.RUnlock()
		return nil, clock.State{}, ErrIncompleteState
	}
	nodes, tombstones, state := cloneNodes(r.nodes), cloneTombstones(r.tombstones), r.clock.Snapshot()
	r.mu.RUnlock()
	encoded, err := marshalRGA(crdt.TypeIDRGAState, nodes, tombstones)
	return encoded, state, err
}

func (r *RGA) Snapshot(frontier map[string]crdt.Tag) (snapshot.Snapshot, error) {
	state, clockState, err := r.MarshalBinaryWithClockState()
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	return snapshot.NewValidatedWithClockState(state, frontier, clockState, validateRGAState)
}

func (r *RGA) SnapshotCurrentState() (snapshot.Snapshot, error) {
	if r == nil || r.clock == nil {
		return snapshot.Snapshot{}, ErrNilText
	}
	r.mu.RLock()
	if len(r.pending) > 0 {
		r.mu.RUnlock()
		return snapshot.Snapshot{}, ErrIncompleteState
	}
	nodes := cloneNodes(r.nodes)
	tombstones := cloneTombstones(r.tombstones)
	clockState := r.clock.Snapshot()
	frontier := frontierForState(nodes, tombstones)
	r.mu.RUnlock()
	state, err := marshalRGA(crdt.TypeIDRGAState, nodes, tombstones)
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	return snapshot.NewValidatedWithClockState(state, frontier, clockState, validateRGAState)
}

func validateRGAState(data []byte) error {
	_, _, err := unmarshalRGA(data, crdt.TypeIDRGAState, frame.DefaultLimits(), true)
	return err
}

func NewFromSnapshot(saved snapshot.Snapshot) (*RGA, error) {
	clockState, ok := saved.ClockState()
	if !ok {
		return nil, ErrInvalidDelta
	}
	r, err := NewFromClock(clockState)
	if err != nil {
		return nil, err
	}
	var unmarshal func([]byte) error
	switch saved.TypeID {
	case crdt.TypeIDRGAState:
		unmarshal = r.UnmarshalBinary
	case crdt.TypeIDRGARunState:
		unmarshal = r.UnmarshalRunBinary
	default:
		return nil, ErrInvalidDelta
	}
	if err := unmarshal(saved.Bytes()); err != nil {
		return nil, err
	}
	return r, nil
}

func unmarshalRGA(data []byte, expectedType uint64, limits frame.DecoderLimits, complete bool) (map[Position]node, map[Position]struct{}, error) {
	decoded, err := frame.UnmarshalFrame(data, limits)
	if err != nil {
		return nil, nil, err
	}
	if decoded.TypeID != expectedType || decoded.CodecID != "" {
		return nil, nil, frame.ErrInvalidFrame
	}
	pos := 0
	count, next, ok := frame.ReadUvarint(decoded.Payload, pos)
	if !ok || count > uint64(limits.MaxElements) || count > uint64(limits.MaxTags) {
		return nil, nil, frame.ErrInvalidFrame
	}
	pos = next
	nodes := make(map[Position]node, int(count))
	var previous Position
	for index := uint64(0); index < count; index++ {
		id, afterID, ok := frame.ReadTag(decoded.Payload, pos, limits.MaxStringBytes)
		if !ok || (index > 0 && previous.Compare(id) >= 0) {
			return nil, nil, frame.ErrInvalidFrame
		}
		pos = afterID
		parentFlag, next, ok := frame.ReadUvarint(decoded.Payload, pos)
		if !ok || parentFlag > 1 {
			return nil, nil, frame.ErrInvalidFrame
		}
		pos = next
		parent := Position{}
		if parentFlag == 1 {
			parent, next, ok = frame.ReadTag(decoded.Payload, pos, limits.MaxStringBytes)
			if !ok {
				return nil, nil, frame.ErrInvalidFrame
			}
			pos = next
		}
		runeValue, next, ok := frame.ReadUvarint(decoded.Payload, pos)
		if !ok || runeValue > uint64(^uint32(0)) {
			return nil, nil, frame.ErrInvalidFrame
		}
		pos = next
		item := node{parent: parent, rune: rune(runeValue)}
		if err := validateDelta(Delta{nodes: map[Position]node{id: item}}); err != nil {
			return nil, nil, frame.ErrInvalidFrame
		}
		nodes[id], previous = item, id
	}
	tombCount, next, ok := frame.ReadUvarint(decoded.Payload, pos)
	if !ok || tombCount > uint64(limits.MaxTags-int(count)) {
		return nil, nil, frame.ErrInvalidFrame
	}
	pos = next
	tombstones := make(map[Position]struct{}, int(tombCount))
	var previousTomb Position
	for index := uint64(0); index < tombCount; index++ {
		id, next, ok := frame.ReadTag(decoded.Payload, pos, limits.MaxStringBytes)
		if !ok || (index > 0 && previousTomb.Compare(id) >= 0) {
			return nil, nil, frame.ErrInvalidFrame
		}
		tombstones[id] = struct{}{}
		previousTomb = id
		pos = next
	}
	if pos != len(decoded.Payload) || !acyclicAgainst(nodes, nil) {
		return nil, nil, frame.ErrInvalidFrame
	}
	if complete && !hasCompleteParents(nodes) {
		return nil, nil, frame.ErrInvalidFrame
	}
	return nodes, tombstones, nil
}

func addWireTagSize(payloadSize *int, tag Position, limits frame.DecoderLimits) error {
	if len(tag.ReplicaID) > limits.MaxStringBytes {
		return frame.ErrFrameLimit
	}
	additional := frame.TagSize(tag)
	if additional > limits.MaxPayload-*payloadSize {
		return frame.ErrFrameLimit
	}
	*payloadSize += additional
	return nil
}

func hasCompleteParents(nodes map[Position]node) bool {
	for _, item := range nodes {
		if item.parent.Valid() {
			if _, ok := nodes[item.parent]; !ok {
				return false
			}
		}
	}
	return true
}
func sortedNodeIDs(nodes map[Position]node) []Position {
	ids := make([]Position, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Compare(ids[j]) < 0 })
	return ids
}
func sortedTombstoneIDs(tombstones map[Position]struct{}) []Position {
	ids := make([]Position, 0, len(tombstones))
	for id := range tombstones {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Compare(ids[j]) < 0 })
	return ids
}
func recordFrontier(frontier map[string]crdt.Tag, tag Position) {
	if current, ok := frontier[tag.ReplicaID]; !ok || current.Compare(tag) < 0 {
		frontier[tag.ReplicaID] = tag
	}
}

func frontierForState(nodes map[Position]node, tombstones map[Position]struct{}) map[string]crdt.Tag {
	frontier := make(map[string]crdt.Tag)
	for id, item := range nodes {
		recordFrontier(frontier, id)
		if item.parent.Valid() {
			recordFrontier(frontier, item.parent)
		}
	}
	for id := range tombstones {
		recordFrontier(frontier, id)
	}
	return frontier
}
