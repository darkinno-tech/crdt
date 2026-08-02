package text

// sequenceIndex keeps RGA's deterministic depth-first order incrementally.
// Each RGA node owns an enter/exit marker. Inserting a child after a sibling's
// exit marker avoids rewriting ancestor ranges, while the implicit treap
// supplies O(log n) visible-offset lookup. Marker next/prev links make full
// text scans linear without materializing a projection.
type sequenceIndex struct {
	root  *sequenceMarker
	pairs map[Position]*sequencePair
}

// sequencePair keeps the paired enter/exit markers for one RGA position in
// one allocation and stores the Position only once. Both markers are still
// required to preserve a subtree boundary when a later sibling arrives, but
// they never need independent ownership.
type sequencePair struct {
	position    Position
	singleChild *sequencePair
	entry       sequenceMarker
	exit        sequenceMarker
}

type sequenceMarker struct {
	pair     *sequencePair
	visible  bool
	priority uint64

	left, right, parent *sequenceMarker
	prev, next          *sequenceMarker
	markers, visibleN   int
}

func newSequenceIndex() *sequenceIndex {
	pair := newSequencePair(Position{}, false)
	entry, exit := &pair.entry, &pair.exit
	entry.next, exit.prev = exit, entry
	index := &sequenceIndex{
		pairs: map[Position]*sequencePair{{}: pair},
	}
	index.root = mergeMarkers(entry, exit)
	return index
}

func newSequencePair(position Position, visible bool) *sequencePair {
	pair := new(sequencePair)
	initializeSequencePair(pair, position, visible)
	return pair
}

// initializeSequencePair resets pair for one retained RGA position. Batch
// integration uses it for elements of one contiguous allocation, while the
// generic path continues to allocate one pair at a time through
// newSequencePair. A pointer to an element of that allocation remains stable
// after the batch slice itself goes out of scope because sequenceIndex retains
// the pointer in both its map and marker tree.
func initializeSequencePair(pair *sequencePair, position Position, visible bool) {
	*pair = sequencePair{position: position}
	pair.entry = sequenceMarker{pair: pair, visible: visible, priority: markerPriority(position, false)}
	pair.exit = sequenceMarker{pair: pair, priority: markerPriority(position, true)}
	refreshMarker(&pair.entry)
	refreshMarker(&pair.exit)
}

func markerPriority(position Position, exit bool) uint64 {
	// A stable, well-mixed priority keeps the treap independent of arrival
	// order. It affects only internal shape; RGA ordering is always tag based.
	value := uint64(0x9e3779b97f4a7c15)
	for index := 0; index < len(position.ReplicaID); index++ {
		value ^= uint64(position.ReplicaID[index])
		value *= 0x100000001b3
	}
	value ^= position.WallTime + 0x9e3779b97f4a7c15 + (value << 6) + (value >> 2)
	value ^= position.Logical + 0x9e3779b97f4a7c15 + (value << 6) + (value >> 2)
	if exit {
		value ^= 0xd6e8feb86659fd93
	}
	value ^= value >> 30
	value *= 0xbf58476d1ce4e5b9
	value ^= value >> 27
	value *= 0x94d049bb133111eb
	return value ^ (value >> 31)
}

func markerCount(marker *sequenceMarker) int {
	if marker == nil {
		return 0
	}
	return marker.markers
}

func visibleCount(marker *sequenceMarker) int {
	if marker == nil {
		return 0
	}
	return marker.visibleN
}

func refreshMarker(marker *sequenceMarker) {
	if marker == nil {
		return
	}
	marker.markers = 1 + markerCount(marker.left) + markerCount(marker.right)
	marker.visibleN = visibleCount(marker.left) + visibleCount(marker.right)
	if marker.visible {
		marker.visibleN++
	}
	if marker.left != nil {
		marker.left.parent = marker
	}
	if marker.right != nil {
		marker.right.parent = marker
	}
}

func mergeMarkers(left, right *sequenceMarker) *sequenceMarker {
	if left == nil {
		if right != nil {
			right.parent = nil
		}
		return right
	}
	if right == nil {
		left.parent = nil
		return left
	}
	if left.priority > right.priority {
		left.right = mergeMarkers(left.right, right)
		refreshMarker(left)
		left.parent = nil
		return left
	}
	right.left = mergeMarkers(left, right.left)
	refreshMarker(right)
	right.parent = nil
	return right
}

func splitMarkers(root *sequenceMarker, count int) (*sequenceMarker, *sequenceMarker) {
	if root == nil {
		return nil, nil
	}
	leftCount := markerCount(root.left)
	if count <= leftCount {
		left, right := splitMarkers(root.left, count)
		root.left = right
		refreshMarker(root)
		if left != nil {
			left.parent = nil
		}
		root.parent = nil
		return left, root
	}
	left, right := splitMarkers(root.right, count-leftCount-1)
	root.right = left
	refreshMarker(root)
	if right != nil {
		right.parent = nil
	}
	root.parent = nil
	return root, right
}

