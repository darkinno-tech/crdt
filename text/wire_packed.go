package text

import (
	"bytes"
	"math"
	"sort"
	"unicode/utf8"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

const packedRunBlockChain uint64 = 2

// packedRunChain represents a chain whose HLC positions can be reconstructed
// from one starting tag, a transition bitmap, and positive wall-clock gaps.
// A zero bitmap bit increments Logical; a one bit advances WallTime and resets
// Logical to zero. This is exactly the sequence emitted by local HLC.Now.
type packedRunChain struct {
	transitions []byte
	wallDeltas  []uint64
	text        []byte
}

type packedRunBlock struct {
	nodes  []runNode
	chain  packedRunChain
	packed bool
}

// MarshalPackedBinary encodes complete state with the separately negotiated
// packed RGA v3 protocol. v1 and run-v2 bytes remain unchanged and are never
// accepted through this API.
func (r *RGA) MarshalPackedBinary() ([]byte, error) {
	return r.MarshalPackedBinaryWithLimits(frame.DefaultLimits())
}

// MarshalPackedBinaryWithLimits encodes complete packed RGA v3 state while
// enforcing the caller-selected output limits.
func (r *RGA) MarshalPackedBinaryWithLimits(limits frame.DecoderLimits) ([]byte, error) {
	if r == nil {
		return nil, ErrNilText
	}
	state, _, err := r.captureRunState(false)
	if err != nil {
		return nil, err
	}
	return marshalRGAPackedState(state, limits)
}

// MarshalPackedBinary encodes one delta with compact dense local HLC chains.
// Its TypeID is distinct from stable run-v2 and requires PackedFrameType.
func (d Delta) MarshalPackedBinary() ([]byte, error) {
	return d.MarshalPackedBinaryWithLimits(frame.DefaultLimits())
}

// MarshalPackedBinaryWithLimits encodes one bounded compact RGA v3 delta.
func (d Delta) MarshalPackedBinaryWithLimits(limits frame.DecoderLimits) ([]byte, error) {
	return marshalRGAPacked(crdt.TypeIDRGAPackedDelta, d.nodes, d.tombstones, limits)
}

func marshalRGAPacked(typeID uint64, nodes map[Position]node, tombstones map[Position]struct{}, limits frame.DecoderLimits) ([]byte, error) {
	delta := Delta{nodes: nodes, tombstones: tombstones}
	if err := validateDelta(delta); err != nil {
		return nil, err
	}
	if typeID == crdt.TypeIDRGAPackedState && !hasCompleteParents(nodes) {
		return nil, ErrIncompleteState
	}
	if len(nodes) > limits.MaxElements || len(nodes) > limits.MaxTags || len(tombstones) > limits.MaxTags-len(nodes) {
		return nil, frame.ErrFrameLimit
	}
	return marshalPackedRunBlocks(typeID, makePackedRunBlocks(nodes), sortedTombstoneIDs(tombstones), limits)
}

func marshalRGAPackedState(state rgaRunState, limits frame.DecoderLimits) ([]byte, error) {
	if len(state.nodes) > limits.MaxElements || len(state.nodes) > limits.MaxTags || len(state.tombstones) > limits.MaxTags-len(state.nodes) {
		return nil, frame.ErrFrameLimit
	}
	sort.Sort(runNodes(state.nodes))
	sort.Slice(state.tombstones, func(i, j int) bool { return state.tombstones[i].Compare(state.tombstones[j]) < 0 })
	if _, err := validateCompleteRunState(state); err != nil {
		return nil, err
	}
	return marshalPackedRunBlocks(crdt.TypeIDRGAPackedState, makePackedRunBlocksFromSortedItems(state.nodes), state.tombstones, limits)
}

func marshalPackedRunBlocks(typeID uint64, blocks []packedRunBlock, tombstones []Position, limits frame.DecoderLimits) ([]byte, error) {
	payloadSize, err := packedRunPayloadSize(blocks, tombstones, limits)
	if err != nil {
		return nil, err
	}
	return frame.MarshalFrameWithPayloadAndLimits(typeID, "", payloadSize, limits, func(payload []byte) error {
		return writePackedRunPayload(payload, blocks, tombstones)
	})
}

func marshalPackedRunPayload(blocks []packedRunBlock, tombstones []Position, limits frame.DecoderLimits) ([]byte, error) {
	payloadSize, err := packedRunPayloadSize(blocks, tombstones, limits)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, payloadSize)
	if err := writePackedRunPayload(payload, blocks, tombstones); err != nil {
		return nil, err
	}
	return payload, nil
}

