package text

// sequenceIndex keeps RGA's deterministic depth-first order incrementally.
// Each RGA node owns an enter/exit marker. Inserting a child after a sibling's
// exit marker avoids rewriting ancestor ranges, while the implicit treap
// supplies O(log n) visible-offset lookup. Marker next/prev links make full
// text scans linear without materializing a projection.
type sequenceIndex struct {
	root    *sequenceMarker
	entries map[Position]*sequenceMarker
	exits   map[Position]*sequenceMarker
}

type sequenceMarker struct {
	position Position
	exit     bool
	visible  bool
	priority uint64

	left, right, parent *sequenceMarker
	prev, next          *sequenceMarker
	markers, visibleN   int
}

func newSequenceIndex() *sequenceIndex {
	entry := newSequenceMarker(Position{}, false, false)
	exit := newSequenceMarker(Position{}, true, false)
	entry.next, exit.prev = exit, entry
	index := &sequenceIndex{
		entries: map[Position]*sequenceMarker{Position{}: entry},
		exits:   make(map[Position]*sequenceMarker),
	}
	index.root = mergeMarkers(entry, exit)
	return index
}

func newSequenceMarker(position Position, exit, visible bool) *sequenceMarker {
	marker := &sequenceMarker{position: position, exit: exit, visible: visible, priority: markerPriority(position, exit)}
	refreshMarker(marker)
	return marker
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

func (index *sequenceIndex) insertAfter(anchor *sequenceMarker, position Position, visible bool) {
	entry := newSequenceMarker(position, false, visible)
	exit := newSequenceMarker(position, true, false)
	next := anchor.next
	anchor.next, entry.prev = entry, anchor
	entry.next, exit.prev = exit, entry
	exit.next = next
	if next != nil {
		next.prev = exit
	}
	left, right := splitMarkers(index.root, markerRank(anchor)+1)
	index.root = mergeMarkers(mergeMarkers(mergeMarkers(left, entry), exit), right)
	index.entries[position] = entry
	index.exits[position] = exit
}

func (index *sequenceIndex) setVisible(position Position, visible bool) {
	entry := index.entries[position]
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
	entry, exit := index.entries[position], index.exits[position]
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
	delete(index.entries, position)
	delete(index.exits, position)
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
				return current.position, true
			}
			offset--
		}
		current = current.right
	}
	return Position{}, false
}

func (index *sequenceIndex) visiblePositions() []Position {
	positions := make([]Position, 0, visibleCount(index.root))
	for current := index.entries[Position{}].next; current != nil; current = current.next {
		if current.visible {
			positions = append(positions, current.position)
		}
	}
	return positions
}
