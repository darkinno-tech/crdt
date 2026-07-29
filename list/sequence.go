package list

// sequenceIndex maintains the deterministic depth-first RGA order
// incrementally. Its implicit treap provides O(log n) visible-offset lookup,
// while paired enter/exit markers preserve a subtree boundary for later
// concurrent siblings without rebuilding the full list projection.
type sequenceIndex struct {
	root  *sequenceMarker
	pairs map[Position]*sequencePair
}

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

type childIndex struct {
	branches map[Position][]*sequencePair
}

func newSequenceIndex() *sequenceIndex {
	pair := newSequencePair(Position{}, false)
	entry, exit := &pair.entry, &pair.exit
	entry.next, exit.prev = exit, entry
	return &sequenceIndex{root: mergeMarkers(entry, exit), pairs: map[Position]*sequencePair{Position{}: pair}}
}

func newChildIndex() childIndex { return childIndex{branches: make(map[Position][]*sequencePair)} }

func newSequencePair(position Position, visible bool) *sequencePair {
	pair := &sequencePair{position: position}
	pair.entry = sequenceMarker{pair: pair, visible: visible, priority: markerPriority(position, false)}
	pair.exit = sequenceMarker{pair: pair, priority: markerPriority(position, true)}
	refreshMarker(&pair.entry)
	refreshMarker(&pair.exit)
	return pair
}

func markerPriority(position Position, exit bool) uint64 {
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

func (index *sequenceIndex) pair(position Position) *sequencePair { return index.pairs[position] }

func (index *sequenceIndex) insertPairAfter(anchor *sequenceMarker, pair *sequencePair) {
	entry, exit := &pair.entry, &pair.exit
	next := anchor.next
	anchor.next, entry.prev = entry, anchor
	entry.next, exit.prev = exit, entry
	exit.next = next
	if next != nil {
		next.prev = exit
	}
	left, right := splitMarkers(index.root, markerRank(anchor)+1)
	index.root = mergeMarkers(mergeMarkers(mergeMarkers(left, entry), exit), right)
	index.pairs[pair.position] = pair
}

func (index *sequenceIndex) setVisible(position Position, visible bool) {
	pair := index.pair(position)
	if pair == nil || pair.entry.visible == visible {
		return
	}
	pair.entry.visible = visible
	for current := &pair.entry; current != nil; current = current.parent {
		refreshMarker(current)
	}
}

func (index *sequenceIndex) removeLeaf(position Position) bool {
	pair := index.pair(position)
	if pair == nil || pair.entry.next != &pair.exit {
		return false
	}
	before, after := pair.entry.prev, pair.exit.next
	if before == nil {
		return false
	}
	before.next = after
	if after != nil {
		after.prev = before
	}
	left, rest := splitMarkers(index.root, markerRank(&pair.entry))
	_, right := splitMarkers(rest, 2)
	index.root = mergeMarkers(left, right)
	delete(index.pairs, position)
	return true
}

func (index *sequenceIndex) visibleAt(offset int) (Position, bool) {
	if offset < 0 || offset >= visibleCount(index.root) {
		return Position{}, false
	}
	for current := index.root; current != nil; {
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
	root := index.pair(Position{})
	for current := root.entry.next; current != nil; current = current.next {
		if current.visible {
			positions = append(positions, current.pair.position)
		}
	}
	return positions
}

func (index *childIndex) count(parent *sequencePair) int {
	if parent == nil {
		return 0
	}
	if siblings, exists := index.branches[parent.position]; exists {
		return len(siblings)
	}
	if parent.singleChild != nil {
		return 1
	}
	return 0
}

func (index *childIndex) insert(parent, child *sequencePair) (*sequencePair, bool) {
	if siblings, exists := index.branches[parent.position]; exists {
		insertAt := sortSearchDescending(siblings, child.position)
		if insertAt < len(siblings) && siblings[insertAt].position == child.position {
			if insertAt == 0 {
				return nil, false
			}
			return siblings[insertAt-1], true
		}
		var previous *sequencePair
		if insertAt > 0 {
			previous = siblings[insertAt-1]
		}
		siblings = append(siblings, nil)
		copy(siblings[insertAt+1:], siblings[insertAt:])
		siblings[insertAt] = child
		index.branches[parent.position] = siblings
		return previous, insertAt > 0
	}
	current := parent.singleChild
	if current == nil {
		parent.singleChild = child
		return nil, false
	}
	if current.position == child.position {
		return nil, false
	}
	parent.singleChild = nil
	if current.position.Compare(child.position) < 0 {
		index.branches[parent.position] = []*sequencePair{child, current}
		return nil, false
	}
	index.branches[parent.position] = []*sequencePair{current, child}
	return current, true
}

func (index *childIndex) remove(parent, child *sequencePair) bool {
	if siblings, exists := index.branches[parent.position]; exists {
		removeAt := sortSearchDescendingOrEqual(siblings, child.position)
		if removeAt == len(siblings) || siblings[removeAt].position != child.position {
			return false
		}
		copy(siblings[removeAt:], siblings[removeAt+1:])
		siblings = siblings[:len(siblings)-1]
		switch len(siblings) {
		case 0:
			delete(index.branches, parent.position)
		case 1:
			delete(index.branches, parent.position)
			parent.singleChild = siblings[0]
		default:
			index.branches[parent.position] = siblings
		}
		return true
	}
	if parent.singleChild != nil && parent.singleChild.position == child.position {
		parent.singleChild = nil
		return true
	}
	return false
}

func sortSearchDescending(items []*sequencePair, target Position) int {
	left, right := 0, len(items)
	for left < right {
		middle := left + (right-left)/2
		if items[middle].position.Compare(target) < 0 {
			right = middle
		} else {
			left = middle + 1
		}
	}
	return left
}

func sortSearchDescendingOrEqual(items []*sequencePair, target Position) int {
	left, right := 0, len(items)
	for left < right {
		middle := left + (right-left)/2
		if items[middle].position.Compare(target) <= 0 {
			right = middle
		} else {
			left = middle + 1
		}
	}
	return left
}