func packedRunPayloadSize(blocks []packedRunBlock, tombstones []Position, limits frame.DecoderLimits) (int, error) {
	size := frame.UvarintSize(uint64(len(blocks)))
	for _, block := range blocks {
		blockSize, err := packedRunBlockSize(block, limits)
		if err != nil || blockSize > limits.MaxPayload-size {
			if err != nil {
				return 0, err
			}
			return 0, frame.ErrFrameLimit
		}
		size += blockSize
	}
	additional := frame.UvarintSize(uint64(len(tombstones)))
	if additional > limits.MaxPayload-size {
		return 0, frame.ErrFrameLimit
	}
	size += additional
	for _, id := range tombstones {
		if err := addWireTagSize(&size, id, limits); err != nil {
			return 0, err
		}
	}
	return size, nil
}

func packedRunBlockSize(block packedRunBlock, limits frame.DecoderLimits) (int, error) {
	if len(block.nodes) == 0 {
		return 0, frame.ErrInvalidFrame
	}
	if len(block.nodes) == 1 {
		item := block.nodes[0]
		scalar, ok := encodeRunScalar(item.item.rune)
		if !ok {
			return 0, frame.ErrInvalidFrame
		}
		size := frame.UvarintSize(runBlockNode) + frame.TagSize(item.id) + frame.UvarintSize(scalar) + 1
		if item.item.parent.Valid() {
			size += frame.TagSize(item.item.parent)
		}
		if len(item.id.ReplicaID) > limits.MaxStringBytes || (item.item.parent.Valid() && len(item.item.parent.ReplicaID) > limits.MaxStringBytes) {
			return 0, frame.ErrFrameLimit
		}
		return size, nil
	}
	first := block.nodes[0]
	if len(first.id.ReplicaID) > limits.MaxStringBytes || (first.item.parent.Valid() && len(first.item.parent.ReplicaID) > limits.MaxStringBytes) {
		return 0, frame.ErrFrameLimit
	}
	size := frame.UvarintSize(runBlockChain) + frame.UvarintSize(uint64(len(block.nodes))) + frame.UvarintSize(uint64(len(first.id.ReplicaID))) + len(first.id.ReplicaID) + 1
	if first.item.parent.Valid() {
		size += frame.TagSize(first.item.parent)
	}
	if block.packed {
		size += frame.UvarintSize(first.id.WallTime) + frame.UvarintSize(first.id.Logical)
		size += frame.UvarintSize(uint64(len(block.chain.transitions))) + len(block.chain.transitions)
		for _, gap := range block.chain.wallDeltas {
			size += frame.UvarintSize(gap)
		}
		size += frame.UvarintSize(uint64(len(block.chain.text))) + len(block.chain.text)
		return size, nil
	}
	for _, item := range block.nodes {
		scalar, ok := encodeRunScalar(item.item.rune)
		if !ok {
			return 0, frame.ErrInvalidFrame
		}
		size += frame.UvarintSize(item.id.WallTime) + frame.UvarintSize(item.id.Logical) + frame.UvarintSize(scalar)
	}
	return size, nil
}

