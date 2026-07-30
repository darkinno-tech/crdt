package list

import (
	"sort"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
)

// UnmarshalMoveDelta decodes a bounded canonical move-sequence delta.
func UnmarshalMoveDelta[T any](data []byte, codec ElementCodec[T]) (MoveDelta, error) {
	return UnmarshalMoveDeltaWithLimits(data, codec, frame.DefaultLimits())
}

// UnmarshalMoveDeltaWithLimits decodes a bounded canonical move-sequence
// delta and verifies that every application value round-trips canonically.
func UnmarshalMoveDeltaWithLimits[T any](data []byte, codec ElementCodec[T], limits frame.DecoderLimits) (MoveDelta, error) {
	codecID, err := codecIdentifier(codec)
	if err != nil {
		return MoveDelta{}, err
	}
	delta, err := unmarshalMoveRGA(data, crdt.TypeIDMoveRGADelta, codecID, limits, false)
	if err != nil {
		return MoveDelta{}, err
	}
	if err := validateMoveCodecValues(delta, codec); err != nil {
		return MoveDelta{}, err
	}
	return delta, nil
}

func marshalMoveRGA(typeID uint64, delta MoveDelta, limits frame.DecoderLimits) ([]byte, error) {
	if validateMoveDelta(delta) != nil || len(delta.codecID) > limits.MaxStringBytes || len(delta.nodes) > limits.MaxElements || len(delta.nodes)+len(delta.moves) > limits.MaxTags || len(delta.tombstones) > limits.MaxTags {
		return nil, frame.ErrFrameLimit
	}
	if typeID == crdt.TypeIDMoveRGAState && !completeMoveState(delta) {
		return nil, ErrIncompleteState
	}
	nodes := sortedNodeIDs(delta.nodes)
	tombstones := sortedTombstoneIDs(delta.tombstones)
	moves := sortedMoveIDs(delta.moves)
	payloadSize := frame.UvarintSize(uint64(len(nodes)))
	for _, id := range nodes {
		item := delta.nodes[id]
		if err := addTagSize(&payloadSize, id, limits); err != nil {
			return nil, err
		}
		additional := 1 + frame.UvarintSize(uint64(len(item.value))) + len(item.value)
		if item.parent.Valid() {
			additional += frame.TagSize(item.parent)
		}
		if len(item.value) > limits.MaxStringBytes || additional > limits.MaxPayload-payloadSize {
			return nil, frame.ErrFrameLimit
		}
		payloadSize += additional
	}
	additional := frame.UvarintSize(uint64(len(tombstones)))
	if additional > limits.MaxPayload-payloadSize {
		return nil, frame.ErrFrameLimit
	}
	payloadSize += additional
	for _, id := range tombstones {
		if err := addTagSize(&payloadSize, id, limits); err != nil {
			return nil, err
		}
	}
	additional = frame.UvarintSize(uint64(len(moves)))
	if additional > limits.MaxPayload-payloadSize {
		return nil, frame.ErrFrameLimit
	}
	payloadSize += additional
	for _, id := range moves {
		record := delta.moves[id]
		if err := addTagSize(&payloadSize, id, limits); err != nil {
			return nil, err
		}
		if err := addTagSize(&payloadSize, record.tag, limits); err != nil {
			return nil, err
		}
		additional := 1 + frame.UvarintSize(record.rank)
		if record.anchor.Valid() {
			additional += frame.TagSize(record.anchor)
		}
		if additional > limits.MaxPayload-payloadSize {
			return nil, frame.ErrFrameLimit
		}
		payloadSize += additional
	}
	return frame.MarshalFrameWithPayload(typeID, delta.codecID, payloadSize, func(payload []byte) error {
		output := frame.AppendUvarint(payload[:0], uint64(len(nodes)))
		for _, id := range nodes {
			item := delta.nodes[id]
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
		output = frame.AppendUvarint(output, uint64(len(tombstones)))
		for _, id := range tombstones {
			output = frame.AppendTag(output, id)
		}
		output = frame.AppendUvarint(output, uint64(len(moves)))
		for _, id := range moves {
			record := delta.moves[id]
			output = frame.AppendTag(output, id)
			output = frame.AppendTag(output, record.tag)
			if record.anchor.Valid() {
				output = frame.AppendUvarint(output, 1)
				output = frame.AppendTag(output, record.anchor)
			} else {
				output = frame.AppendUvarint(output, 0)
			}
			output = frame.AppendUvarint(output, record.rank)
		}
		if len(output) != payloadSize {
			return frame.ErrInvalidFrame
		}
		return nil
	})
}

func unmarshalMoveRGA(data []byte, expectedType uint64, codecID string, limits frame.DecoderLimits, complete bool) (MoveDelta, error) {
	decoded, err := frame.UnmarshalFrame(data, limits)
	if err != nil {
		return MoveDelta{}, err
	}
	if decoded.TypeID != expectedType || decoded.CodecID != codecID {
		return MoveDelta{}, frame.ErrInvalidFrame
	}
	position := 0
	nodeCount, next, ok := frame.ReadUvarint(decoded.Payload, position)
	if !ok || nodeCount > uint64(limits.MaxElements) || nodeCount > uint64(limits.MaxTags) {
		return MoveDelta{}, frame.ErrInvalidFrame
	}
	position = next
	delta := MoveDelta{codecID: codecID, nodes: make(map[Position]node, int(nodeCount)), tombstones: map[Position]struct{}{}, moves: map[Position]moveRecord{}}
	var previous Position
	for index := uint64(0); index < nodeCount; index++ {
		id, next, ok := frame.ReadTag(decoded.Payload, position, limits.MaxStringBytes)
		if !ok || (index > 0 && previous.Compare(id) >= 0) {
			return MoveDelta{}, frame.ErrInvalidFrame
		}
		position = next
		parentFlag, next, ok := frame.ReadUvarint(decoded.Payload, position)
		if !ok || parentFlag > 1 {
			return MoveDelta{}, frame.ErrInvalidFrame
		}
		position = next
		item := node{}
		if parentFlag == 1 {
			item.parent, next, ok = frame.ReadTag(decoded.Payload, position, limits.MaxStringBytes)
			if !ok {
				return MoveDelta{}, frame.ErrInvalidFrame
			}
			position = next
		}
		item.value, next, ok = frame.ReadBytes(decoded.Payload, position, limits.MaxStringBytes)
		if !ok {
			return MoveDelta{}, frame.ErrInvalidFrame
		}
		item.value = append([]byte(nil), item.value...)
		position, previous = next, id
		delta.nodes[id] = item
	}
	tombstoneCount, next, ok := frame.ReadUvarint(decoded.Payload, position)
	if !ok || tombstoneCount > uint64(limits.MaxTags) {
		return MoveDelta{}, frame.ErrInvalidFrame
	}
	position = next
	previous = Position{}
	for index := uint64(0); index < tombstoneCount; index++ {
		id, next, ok := frame.ReadTag(decoded.Payload, position, limits.MaxStringBytes)
		if !ok || (index > 0 && previous.Compare(id) >= 0) {
			return MoveDelta{}, frame.ErrInvalidFrame
		}
		position, previous = next, id
		delta.tombstones[id] = struct{}{}
	}
	moveCount, next, ok := frame.ReadUvarint(decoded.Payload, position)
	if !ok || moveCount > uint64(limits.MaxTags) {
		return MoveDelta{}, frame.ErrInvalidFrame
	}
	position = next
	previous = Position{}
	for index := uint64(0); index < moveCount; index++ {
		id, next, ok := frame.ReadTag(decoded.Payload, position, limits.MaxStringBytes)
		if !ok || (index > 0 && previous.Compare(id) >= 0) {
			return MoveDelta{}, frame.ErrInvalidFrame
		}
		position = next
		tag, next, ok := frame.ReadTag(decoded.Payload, position, limits.MaxStringBytes)
		if !ok {
			return MoveDelta{}, frame.ErrInvalidFrame
		}
		position = next
		anchorFlag, next, ok := frame.ReadUvarint(decoded.Payload, position)
		if !ok || anchorFlag > 1 {
			return MoveDelta{}, frame.ErrInvalidFrame
		}
		position = next
		record := moveRecord{tag: tag}
		if anchorFlag == 1 {
			record.anchor, next, ok = frame.ReadTag(decoded.Payload, position, limits.MaxStringBytes)
			if !ok {
				return MoveDelta{}, frame.ErrInvalidFrame
			}
			position = next
		}
		record.rank, next, ok = frame.ReadUvarint(decoded.Payload, position)
		if !ok {
			return MoveDelta{}, frame.ErrInvalidFrame
		}
		position, previous = next, id
		delta.moves[id] = record
	}
	if position != len(decoded.Payload) || validateMoveDelta(delta) != nil || (complete && !completeMoveState(delta)) {
		return MoveDelta{}, frame.ErrInvalidFrame
	}
	return delta, nil
}

func validateMoveCodecValues[T any](delta MoveDelta, codec ElementCodec[T]) error {
	for _, item := range delta.nodes {
		decoded, err := codec.Unmarshal(item.value)
		if err != nil {
			return ErrInvalidCodec
		}
		canonical, err := codec.Marshal(decoded)
		if err != nil || string(canonical) != string(item.value) {
			return ErrInvalidCodec
		}
	}
	return nil
}

func completeMoveState(delta MoveDelta) bool {
	for _, item := range delta.nodes {
		if item.parent.Valid() {
			if _, exists := delta.nodes[item.parent]; !exists {
				return false
			}
		}
	}
	for id, record := range delta.moves {
		if _, exists := delta.nodes[id]; !exists {
			return false
		}
		if record.anchor.Valid() {
			if _, exists := delta.nodes[record.anchor]; !exists {
				return false
			}
		}
	}
	return true
}

func sortedMoveIDs(moves map[Position]moveRecord) []Position {
	ids := make([]Position, 0, len(moves))
	for id := range moves {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left].Compare(ids[right]) < 0 })
	return ids
}

func moveFrontier(nodes map[Position]node, tombstones map[Position]struct{}, moves map[Position]moveRecord) map[string]crdt.Tag {
	frontier := frontierForState(nodes, tombstones)
	record := func(tag Position) {
		if current, ok := frontier[tag.ReplicaID]; !ok || current.Compare(tag) < 0 {
			frontier[tag.ReplicaID] = tag
		}
	}
	for id, move := range moves {
		record(id)
		record(move.tag)
		if move.anchor.Valid() {
			record(move.anchor)
		}
	}
	return frontier
}
