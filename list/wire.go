package list

import (
	"bytes"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

// MarshalBinary returns a canonical complete list state frame. Pending
// out-of-order dependencies are deliberately not serializable as a snapshot.
func (r *RGA[T]) MarshalBinary() ([]byte, error) {
	return r.MarshalBinaryWithLimits(frame.DefaultLimits())
}

// MarshalBinaryWithLimits returns a canonical complete list state constrained
// by caller-selected transport limits.
func (r *RGA[T]) MarshalBinaryWithLimits(limits frame.DecoderLimits) ([]byte, error) {
	if r == nil {
		return nil, ErrNilList
	}
	r.mu.RLock()
	if len(r.pending) > 0 {
		r.mu.RUnlock()
		return nil, ErrIncompleteState
	}
	nodes, tombstones, codecID := cloneNodes(r.nodes), cloneTombstones(r.tombstones), r.codecID
	r.mu.RUnlock()
	return marshalRGA(crdt.TypeIDListRGAState, codecID, nodes, tombstones, limits)
}

// MarshalBinary returns one canonical list delta frame.
func (d Delta) MarshalBinary() ([]byte, error) {
	return d.MarshalBinaryWithLimits(frame.DefaultLimits())
}

// MarshalBinaryWithLimits returns a canonical bounded list delta frame.
func (d Delta) MarshalBinaryWithLimits(limits frame.DecoderLimits) ([]byte, error) {
	if err := validateDelta(d); err != nil {
		return nil, err
	}
	return marshalRGA(crdt.TypeIDListRGADelta, d.codecID, d.nodes, d.tombstones, limits)
}

func marshalRGA(typeID uint64, codecID string, nodes map[Position]node, tombstones map[Position]struct{}, limits frame.DecoderLimits) ([]byte, error) {
	delta := Delta{codecID: codecID, nodes: nodes, tombstones: tombstones}
	if err := validateDelta(delta); err != nil {
		return nil, err
	}
	if typeID == crdt.TypeIDListRGAState && !completeParents(nodes) {
		return nil, ErrIncompleteState
	}
	if len(codecID) > limits.MaxStringBytes || len(nodes) > limits.MaxElements || len(nodes) > limits.MaxTags || len(tombstones) > limits.MaxTags-len(nodes) {
		return nil, frame.ErrFrameLimit
	}
	ids := sortedNodeIDs(nodes)
	tombIDs := sortedTombstoneIDs(tombstones)
	payloadSize := frame.UvarintSize(uint64(len(ids)))
	for _, id := range ids {
		item := nodes[id]
		if err := addTagSize(&payloadSize, id, limits); err != nil {
			return nil, err
		}
		additional := frame.UvarintSize(uint64(len(item.value))) + len(item.value)
		if item.parent.Valid() {
			if len(item.parent.ReplicaID) > limits.MaxStringBytes {
				return nil, frame.ErrFrameLimit
			}
			additional += 1 + frame.TagSize(item.parent)
		} else {
			additional++
		}
		if len(item.value) > limits.MaxStringBytes || additional > limits.MaxPayload-payloadSize {
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
		if err := addTagSize(&payloadSize, id, limits); err != nil {
			return nil, err
		}
	}
	return frame.MarshalFrameWithPayload(typeID, codecID, payloadSize, func(payload []byte) error {
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
			output = frame.AppendUvarint(output, uint64(len(item.value)))
			output = append(output, item.value...)
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

// UnmarshalDelta decodes a bounded canonical list delta for codec.
func UnmarshalDelta[T any](data []byte, codec ElementCodec[T]) (Delta, error) {
	return UnmarshalDeltaWithLimits(data, codec, frame.DefaultLimits())
}

// UnmarshalDeltaWithLimits decodes a bounded canonical list delta for codec.
func UnmarshalDeltaWithLimits[T any](data []byte, codec ElementCodec[T], limits frame.DecoderLimits) (Delta, error) {
	codecID, err := codecIdentifier(codec)
	if err != nil {
		return Delta{}, err
	}
	nodes, tombstones, err := unmarshalRGA(data, crdt.TypeIDListRGADelta, codecID, limits, false)
	if err != nil {
		return Delta{}, err
	}
	delta := Delta{codecID: codecID, nodes: nodes, tombstones: tombstones}
	if err := validateCodecValues(delta, codec); err != nil {
		return Delta{}, err
	}
	return delta, nil
}

// UnmarshalBinary validates a full list state before atomically replacing r.
func (r *RGA[T]) UnmarshalBinary(data []byte) error {
	return r.UnmarshalBinaryWithLimits(data, frame.DefaultLimits())
}

// UnmarshalBinaryWithLimits validates a bounded full state before atomically
// replacing r. It refuses incomplete states and non-canonical values.
func (r *RGA[T]) UnmarshalBinaryWithLimits(data []byte, limits frame.DecoderLimits) error {
	if r == nil || r.clock == nil {
		return ErrNilList
	}
	nodes, tombstones, err := unmarshalRGA(data, crdt.TypeIDListRGAState, r.codecID, limits, true)
	if err != nil {
		return err
	}
	delta := Delta{codecID: r.codecID, nodes: nodes, tombstones: tombstones}
	if err := r.validateValues(delta); err != nil {
		return err
	}
	if len(nodes) > r.options.MaxNodes || len(tombstones) > r.options.MaxTombstones {
		return ErrResourceLimit
	}
	sequence, children, err := buildSequence[T](nodes, tombstones)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if greatest, ok := greatestTag(delta); ok {
		if err := r.clock.Witness(greatest); err != nil {
			return err
		}
	}
	r.nodes = nodes
	r.pending = make(map[Position]node)
	r.waitingByParent = make(map[Position]map[Position]struct{})
	r.pendingBytes = 0
	r.tombstones = tombstones
	r.sequence = sequence
	r.children = children
	r.version++
	return nil
}

// MarshalBinaryWithClockState returns one complete frame and the HLC state
// that must be durably stored with it.
func (r *RGA[T]) MarshalBinaryWithClockState() ([]byte, clock.State, error) {
	if r == nil || r.clock == nil {
		return nil, clock.State{}, ErrNilList
	}
	r.mu.RLock()
	if len(r.pending) > 0 {
		r.mu.RUnlock()
		return nil, clock.State{}, ErrIncompleteState
	}
	nodes, tombstones, state, codecID := cloneNodes(r.nodes), cloneTombstones(r.tombstones), r.clock.Snapshot(), r.codecID
	r.mu.RUnlock()
	encoded, err := marshalRGA(crdt.TypeIDListRGAState, codecID, nodes, tombstones, frame.DefaultLimits())
	return encoded, state, err
}

// SnapshotCurrentState creates a complete HLC-backed list snapshot.
func (r *RGA[T]) SnapshotCurrentState() (snapshot.Snapshot, error) {
	if r == nil || r.clock == nil {
		return snapshot.Snapshot{}, ErrNilList
	}
	r.mu.RLock()
	if len(r.pending) > 0 {
		r.mu.RUnlock()
		return snapshot.Snapshot{}, ErrIncompleteState
	}
	nodes, tombstones, state, codecID := cloneNodes(r.nodes), cloneTombstones(r.tombstones), r.clock.Snapshot(), r.codecID
	r.mu.RUnlock()
	encoded, err := marshalRGA(crdt.TypeIDListRGAState, codecID, nodes, tombstones, frame.DefaultLimits())
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	return snapshot.NewWithClockState(encoded, frontierForState(nodes, tombstones), state)
}

// NewFromSnapshot restores a complete HLC-backed list snapshot.
func NewFromSnapshot[T any](saved snapshot.Snapshot, codec ElementCodec[T]) (*RGA[T], error) {
	return NewFromSnapshotWithOptions(saved, codec, DefaultOptions(), frame.DefaultLimits())
}

// NewFromSnapshotWithOptions restores a complete snapshot within caller
// retention and decoder limits.
func NewFromSnapshotWithOptions[T any](saved snapshot.Snapshot, codec ElementCodec[T], options Options, limits frame.DecoderLimits) (*RGA[T], error) {
	if saved.TypeID != crdt.TypeIDListRGAState {
		return nil, ErrInvalidDelta
	}
	state, ok := saved.ClockState()
	if !ok {
		return nil, ErrInvalidDelta
	}
	r, err := NewFromClockWithOptions(state, codec, options)
	if err != nil {
		return nil, err
	}
	if err := r.UnmarshalBinaryWithLimits(saved.Bytes(), limits); err != nil {
		return nil, err
	}
	if greatest, ok := greatestFrontier(saved.Frontier()); ok {
		if err := r.clock.Witness(greatest); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func unmarshalRGA(data []byte, expectedType uint64, codecID string, limits frame.DecoderLimits, complete bool) (map[Position]node, map[Position]struct{}, error) {
	decoded, err := frame.UnmarshalFrame(data, limits)
	if err != nil {
		return nil, nil, err
	}
	if decoded.TypeID != expectedType || decoded.CodecID != codecID {
		return nil, nil, frame.ErrInvalidFrame
	}
	position := 0
	count, next, ok := frame.ReadUvarint(decoded.Payload, position)
	if !ok || count > uint64(limits.MaxElements) || count > uint64(limits.MaxTags) {
		return nil, nil, frame.ErrInvalidFrame
	}
	position = next
	nodes := make(map[Position]node, int(count))
	var previous Position
	for index := uint64(0); index < count; index++ {
		id, afterID, ok := frame.ReadTag(decoded.Payload, position, limits.MaxStringBytes)
		if !ok || (index > 0 && previous.Compare(id) >= 0) {
			return nil, nil, frame.ErrInvalidFrame
		}
		position = afterID
		parentFlag, next, ok := frame.ReadUvarint(decoded.Payload, position)
		if !ok || parentFlag > 1 {
			return nil, nil, frame.ErrInvalidFrame
		}
		position = next
		parent := Position{}
		if parentFlag == 1 {
			parent, next, ok = frame.ReadTag(decoded.Payload, position, limits.MaxStringBytes)
			if !ok {
				return nil, nil, frame.ErrInvalidFrame
			}
			position = next
		}
		value, next, ok := frame.ReadBytes(decoded.Payload, position, limits.MaxStringBytes)
		if !ok {
			return nil, nil, frame.ErrInvalidFrame
		}
		position = next
		item := node{parent: parent, value: append([]byte(nil), value...)}
		if err := validateDelta(Delta{codecID: codecID, nodes: map[Position]node{id: item}, tombstones: map[Position]struct{}{}}); err != nil {
			return nil, nil, frame.ErrInvalidFrame
		}
		nodes[id], previous = item, id
	}
	tombCount, next, ok := frame.ReadUvarint(decoded.Payload, position)
	if !ok || tombCount > uint64(limits.MaxTags-int(count)) {
		return nil, nil, frame.ErrInvalidFrame
	}
	position = next
	tombstones := make(map[Position]struct{}, int(tombCount))
	var previousTomb Position
	for index := uint64(0); index < tombCount; index++ {
		id, next, ok := frame.ReadTag(decoded.Payload, position, limits.MaxStringBytes)
		if !ok || (index > 0 && previousTomb.Compare(id) >= 0) {
			return nil, nil, frame.ErrInvalidFrame
		}
		position = next
		tombstones[id] = struct{}{}
		previousTomb = id
	}
	if position != len(decoded.Payload) || !acyclicAgainst(nodes, nil) || (complete && !completeParents(nodes)) {
		return nil, nil, frame.ErrInvalidFrame
	}
	return nodes, tombstones, nil
}

func validateCodecValues[T any](delta Delta, codec ElementCodec[T]) error {
	for _, item := range delta.nodes {
		decoded, err := unmarshalCodec(codec, item.value)
		if err != nil {
			return ErrInvalidCodec
		}
		canonical, err := marshalCodec(codec, decoded)
		if err != nil || !bytes.Equal(canonical, item.value) {
			return ErrInvalidCodec
		}
	}
	return nil
}

func completeParents(nodes map[Position]node) bool {
	for _, item := range nodes {
		if item.parent.Valid() {
			if _, exists := nodes[item.parent]; !exists {
				return false
			}
		}
	}
	return true
}

func addTagSize(payloadSize *int, tag Position, limits frame.DecoderLimits) error {
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

func sortedNodeIDs(nodes map[Position]node) []Position {
	ids := make([]Position, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sortPositions(ids)
	return ids
}

func sortedTombstoneIDs(tombstones map[Position]struct{}) []Position {
	ids := make([]Position, 0, len(tombstones))
	for id := range tombstones {
		ids = append(ids, id)
	}
	sortPositions(ids)
	return ids
}

func frontierForState(nodes map[Position]node, tombstones map[Position]struct{}) map[string]crdt.Tag {
	frontier := make(map[string]crdt.Tag)
	record := func(tag Position) {
		if current, ok := frontier[tag.ReplicaID]; !ok || current.Compare(tag) < 0 {
			frontier[tag.ReplicaID] = tag
		}
	}
	for id, item := range nodes {
		record(id)
		if item.parent.Valid() {
			record(item.parent)
		}
	}
	for id := range tombstones {
		record(id)
	}
	return frontier
}

func greatestFrontier(frontier map[string]crdt.Tag) (crdt.Tag, bool) {
	var greatest crdt.Tag
	found := false
	for _, tag := range frontier {
		if !found || greatest.Compare(tag) < 0 {
			greatest, found = tag, true
		}
	}
	return greatest, found
}