func writePackedRunPayload(payload []byte, blocks []packedRunBlock, tombstones []Position) error {
	output := frame.AppendUvarint(payload[:0], uint64(len(blocks)))
	for _, block := range blocks {
		if len(block.nodes) == 1 {
			output = frame.AppendUvarint(output, runBlockNode)
			var err error
			output, err = appendRunNode(output, block.nodes[0])
			if err != nil {
				return err
			}
			continue
		}
		if block.packed {
			output = frame.AppendUvarint(output, packedRunBlockChain)
		} else {
			output = frame.AppendUvarint(output, runBlockChain)
		}
		output = frame.AppendUvarint(output, uint64(len(block.nodes)))
		output = frame.AppendUvarint(output, uint64(len(block.nodes[0].id.ReplicaID)))
		output = append(output, block.nodes[0].id.ReplicaID...)
		if block.nodes[0].item.parent.Valid() {
			output = frame.AppendUvarint(output, 1)
			output = frame.AppendTag(output, block.nodes[0].item.parent)
		} else {
			output = frame.AppendUvarint(output, 0)
		}
		if block.packed {
			output = frame.AppendUvarint(output, block.nodes[0].id.WallTime)
			output = frame.AppendUvarint(output, block.nodes[0].id.Logical)
			output = frame.AppendUvarint(output, uint64(len(block.chain.transitions)))
			output = append(output, block.chain.transitions...)
			for _, gap := range block.chain.wallDeltas {
				output = frame.AppendUvarint(output, gap)
			}
			output = frame.AppendUvarint(output, uint64(len(block.chain.text)))
			output = append(output, block.chain.text...)
			continue
		}
		for _, item := range block.nodes {
			scalar, ok := encodeRunScalar(item.item.rune)
			if !ok {
				return frame.ErrInvalidFrame
			}
			output = frame.AppendUvarint(output, item.id.WallTime)
			output = frame.AppendUvarint(output, item.id.Logical)
			output = frame.AppendUvarint(output, scalar)
		}
	}
	output = frame.AppendUvarint(output, uint64(len(tombstones)))
	for _, id := range tombstones {
		output = frame.AppendTag(output, id)
	}
	if len(output) != len(payload) {
		return frame.ErrInvalidFrame
	}
	return nil
}

func makePackedRunBlocks(nodes map[Position]node) []packedRunBlock {
	return makePackedRunBlocksFromRunBlocks(makeRunBlocks(nodes))
}

func makePackedRunBlocksFromSortedItems(items []runNode) []packedRunBlock {
	return makePackedRunBlocksFromRunBlocks(makeRunBlocksFromSortedItems(items))
}

func makePackedRunBlocksFromRunBlocks(blocks [][]runNode) []packedRunBlock {
	packed := make([]packedRunBlock, 0, len(blocks))
	for _, block := range blocks {
		item := packedRunBlock{nodes: block}
		if len(block) > 1 {
			if chain, ok := makePackedRunChain(block); ok {
				candidate := packedRunBlock{nodes: block, chain: chain, packed: true}
				// Canonical representation must not depend on the local decoder
				// policy. The caller has already enforced its configured bounds;
				// here we only compare the two valid encodings.
				candidateSize, candidateErr := packedRunBlockCanonicalSize(candidate)
				regularSize, regularErr := packedRunBlockCanonicalSize(item)
				if candidateErr == nil && regularErr == nil && candidateSize < regularSize {
					item = candidate
				}
			}
		}
		packed = append(packed, item)
	}
	return packed
}

func packedRunBlockCanonicalSize(block packedRunBlock) (int, error) {
	return packedRunBlockSize(block, frame.DecoderLimits{
		MaxPayload:     maxPackedInt,
		MaxStringBytes: maxPackedInt,
	})
}

