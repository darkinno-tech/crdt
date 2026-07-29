package text

import (
	"bytes"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

const (
	runBlockNode uint64 = iota
	runBlockChain
)

type runNode struct {
	id   Position
	item node
}

// runChildKey groups potential run successors by their parent and replica. A
// summary avoids allocating one slice per parent while preserving the exact
// "one unused same-replica child" rule used by canonical run construction.
type runChildKey struct {
	parent    Position
	replicaID string
}

type runChild struct {
	id    Position
	count int
}

// MarshalRunBinary encodes complete RGA state using the separately negotiated
// run-v2 frame. It retains v1 scalar Positions and is therefore safe to merge
// with v1 deltas after decoding.
func (r *RGA) MarshalRunBinary() ([]byte, error) {
	return r.MarshalRunBinaryWithLimits(frame.DefaultLimits())
}

// MarshalRunBinaryWithLimits encodes complete RGA state using the run-v2
// frame while enforcing caller-selected output limits.
func (r *RGA) MarshalRunBinaryWithLimits(limits frame.DecoderLimits) ([]byte, error) {
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
	return marshalRGARun(crdt.TypeIDRGARunState, nodes, tombstones, limits)
}

// MarshalRunBinary encodes a delta with compact same-replica parent chains.
func (d Delta) MarshalRunBinary() ([]byte, error) {
	return d.MarshalRunBinaryWithLimits(frame.DefaultLimits())
}

// MarshalRunBinaryWithLimits encodes a delta with compact same-replica parent
// chains while enforcing caller-selected output limits.
func (d Delta) MarshalRunBinaryWithLimits(limits frame.DecoderLimits) ([]byte, error) {
	return marshalRGARun(crdt.TypeIDRGARunDelta, d.nodes, d.tombstones, limits)
}

func marshalRGARun(typeID uint64, nodes map[Position]node, tombstones map[Position]struct{}, limits frame.DecoderLimits) ([]byte, error) {
	delta := Delta{nodes: nodes, tombstones: tombstones}
	if err := validateDelta(delta); err != nil {
		return nil, err
	}
	if typeID == crdt.TypeIDRGARunState && !hasCompleteParents(nodes) {
		return nil, ErrIncompleteState
	}
	if len(nodes) > limits.MaxElements || len(nodes) > limits.MaxTags || len(tombstones) > limits.MaxTags-len(nodes) {
		return nil, frame.ErrFrameLimit
	}
	blocks := makeRunBlocks(nodes)
	payloadSize, err := runPayloadSize(blocks, tombstones, limits)
	if err != nil {
		return nil, err
	}
	return frame.MarshalFrameWithPayload(typeID, "", payloadSize, func(payload []byte) error {
		output := frame.AppendUvarint(payload[:0], uint64(len(blocks)))
		for _, block := range blocks {
			if len(block) == 1 {
				output = frame.AppendUvarint(output, runBlockNode)
				output = appendRunNode(output, block[0])
				continue
			}
			output = frame.AppendUvarint(output, runBlockChain)
			output = frame.AppendUvarint(output, uint64(len(block)))
			output = frame.AppendUvarint(output, uint64(len(block[0].id.ReplicaID)))
			output = append(output, block[0].id.ReplicaID...)
			if block[0].item.parent.Valid() {
				output = frame.AppendUvarint(output, 1)
				output = frame.AppendTag(output, block[0].item.parent)
			} else {
				output = frame.AppendUvarint(output, 0)
			}
			for _, item := range block {
				output = frame.AppendUvarint(output, item.id.WallTime)
				output = frame.AppendUvarint(output, item.id.Logical)
				output = frame.AppendUvarint(output, uint64(item.item.rune))
			}
		}
		tombIDs := sortedTombstoneIDs(tombstones)
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

func appendRunNode(output []byte, item runNode) []byte {
	output = frame.AppendTag(output, item.id)
	if item.item.parent.Valid() {
		output = frame.AppendUvarint(output, 1)
		output = frame.AppendTag(output, item.item.parent)
	} else {
		output = frame.AppendUvarint(output, 0)
	}
	return frame.AppendUvarint(output, uint64(item.item.rune))
}

func runPayloadSize(blocks [][]runNode, tombstones map[Position]struct{}, limits frame.DecoderLimits) (int, error) {
	size := frame.UvarintSize(uint64(len(blocks)))
	for _, block := range blocks {
		if len(block) == 1 {
			item := block[0]
			if err := addWireTagSize(&size, item.id, limits); err != nil {
				return 0, err
			}
			additional := frame.UvarintSize(uint64(item.item.rune)) + 1
			if item.item.parent.Valid() {
				if len(item.item.parent.ReplicaID) > limits.MaxStringBytes {
					return 0, frame.ErrFrameLimit
				}
				additional += frame.TagSize(item.item.parent)
			}
			if additional > limits.MaxPayload-size-1 {
				return 0, frame.ErrFrameLimit
			}
			size += 1 + additional
			continue
		}
		if len(block[0].id.ReplicaID) > limits.MaxStringBytes {
			return 0, frame.ErrFrameLimit
		}
		additional := frame.UvarintSize(uint64(len(block))) + frame.UvarintSize(uint64(len(block[0].id.ReplicaID))) + len(block[0].id.ReplicaID) + 1
		if block[0].item.parent.Valid() {
			if len(block[0].item.parent.ReplicaID) > limits.MaxStringBytes {
				return 0, frame.ErrFrameLimit
			}
			additional += frame.TagSize(block[0].item.parent)
		}
		for _, item := range block {
			additional += frame.UvarintSize(item.id.WallTime) + frame.UvarintSize(item.id.Logical) + frame.UvarintSize(uint64(item.item.rune))
		}
		if additional > limits.MaxPayload-size-1 {
			return 0, frame.ErrFrameLimit
		}
		size += 1 + additional
	}
	additional := frame.UvarintSize(uint64(len(tombstones)))
	if additional > limits.MaxPayload-size {
		return 0, frame.ErrFrameLimit
	}
	size += additional
	for _, id := range sortedTombstoneIDs(tombstones) {
		if err := addWireTagSize(&size, id, limits); err != nil {
			return 0, err
		}
	}
	return size, nil
}

func makeRunBlocks(nodes map[Position]node) [][]runNode {
	ids := sortedNodeIDs(nodes)
	if len(ids) == 0 {
		return nil
	}
	if block, ok := singleRunBlock(ids, nodes); ok {
		return [][]runNode{block}
	}
	return makeRunBlocksFromSortedIDs(ids, nodes)
}

// singleRunBlock recognizes the common local-insert shape without building a
// parent index: canonical tag order is also the parent chain and every node
// belongs to one replica. Partial deltas may begin at an external parent, so
// only later links are required to point inside the block.
func singleRunBlock(ids []Position, nodes map[Position]node) ([]runNode, bool) {
	replicaID := ids[0].ReplicaID
	for index, id := range ids {
		item := nodes[id]
		if id.ReplicaID != replicaID || (index > 0 && item.parent != ids[index-1]) {
			return nil, false
		}
	}
	block := make([]runNode, len(ids))
	for index, id := range ids {
		block[index] = runNode{id: id, item: nodes[id]}
	}
	return block, true
}

func makeRunBlocksFromSortedIDs(ids []Position, nodes map[Position]node) [][]runNode {
	children := make(map[runChildKey]runChild, len(nodes))
	for id, item := range nodes {
		key := runChildKey{parent: item.parent, replicaID: id.ReplicaID}
		child := children[key]
		child.count++
		if child.count == 1 {
			child.id = id
		}
		children[key] = child
	}
	used := make(map[Position]struct{}, len(nodes))
	blocks := make([][]runNode, 0)
	items := make([]runNode, 0, len(nodes))
	for _, id := range ids {
		if _, exists := used[id]; exists {
			continue
		}
		start := len(items)
		items = append(items, runNode{id: id, item: nodes[id]})
		used[id] = struct{}{}
		for {
			current := items[len(items)-1]
			next, ok := children[runChildKey{parent: current.id, replicaID: id.ReplicaID}]
			if !ok || next.count != 1 {
				break
			}
			if _, exists := used[next.id]; exists {
				break
			}
			items = append(items, runNode{id: next.id, item: nodes[next.id]})
			used[next.id] = struct{}{}
		}
		blocks = append(blocks, items[start:])
	}
	return blocks
}

// UnmarshalRGARunDelta decodes a bounded run-v2 delta.
func UnmarshalRGARunDelta(data []byte) (Delta, error) {
	return unmarshalRGARunDeltaWithLimits(data, frame.DefaultLimits())
}

// UnmarshalRGARunDeltaWithLimits decodes a bounded run-v2 delta while
// enforcing caller-selected input limits.
func UnmarshalRGARunDeltaWithLimits(data []byte, limits frame.DecoderLimits) (Delta, error) {
	return unmarshalRGARunDeltaWithLimits(data, limits)
}

func unmarshalRGARunDeltaWithLimits(data []byte, limits frame.DecoderLimits) (Delta, error) {
	nodes, tombstones, err := unmarshalRGARun(data, crdt.TypeIDRGARunDelta, limits, false)
	if err != nil {
		return Delta{}, err
	}
	return Delta{nodes: nodes, tombstones: tombstones}, nil
}

// UnmarshalRunBinary installs one complete run-v2 RGA state frame.
func (r *RGA) UnmarshalRunBinary(data []byte) error {
	return r.UnmarshalRunBinaryWithLimits(data, frame.DefaultLimits())
}

// UnmarshalRunBinaryWithLimits installs one complete run-v2 RGA state frame
// while enforcing caller-selected input limits.
func (r *RGA) UnmarshalRunBinaryWithLimits(data []byte, limits frame.DecoderLimits) error {
	if r == nil || r.clock == nil {
		return ErrNilText
	}
	nodes, tombstones, err := unmarshalRGARun(data, crdt.TypeIDRGARunState, limits, true)
	if err != nil {
		return err
	}
	return r.installState(nodes, tombstones)
}

func (r *RGA) installState(nodes map[Position]node, tombstones map[Position]struct{}) error {
	if len(nodes) > r.options.MaxNodes || len(tombstones) > r.options.MaxTombstones {
		return ErrResourceLimit
	}
	sequence, children, err := buildSequence(nodes, tombstones)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if tag, ok := greatestTag(Delta{nodes: nodes, tombstones: tombstones}); ok {
		if err := r.clock.Witness(tag); err != nil {
			return err
		}
	}
	r.nodes, r.tombstones = nodes, tombstones
	r.pending = make(map[Position]node)
	r.waitingByParent = make(map[Position]map[Position]struct{})
	r.pendingBytes = 0
	r.sequence, r.children = sequence, children
	r.version++
	return nil
}

func unmarshalRGARun(data []byte, expectedType uint64, limits frame.DecoderLimits, complete bool) (map[Position]node, map[Position]struct{}, error) {
	decoded, err := frame.UnmarshalFrame(data, limits)
	if err != nil || decoded.TypeID != expectedType || decoded.CodecID != "" {
		return nil, nil, frame.ErrInvalidFrame
	}
	position := 0
	blockCount, next, ok := frame.ReadUvarint(decoded.Payload, position)
	if !ok || blockCount > uint64(limits.MaxElements) {
		return nil, nil, frame.ErrInvalidFrame
	}
	position = next
	nodes := make(map[Position]node)
	for blockIndex := uint64(0); blockIndex < blockCount; blockIndex++ {
		kind, next, ok := frame.ReadUvarint(decoded.Payload, position)
		if !ok || kind > runBlockChain {
			return nil, nil, frame.ErrInvalidFrame
		}
		position = next
		if kind == runBlockNode {
			id, item, next, ok := readRunNode(decoded.Payload, position, limits)
			if !ok || len(nodes) >= limits.MaxElements {
				return nil, nil, frame.ErrInvalidFrame
			}
			if _, exists := nodes[id]; exists {
				return nil, nil, frame.ErrInvalidFrame
			}
			nodes[id], position = item, next
			continue
		}
		count, next, ok := frame.ReadUvarint(decoded.Payload, position)
		if !ok || count < 2 || count > uint64(limits.MaxElements-len(nodes)) {
			return nil, nil, frame.ErrInvalidFrame
		}
		position = next
		replicaBytes, next, ok := frame.ReadBytes(decoded.Payload, position, limits.MaxStringBytes)
		if !ok || !(crdt.Tag{ReplicaID: string(replicaBytes)}).Valid() {
			return nil, nil, frame.ErrInvalidFrame
		}
		replicaID := string(replicaBytes)
		position = next
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
		for index := uint64(0); index < count; index++ {
			wallTime, next, ok := frame.ReadUvarint(decoded.Payload, position)
			if !ok {
				return nil, nil, frame.ErrInvalidFrame
			}
			logical, next, ok := frame.ReadUvarint(decoded.Payload, next)
			if !ok {
				return nil, nil, frame.ErrInvalidFrame
			}
			runeValue, next, ok := frame.ReadUvarint(decoded.Payload, next)
			if !ok || runeValue > uint64(^uint32(0)) {
				return nil, nil, frame.ErrInvalidFrame
			}
			id := Position{ReplicaID: replicaID, WallTime: wallTime, Logical: logical}
			if _, exists := nodes[id]; exists {
				return nil, nil, frame.ErrInvalidFrame
			}
			nodes[id] = node{parent: parent, rune: rune(runeValue)}
			parent, position = id, next
		}
	}
	tombCount, next, ok := frame.ReadUvarint(decoded.Payload, position)
	if !ok || tombCount > uint64(limits.MaxTags-len(nodes)) {
		return nil, nil, frame.ErrInvalidFrame
	}
	position = next
	tombstones := make(map[Position]struct{}, int(tombCount))
	for index := uint64(0); index < tombCount; index++ {
		id, next, ok := frame.ReadTag(decoded.Payload, position, limits.MaxStringBytes)
		if !ok {
			return nil, nil, frame.ErrInvalidFrame
		}
		if _, exists := tombstones[id]; exists {
			return nil, nil, frame.ErrInvalidFrame
		}
		tombstones[id], position = struct{}{}, next
	}
	if position != len(decoded.Payload) || validateDelta(Delta{nodes: nodes, tombstones: tombstones}) != nil || !acyclicAgainst(nodes, nil) || (complete && !hasCompleteParents(nodes)) {
		return nil, nil, frame.ErrInvalidFrame
	}
	canonical, err := marshalRGARun(expectedType, nodes, tombstones, limits)
	if err != nil || !bytes.Equal(canonical, data) {
		return nil, nil, frame.ErrInvalidFrame
	}
	return nodes, tombstones, nil
}

func readRunNode(payload []byte, position int, limits frame.DecoderLimits) (Position, node, int, bool) {
	id, next, ok := frame.ReadTag(payload, position, limits.MaxStringBytes)
	if !ok {
		return Position{}, node{}, position, false
	}
	parentFlag, next, ok := frame.ReadUvarint(payload, next)
	if !ok || parentFlag > 1 {
		return Position{}, node{}, position, false
	}
	parent := Position{}
	if parentFlag == 1 {
		parent, next, ok = frame.ReadTag(payload, next, limits.MaxStringBytes)
		if !ok {
			return Position{}, node{}, position, false
		}
	}
	runeValue, next, ok := frame.ReadUvarint(payload, next)
	if !ok || runeValue > uint64(^uint32(0)) {
		return Position{}, node{}, position, false
	}
	return id, node{parent: parent, rune: rune(runeValue)}, next, true
}

// SnapshotRunCurrentState returns an HLC-backed, validated run-v2 snapshot.
func (r *RGA) SnapshotRunCurrentState() (snapshot.Snapshot, error) {
	return r.SnapshotRunCurrentStateWithLimits(frame.DefaultLimits())
}

// SnapshotRunCurrentStateWithLimits returns an HLC-backed, validated run-v2
// snapshot while enforcing caller-selected output limits.
func (r *RGA) SnapshotRunCurrentStateWithLimits(limits frame.DecoderLimits) (snapshot.Snapshot, error) {
	if r == nil || r.clock == nil {
		return snapshot.Snapshot{}, ErrNilText
	}
	r.mu.RLock()
	if len(r.pending) > 0 {
		r.mu.RUnlock()
		return snapshot.Snapshot{}, ErrIncompleteState
	}
	nodes, tombstones, clockState := cloneNodes(r.nodes), cloneTombstones(r.tombstones), r.clock.Snapshot()
	frontier := frontierForState(nodes, tombstones)
	r.mu.RUnlock()
	state, err := marshalRGARun(crdt.TypeIDRGARunState, nodes, tombstones, limits)
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	return snapshot.NewValidatedWithClockState(state, frontier, clockState, validateRGARunState)
}

func validateRGARunState(data []byte) error {
	_, _, err := unmarshalRGARun(data, crdt.TypeIDRGARunState, frame.DefaultLimits(), true)
	return err
}