func markerRank(marker *sequenceMarker) int {
	rank := markerCount(marker.left)
	for marker.parent != nil {
		if marker == marker.parent.right {
			rank += 1 + markerCount(marker.parent.left)
		}
		marker = marker.parent
	}
	return rank
}

// visibleRank reports the number of visible entries strictly before marker.
// Like markerRank, it follows parent links through the implicit treap rather
// than scanning the document. Tombstoned entries still have a stable rank,
// which is what lets local cursor anchors remain meaningful until compaction.
func visibleRank(marker *sequenceMarker) int {
	rank := visibleCount(marker.left)
	for marker.parent != nil {
		if marker == marker.parent.right {
			rank += visibleCount(marker.parent.left)
			if marker.parent.visible {
				rank++
			}
		}
		marker = marker.parent
	}
	return rank
}

func (index *sequenceIndex) entry(position Position) *sequenceMarker {
	pair := index.pair(position)
	if pair == nil {
		return nil
	}
	return &pair.entry
}

func (index *sequenceIndex) exit(position Position) *sequenceMarker {
	pair := index.pair(position)
	if pair == nil {
		return nil
	}
	return &pair.exit
}

func (index *sequenceIndex) pair(position Position) *sequencePair {
	return index.pairs[position]
}

func (index *sequenceIndex) has(position Position) bool {
	_, ok := index.pairs[position]
	return ok
}

// reserveInitialPairs reserves the position index only while it contains the
// root sentinel. A large first insert otherwise grows this map repeatedly;
// rebuilding a non-empty index would add copy work to ordinary edits, so that
// path deliberately remains unchanged.
func (index *sequenceIndex) reserveInitialPairs(additional int) {
	if additional <= 0 || len(index.pairs) != 1 {
		return
	}
	pairs := make(map[Position]*sequencePair, additional+1)
	for position, pair := range index.pairs {
		pairs[position] = pair
	}
	index.pairs = pairs
}

func (index *sequenceIndex) insertPairAfter(anchor *sequenceMarker, pair *sequencePair) {
	index.insertLinearMarkersAfter(anchor, []*sequenceMarker{&pair.entry, &pair.exit}, 1)
}

// insertLinearMarkersAfter inserts one parent-before-child RGA chain in a
// single index splice. Its marker order is the chain's deterministic depth
// first order: every entry in forward order followed by every exit in reverse
// order. The caller has already established that every pair after the first
// is the sole child of its predecessor.
func (index *sequenceIndex) insertLinearMarkersAfter(anchor *sequenceMarker, markers []*sequenceMarker, pairCount int) {
	if anchor == nil || pairCount == 0 || len(markers) != pairCount*2 {
		return
	}
	for markerIndex := 1; markerIndex < len(markers); markerIndex++ {
		previous, current := markers[markerIndex-1], markers[markerIndex]
		previous.next, current.prev = current, previous
	}

	first, last := markers[0], markers[len(markers)-1]
	next := anchor.next
	anchor.next, first.prev = first, anchor
	last.next = next
	if next != nil {
		next.prev = last
	}
	left, right := splitMarkers(index.root, markerRank(anchor)+1)
	for pairIndex := 0; pairIndex < pairCount; pairIndex++ {
		pair := markers[pairIndex].pair
		index.pairs[pair.position] = pair
	}
	index.root = mergeMarkers(mergeMarkers(left, markerTree(markers)), right)
}

// insertLinearPairsAfter is the resolved-run counterpart to
// insertLinearMarkersAfter. Pairs already occupy one retained backing array,
// so walking that array directly avoids building a second, transient slice of
// marker pointers solely to restore their deterministic enter/exit order.
func (index *sequenceIndex) insertLinearPairsAfter(anchor *sequenceMarker, pairs []sequencePair) {
	if anchor == nil || len(pairs) == 0 {
		return
	}
	next := anchor.next
	previous := anchor
	for pairIndex := range pairs {
		marker := &pairs[pairIndex].entry
		previous.next, marker.prev = marker, previous
		previous = marker
	}
	for pairIndex := len(pairs) - 1; pairIndex >= 0; pairIndex-- {
		marker := &pairs[pairIndex].exit
		previous.next, marker.prev = marker, previous
		previous = marker
	}
	previous.next = next
	if next != nil {
		next.prev = previous
	}

	left, right := splitMarkers(index.root, markerRank(anchor)+1)
	for pairIndex := range pairs {
		pair := &pairs[pairIndex]
		index.pairs[pair.position] = pair
	}
	index.root = mergeMarkers(mergeMarkers(left, markerTreeForLinearPairs(pairs)), right)
}