func makePackedRunChain(block []runNode) (packedRunChain, bool) {
	if len(block) < 2 {
		return packedRunChain{}, false
	}
	transitionLength, ok := packedTransitionLength(len(block))
	if !ok {
		return packedRunChain{}, false
	}
	previous := block[0].id
	for index, item := range block {
		if !utf8.ValidRune(item.item.rune) {
			return packedRunChain{}, false
		}
		if index == 0 {
			continue
		}
		switch {
		case item.id.WallTime == previous.WallTime && previous.Logical != math.MaxUint64 && item.id.Logical == previous.Logical+1:
		case item.id.WallTime > previous.WallTime && item.id.Logical == 0:
		default:
			return packedRunChain{}, false
		}
		previous = item.id
	}
	textBytes := 0
	transitionCount := 0
	for index, item := range block {
		textBytes += utf8.RuneLen(item.item.rune)
		if index > 0 && item.id.WallTime > block[index-1].id.WallTime {
			transitionCount++
		}
	}
	chain := packedRunChain{
		transitions: make([]byte, transitionLength),
		wallDeltas:  make([]uint64, transitionCount),
		text:        make([]byte, 0, textBytes),
	}
	previous = block[0].id
	wallIndex := 0
	for index, item := range block {
		chain.text = utf8.AppendRune(chain.text, item.item.rune)
		if index > 0 && item.id.WallTime > previous.WallTime {
			setPackedTransition(chain.transitions, index-1)
			chain.wallDeltas[wallIndex] = item.id.WallTime - previous.WallTime
			wallIndex++
		}
		previous = item.id
	}
	return chain, true
}

func packedTransitionLength(count int) (int, bool) {
	if count < 2 || count > maxPackedInt {
		return 0, false
	}
	return (count-2)/8 + 1, true
}

const maxPackedInt = int(^uint(0) >> 1)

func setPackedTransition(transitions []byte, index int) {
	transitions[index/8] |= byte(1 << (index % 8))
}

func packedTransition(transitions []byte, index int) bool {
	return transitions[index/8]&byte(1<<(index%8)) != 0
}

// UnmarshalRGAPackedDelta decodes one bounded compact RGA v3 delta.
func UnmarshalRGAPackedDelta(data []byte) (Delta, error) {
	return UnmarshalRGAPackedDeltaWithLimits(data, frame.DefaultLimits())
}

// UnmarshalRGAPackedDeltaWithLimits decodes one compact RGA v3 delta with
// caller-selected resource limits.
func UnmarshalRGAPackedDeltaWithLimits(data []byte, limits frame.DecoderLimits) (Delta, error) {
	nodes, tombstones, err := unmarshalRGAPacked(data, crdt.TypeIDRGAPackedDelta, limits, false)
	if err != nil {
		return Delta{}, err
	}
	return Delta{nodes: nodes, tombstones: tombstones}, nil
}

// UnmarshalPackedBinary installs one complete compact RGA v3 state frame.
func (r *RGA) UnmarshalPackedBinary(data []byte) error {
	return r.UnmarshalPackedBinaryWithLimits(data, frame.DefaultLimits())
}

// UnmarshalPackedBinaryWithLimits installs one complete compact RGA v3 state
// frame after complete validation and pre-allocation bounds checks.
func (r *RGA) UnmarshalPackedBinaryWithLimits(data []byte, limits frame.DecoderLimits) error {
	if r == nil || r.clock == nil {
		return ErrNilText
	}
	nodes, tombstones, err := unmarshalRGAPacked(data, crdt.TypeIDRGAPackedState, limits, true)
	if err != nil {
		return err
	}
	return r.installState(nodes, tombstones)
}

