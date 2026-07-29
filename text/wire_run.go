package text

import (
	"bytes"
	"sort"
	"unicode/utf8"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
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

// rgaRunState is an immutable, point-in-time copy of the fields needed to
// encode a complete run-v2 state. Slices cost substantially less than cloning
// the node and tombstone maps, while preserving the short read-lock window
// required by concurrent callers.
type rgaRunState struct {
	nodes      []runNode
	tombstones []Position
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

type indexedRunChild struct {
	index int
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
	state, _, err := r.captureRunState(false)
	if err != nil {
		return nil, err
	}
	return marshalRGARunState(state, limits)
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
	tombIDs := sortedTombstoneIDs(tombstones)
	return marshalRunBlocks(typeID, blocks, tombIDs, limits)
}

func marshalRGARunState(state rgaRunState, limits frame.DecoderLimits) ([]byte, error) {
	if len(state.nodes) > limits.MaxElements || len(state.nodes) > limits.MaxTags || len(state.tombstones) > limits.MaxTags-len(state.nodes) {
		return nil, frame.ErrFrameLimit
	}
	sort.Slice(state.nodes, func(i, j int) bool { return state.nodes[i].id.Compare(state.nodes[j].id) < 0 })
	sort.Slice(state.tombstones, func(i, j int) bool { return state.tombstones[i].Compare(state.tombstones[j]) < 0 })
	linear, err := validateCompleteRunState(state)
	if err != nil {
		return nil, err
	}
	blocks := makeRunBlocksFromSortedItems(state.nodes)
	if linear {
		blocks = [][]runNode{state.nodes}
	}
	return marshalRunBlocks(crdt.TypeIDRGARunState, blocks, state.tombstones, limits)
}

func marshalRunBlocks(typeID uint64, blocks [][]runNode, tombIDs []Position, limits frame.DecoderLimits) ([]byte, error) {
	payloadSize, err := runPayloadSizeWithTombstoneIDs(blocks, tombIDs, limits)
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

// captureRunState snapshots complete state into compact slices while holding
// r.mu only for the copy. Encoding, sorting, and frame allocation happen
// after the read lock is released, so writers retain the prior contention
// behavior without the map-clone allocation cost.
func (r *RGA) captureRunState(withClock bool) (rgaRunState, clock.State, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.pending) > 0 {
		return rgaRunState{}, clock.State{}, ErrIncompleteState
	}
	state := rgaRunState{
		nodes:      make([]runNode, 0, len(r.nodes)),
		tombstones: make([]Position, 0, len(r.tombstones)),
	}
	for id, item := range r.nodes {
		state.nodes = append(state.nodes, runNode{id: id, item: item})
	}
	for id := range r.tombstones {
		state.tombstones = append(state.tombstones, id)
	}
	if withClock {
		if r.clock == nil {
			return rgaRunState{}, clock.State{}, ErrNilText
		}
		return state, r.clock.Snapshot(), nil
	}
	return state, clock.State{}, nil
}

// validateCompleteRunState keeps the same rejection behavior as map-backed
// state encoding after capture has released r.mu. A sorted linear run proves
// parent completeness in one pass; the uncommon branching case uses binary
// search instead of rebuilding a node map.
func validateCompleteRunState(state rgaRunState) (bool, error) {
	linear := len(state.nodes) > 0
	var replicaID string
	for index, item := range state.nodes {
		if !item.id.Valid() || !utf8.ValidRune(item.item.rune) || (item.item.parent != (Position{}) && !item.item.parent.Valid()) || item.id == item.item.parent || (index > 0 && state.nodes[index-1].id == item.id) {
			return false, ErrInvalidDelta
		}
		if index == 0 {
			replicaID = item.id.ReplicaID
			if item.item.parent.Valid() {
				linear = false
			}
			continue
		}
		if item.id.ReplicaID != replicaID || item.item.parent != state.nodes[index-1].id {
			linear = false
		}
	}
	for _, id := range state.tombstones {
		if !id.Valid() {
			return false, ErrInvalidDelta
		}
	}
	if linear {
		return true, nil
	}
	for _, item := range state.nodes {
		if item.item.parent.Valid() && !containsSortedRunNode(state.nodes, item.item.parent) {
			return false, ErrIncompleteState
		}
	}
	if !acyclicSortedRunNodes(state.nodes) {
		return false, ErrInvalidDelta
	}
	return false, nil
}

func containsSortedRunNode(items []runNode, id Position) bool {
	index := sort.Search(len(items), func(index int) bool { return items[index].id.Compare(id) >= 0 })
	return index < len(items) && items[index].id == id
}

func acyclicSortedRunNodes(items []runNode) bool {
	const (
		unseen uint8 = iota
		visiting
		complete
	)
	state := make([]uint8, len(items))
	path := make([]int, 0)
	for start := range items {
		if state[start] != unseen {
			continue
		}
		path = path[:0]
		current := start
		for {
			switch state[current] {
			case visiting:
				return false
			case complete:
				for _, index := range path {
					state[index] = complete
				}
				goto next
			}
			state[current] = visiting
			path = append(path, current)
			parent := items[current].item.parent
			if !parent.Valid() {
				for _, index := range path {
					state[index] = complete
				}
				goto next
			}
			nextIndex := sort.Search(len(items), func(index int) bool { return items[index].id.Compare(parent) >= 0 })
			if nextIndex >= len(items) || items[nextIndex].id != parent {
				return false
			}
			current = nextIndex
		}
	next:
	}
	return true
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
	return runPayloadSizeWithTombstoneIDs(blocks, sortedTombstoneIDs(tombstones), limits)
}

func runPayloadSizeWithTombstoneIDs(blocks [][]runNode, tombIDs []Position, limits frame.DecoderLimits) (int, error) {
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
	additional := frame.UvarintSize(uint64(len(tombIDs)))
	if additional > limits.MaxPayload-size {
		return 0, frame.ErrFrameLimit
	}
	size += additional
	for _, id := range tombIDs {
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

// makeRunBlocksFromSortedItems preserves the canonical block construction
// used for map-backed deltas without rebuilding a map that was already copied
// into a state snapshot. The common local-insert shape simply reuses items as
// its one chain.
func makeRunBlocksFromSortedItems(items []runNode) [][]runNode {
	if len(items) == 0 {
		return nil
	}
	replicaID := items[0].id.ReplicaID
	linear := true
	for index, item := range items {
		if item.id.ReplicaID != replicaID || (index > 0 && item.item.parent != items[index-1].id) {
			linear = false
			break
		}
	}
	if linear {
		return [][]runNode{items}
	}
	children := make(map[runChildKey]indexedRunChild, len(items))
	for index, item := range items {
		key := runChildKey{parent: item.item.parent, replicaID: item.id.ReplicaID}
		child := children[key]
		child.count++
		if child.count == 1 {
			child.index = index
		}
		children[key] = child
	}
	used := make([]bool, len(items))
	blocks := make([][]runNode, 0)
	ordered := make([]runNode, 0, len(items))
	for index, first := range items {
		if used[index] {
			continue
		}
		start := len(ordered)
		currentIndex := index
		for !used[currentIndex] {
			current := items[currentIndex]
			ordered = append(ordered, current)
			used[currentIndex] = true
			next, ok := children[runChildKey{parent: current.id, replicaID: first.id.ReplicaID}]
			if !ok || next.count != 1 || used[next.index] {
				break
			}
			currentIndex = next.index
		}
		blocks = append(blocks, ordered[start:])
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

// SnapshotRunCurrentState returns an HLC-backed run-v2 snapshot.
func (r *RGA) SnapshotRunCurrentState() (snapshot.Snapshot, error) {
	return r.SnapshotRunCurrentStateWithLimits(frame.DefaultLimits())
}

// SnapshotRunCurrentStateWithLimits returns an HLC-backed, validated run-v2
// snapshot while enforcing caller-selected output limits.
func (r *RGA) SnapshotRunCurrentStateWithLimits(limits frame.DecoderLimits) (snapshot.Snapshot, error) {
	if r == nil || r.clock == nil {
		return snapshot.Snapshot{}, ErrNilText
	}
	captured, clockState, err := r.captureRunState(true)
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	state, err := marshalRGARunState(captured, limits)
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	// state was just created from a complete, captured RGA and checked by
	// marshalRGARunState. Do not use this constructor for externally supplied
	// state: recovery still decodes and validates type-specific bytes before
	// mutation in NewFromSnapshotWithOptions.
	return snapshot.NewWithClockState(state, frontierForRunState(captured), clockState)
}

func frontierForRunState(state rgaRunState) map[string]crdt.Tag {
	frontier := make(map[string]crdt.Tag)
	for _, item := range state.nodes {
		recordFrontier(frontier, item.id)
		if item.item.parent.Valid() {
			recordFrontier(frontier, item.item.parent)
		}
	}
	for _, id := range state.tombstones {
		recordFrontier(frontier, id)
	}
	return frontier
}

func validateRGARunState(data []byte) error {
	_, _, err := unmarshalRGARun(data, crdt.TypeIDRGARunState, frame.DefaultLimits(), true)
	return err
}