// markerTreeForLinearPairs constructs the Cartesian marker tree in the same
// enter-forward/exit-reverse order as markerTree. The parent links form the
// current monotonic right spine, so they replace a temporary []*sequenceMarker
// stack. A marker is refreshed only when it leaves that spine, at which point
// both of its subtrees are complete. This keeps the operation linear without
// a per-marker transient pointer allocation.
func markerTreeForLinearPairs(pairs []sequencePair) *sequenceMarker {
	var top *sequenceMarker
	for pairIndex := range pairs {
		appendLinearMarkerToTree(&top, &pairs[pairIndex].entry)
	}
	for pairIndex := len(pairs) - 1; pairIndex >= 0; pairIndex-- {
		appendLinearMarkerToTree(&top, &pairs[pairIndex].exit)
	}

	var root *sequenceMarker
	for top != nil {
		parent := top.parent
		refreshMarker(top)
		root = top
		top = parent
	}
	return root
}

// appendLinearMarkerToTree extends a Cartesian tree whose right spine is
// stored in top. Popping a marker finalizes it: its future in-order successors
// cannot become children after a higher-priority marker replaces it.
func appendLinearMarkerToTree(top **sequenceMarker, marker *sequenceMarker) {
	marker.left, marker.right, marker.parent = nil, nil, nil
	var left *sequenceMarker
	for *top != nil && (*top).priority < marker.priority {
		left = *top
		*top = left.parent
		refreshMarker(left)
	}
	marker.left = left
	if left != nil {
		left.parent = marker
	}
	if *top != nil {
		(*top).right = marker
		marker.parent = *top
	}
	*top = marker
}

// markerTree builds the max-priority Cartesian tree for an already ordered
// marker sequence in linear time. mergeMarkers would build the same tree, but
// repeatedly merging a long resolved run would retrace the existing index for
// every pair. Markers are newly allocated, so clearing their links cannot
// disturb an installed tree.
func markerTree(markers []*sequenceMarker) *sequenceMarker {
	if len(markers) == 0 {
		return nil
	}
	stack := markers[:0]
	for _, marker := range markers {
		marker.left, marker.right, marker.parent = nil, nil, nil
		var left *sequenceMarker
		for len(stack) > 0 && stack[len(stack)-1].priority < marker.priority {
			left = stack[len(stack)-1]
			stack = stack[:len(stack)-1]
		}
		marker.left = left
		if left != nil {
			left.parent = marker
		}
		if len(stack) > 0 {
			parent := stack[len(stack)-1]
			parent.right = marker
			marker.parent = parent
		}
		stack = append(stack, marker)
	}
	root := stack[0]
	ordered := markers[:0]
	pending := []*sequenceMarker{root}
	for len(pending) > 0 {
		marker := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		ordered = append(ordered, marker)
		if marker.left != nil {
			pending = append(pending, marker.left)
		}
		if marker.right != nil {
			pending = append(pending, marker.right)
		}
	}
	for markerIndex := len(ordered) - 1; markerIndex >= 0; markerIndex-- {
		refreshMarker(ordered[markerIndex])
	}
	return root
}

func (index *sequenceIndex) setVisible(position Position, visible bool) {
	entry := index.entry(position)
	if entry == nil || entry.visible == visible {
		return
	}
	entry.visible = visible
	for current := entry; current != nil; current = current.parent {
		refreshMarker(current)
	}
}

// removeLeaf removes an entry/exit pair after its caller has established that
// no child markers can lie between them.
func (index *sequenceIndex) removeLeaf(position Position) bool {
	entry, exit := index.entry(position), index.exit(position)
	if entry == nil || exit == nil || entry.next != exit {
		return false
	}
	before, after := entry.prev, exit.next
	if before == nil {
		return false
	}
	before.next = after
	if after != nil {
		after.prev = before
	}
	left, rest := splitMarkers(index.root, markerRank(entry))
	_, right := splitMarkers(rest, 2)
	index.root = mergeMarkers(left, right)
	delete(index.pairs, position)
	return true
}

func (index *sequenceIndex) visibleAt(offset int) (Position, bool) {
	if offset < 0 || offset >= visibleCount(index.root) {
		return Position{}, false
	}
	current := index.root
	for current != nil {
		leftVisible := visibleCount(current.left)
		if offset < leftVisible {
			current = current.left
			continue
		}
		offset -= leftVisible
		if current.visible {
			if offset == 0 {
				return current.pair.position, true
			}
			offset--
		}
		current = current.right
	}
	return Position{}, false
}

func (index *sequenceIndex) visiblePositions() []Position {
	positions := make([]Position, 0, visibleCount(index.root))
	for current := index.entry(Position{}).next; current != nil; current = current.next {
		if current.visible {
			positions = append(positions, current.pair.position)
		}
	}
	return positions
}