func unmarshalRGAPacked(data []byte, expectedType uint64, limits frame.DecoderLimits, complete bool) (map[Position]node, map[Position]struct{}, error) {
	decoded, err := frame.UnmarshalFrame(data, limits)
	if err != nil {
		return nil, nil, err
	}
	if decoded.TypeID != expectedType || decoded.CodecID != "" {
		return nil, nil, frame.ErrInvalidFrame
	}
	position := 0
	blockCount, next, ok := frame.ReadUvarint(decoded.Payload, position)
	if !ok || limits.MaxElements < 0 || blockCount > uint64(limits.MaxElements) {
		return nil, nil, frame.ErrInvalidFrame
	}
	position = next
	nodes := make(map[Position]node)
	for blockIndex := uint64(0); blockIndex < blockCount; blockIndex++ {
		kind, next, ok := frame.ReadUvarint(decoded.Payload, position)
		if !ok || kind > packedRunBlockChain {
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
		count, replicaID, parent, next, ok := readPackedRunChainHeader(decoded.Payload, position, len(nodes), limits)
		if !ok {
			return nil, nil, frame.ErrInvalidFrame
		}
		countInt, ok := runCountAsInt(count)
		if !ok {
			return nil, nil, frame.ErrInvalidFrame
		}
		position = next
		if kind == runBlockChain {
			if !decodeRegularPackedRunChain(decoded.Payload, &position, countInt, replicaID, parent, nodes) {
				return nil, nil, frame.ErrInvalidFrame
			}
			continue
		}
		if !decodeDensePackedRunChain(decoded.Payload, &position, countInt, replicaID, parent, nodes, limits) {
			return nil, nil, frame.ErrInvalidFrame
		}
	}
	tombCount, next, ok := frame.ReadUvarint(decoded.Payload, position)
	remainingTags := limits.MaxTags - len(nodes)
	if !ok || remainingTags < 0 || tombCount > uint64(remainingTags) {
		return nil, nil, frame.ErrInvalidFrame
	}
	position = next
	tombstoneCapacity, ok := runCountAsInt(tombCount)
	if !ok {
		return nil, nil, frame.ErrInvalidFrame
	}
	tombstones := make(map[Position]struct{}, tombstoneCapacity)
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
	canonical, err := marshalPackedRunPayload(makePackedRunBlocks(nodes), sortedTombstoneIDs(tombstones), limits)
	if err != nil || !bytes.Equal(canonical, decoded.Payload) {
		return nil, nil, frame.ErrInvalidFrame
	}
	return nodes, tombstones, nil
}

func readPackedRunChainHeader(payload []byte, position, existingNodes int, limits frame.DecoderLimits) (uint64, string, Position, int, bool) {
	count, next, ok := frame.ReadUvarint(payload, position)
	remainingNodes := limits.MaxElements - existingNodes
	if !ok || count < 2 || remainingNodes < 0 || count > uint64(remainingNodes) {
		return 0, "", Position{}, position, false
	}
	position = next
	replicaBytes, next, ok := frame.ReadBytes(payload, position, limits.MaxStringBytes)
	if !ok || !(crdt.Tag{ReplicaID: string(replicaBytes)}).Valid() {
		return 0, "", Position{}, position, false
	}
	position = next
	parentFlag, next, ok := frame.ReadUvarint(payload, position)
	if !ok || parentFlag > 1 {
		return 0, "", Position{}, position, false
	}
	position = next
	parent := Position{}
	if parentFlag == 1 {
		parent, next, ok = frame.ReadTag(payload, position, limits.MaxStringBytes)
		if !ok {
			return 0, "", Position{}, position, false
		}
		position = next
	}
	return count, string(replicaBytes), parent, position, true
}

func decodeRegularPackedRunChain(payload []byte, position *int, count int, replicaID string, parent Position, nodes map[Position]node) bool {
	for index := 0; index < count; index++ {
		wallTime, next, ok := frame.ReadUvarint(payload, *position)
		if !ok {
			return false
		}
		logical, next, ok := frame.ReadUvarint(payload, next)
		if !ok {
			return false
		}
		runeValue, next, ok := frame.ReadUvarint(payload, next)
		scalar, validScalar := decodeRunScalar(runeValue)
		if !ok || !validScalar {
			return false
		}
		id := Position{ReplicaID: replicaID, WallTime: wallTime, Logical: logical}
		if _, exists := nodes[id]; exists {
			return false
		}
		nodes[id] = node{parent: parent, rune: scalar}
		parent, *position = id, next
	}
	return true
}

func decodeDensePackedRunChain(payload []byte, position *int, count int, replicaID string, parent Position, nodes map[Position]node, limits frame.DecoderLimits) bool {
	wallTime, next, ok := frame.ReadUvarint(payload, *position)
	if !ok {
		return false
	}
	_, next, ok = frame.ReadUvarint(payload, next)
	if !ok {
		return false
	}
	transitionLength, ok := packedTransitionLength(count)
	if !ok {
		return false
	}
	transitions, next, ok := frame.ReadBytes(payload, next, transitionLength)
	if !ok || len(transitions) != transitionLength || unusedPackedTransitionBitsSet(transitions, count) {
		return false
	}
	gaps := make([]uint64, 0, packedTransitionCount(transitions, count))
	for index := 1; index < count; index++ {
		if !packedTransition(transitions, index-1) {
			continue
		}
		gap, afterGap, ok := frame.ReadUvarint(payload, next)
		if !ok || gap == 0 || wallTime > math.MaxUint64-gap {
			return false
		}
		wallTime += gap
		gaps = append(gaps, gap)
		next = afterGap
	}
	text, next, ok := frame.ReadBytes(payload, next, limits.MaxPayload)
	if !ok || !utf8.Valid(text) || utf8.RuneCount(text) != count {
		return false
	}
	startWall, startLogical, _, ok := densePackedStart(payload, *position, transitionLength)
	if !ok {
		return false
	}
	wallTime = startWall
	logical := startLogical
	gapIndex := 0
	itemIndex := 0
	for len(text) > 0 {
		scalar, width := utf8.DecodeRune(text)
		text = text[width:]
		if itemIndex > 0 {
			if packedTransition(transitions, itemIndex-1) {
				if gapIndex == len(gaps) || wallTime > math.MaxUint64-gaps[gapIndex] {
					return false
				}
				wallTime += gaps[gapIndex]
				gapIndex++
				logical = 0
			} else {
				if logical == math.MaxUint64 {
					return false
				}
				logical++
			}
		}
		id := Position{ReplicaID: replicaID, WallTime: wallTime, Logical: logical}
		if _, exists := nodes[id]; exists {
			return false
		}
		nodes[id] = node{parent: parent, rune: scalar}
		parent = id
		itemIndex++
	}
	if gapIndex != len(gaps) {
		return false
	}
	*position = next
	return true
}

func packedTransitionCount(transitions []byte, count int) int {
	total := 0
	for index := 0; index < count-1; index++ {
		if packedTransition(transitions, index) {
			total++
		}
	}
	return total
}

func unusedPackedTransitionBitsSet(transitions []byte, count int) bool {
	used := (count - 1) % 8
	if used == 0 {
		return false
	}
	return transitions[len(transitions)-1]&^byte((1<<used)-1) != 0
}

func densePackedStart(payload []byte, position, transitionLength int) (uint64, uint64, int, bool) {
	wallTime, next, ok := frame.ReadUvarint(payload, position)
	if !ok {
		return 0, 0, position, false
	}
	logical, next, ok := frame.ReadUvarint(payload, next)
	if !ok {
		return 0, 0, position, false
	}
	_, next, ok = frame.ReadBytes(payload, next, transitionLength)
	if !ok {
		return 0, 0, position, false
	}
	return wallTime, logical, next, true
}

// SnapshotPackedCurrentState returns a complete HLC-backed compact RGA v3
// snapshot. Persist it with its frontier and clock state before reusing the
// replica ID.
func (r *RGA) SnapshotPackedCurrentState() (snapshot.Snapshot, error) {
	return r.SnapshotPackedCurrentStateWithLimits(frame.DefaultLimits())
}

// SnapshotPackedCurrentStateWithLimits returns a bounded compact RGA v3
// snapshot. Externally supplied snapshots still pass full decode validation in
// NewFromSnapshotWithOptions before installation.
func (r *RGA) SnapshotPackedCurrentStateWithLimits(limits frame.DecoderLimits) (snapshot.Snapshot, error) {
	if r == nil || r.clock == nil {
		return snapshot.Snapshot{}, ErrNilText
	}
	captured, clockState, err := r.captureRunState(true)
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	state, err := marshalRGAPackedState(captured, limits)
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	return snapshot.NewWithClockState(state, frontierForRunState(captured), clockState)
}
