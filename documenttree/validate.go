package documenttree

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/darkinno-tech/crdt"
)

func validateState(state documentState, options Options, allowPending bool) error {
	if !options.valid() {
		return ErrResourceLimit
	}
	if len(state.roots) > options.MaxRoots || len(state.objects) > options.MaxObjects ||
		countMapEntries(state) > options.MaxMapEntries || countArrayNodes(state) > options.MaxArrayNodes ||
		countTombstones(state) > options.MaxArrayTombstones {
		return ErrResourceLimit
	}
	claims := make(map[crdt.Tag]string, len(state.roots)+countMapEntries(state)+countArrayNodes(state))
	claim := func(tag crdt.Tag, use string) error {
		if !tag.Valid() {
			return ErrInvalidDelta
		}
		if previous, exists := claims[tag]; exists && previous != use {
			return ErrTagConflict
		}
		claims[tag] = use
		return nil
	}
	for name, root := range state.roots {
		if name != root.name || !validNameWithLimit(name, options.MaxKeyBytes) || !root.id.Valid() || !root.kind.valid() {
			return ErrInvalidDelta
		}
		if err := claim(root.id, "root:"+name); err != nil {
			return err
		}
	}
	for id, object := range state.objects {
		if id != object.id || !id.Valid() || !object.kind.valid() || !validOwner(object.owner, options) {
			return ErrInvalidDelta
		}
	}
	for target, entries := range state.maps {
		if !target.Valid() {
			return ErrInvalidDelta
		}
		for key, entry := range entries {
			if !validNameWithLimit(key, options.MaxKeyBytes) || !entry.tag.Valid() {
				return ErrInvalidDelta
			}
			if err := claim(entry.tag, "map:"+tagKey(target)+":"+key); err != nil {
				return err
			}
			if entry.present && !validStoredValue(entry.value, options) {
				return ErrInvalidDelta
			}
		}
	}
	for target, nodes := range state.arrays {
		if !target.Valid() {
			return ErrInvalidDelta
		}
		for id, node := range nodes {
			if id != node.id || !id.Valid() || node.parent == id || !validStoredValue(node.value, options) {
				return ErrInvalidDelta
			}
			if err := claim(id, "array:"+tagKey(target)+":"+tagKey(id)); err != nil {
				return err
			}
		}
		if !acyclicArray(nodes) {
			return ErrInvalidDelta
		}
	}
	for target, tombstones := range state.tombstones {
		if !target.Valid() {
			return ErrInvalidDelta
		}
		for id := range tombstones {
			if !id.Valid() {
				return ErrInvalidDelta
			}
		}
	}
	if err := validateResolvedTypes(state); err != nil {
		return err
	}
	if err := validateObjectGraph(state, options, allowPending); err != nil {
		return err
	}
	pending, bytes := pendingState(state)
	if pending > options.MaxPendingOperations || bytes > options.MaxPendingBytes {
		return ErrResourceLimit
	}
	if !allowPending && pending != 0 {
		return ErrIncompleteState
	}
	return nil
}

func validOwner(owner objectOwner, options Options) bool {
	switch owner.kind {
	case ownerRoot:
		return validNameWithLimit(owner.rootName, options.MaxKeyBytes) && !owner.parent.Valid() && !owner.position.Valid() && owner.key == ""
	case ownerMap:
		return owner.parent.Valid() && validNameWithLimit(owner.key, options.MaxKeyBytes) && !owner.position.Valid() && owner.rootName == ""
	case ownerArray:
		return owner.parent.Valid() && owner.position.Valid() && owner.key == "" && owner.rootName == ""
	default:
		return false
	}
}

func validStoredValue(value Value, options Options) bool {
	switch value.Kind {
	case ValueBytes:
		return len(value.Bytes) <= options.MaxValueBytes && !value.Object.ID.Valid() && value.Object.Kind == 0
	case ValueObject:
		return value.Object.ID.Valid() && value.Object.Kind.valid() && len(value.Bytes) == 0
	default:
		return false
	}
}

func validNameWithLimit(value string, max int) bool {
	if max <= 0 || !utf8.ValidString(value) || len(value) == 0 || len(value) > max || strings.TrimSpace(value) != value {
		return false
	}
	for _, part := range value {
		if unicode.IsControl(part) {
			return false
		}
	}
	return true
}

func validateResolvedTypes(state documentState) error {
	for name, root := range state.roots {
		if object, exists := state.objects[root.id]; exists && (object.kind != root.kind || object.owner.kind != ownerRoot || object.owner.rootName != name) {
			return ErrInvalidState
		}
	}
	for target, entries := range state.maps {
		if object, exists := state.objects[target]; exists && object.kind != KindMap {
			return ErrTypeMismatch
		}
		for key, entry := range entries {
			if !entry.present || entry.value.Kind != ValueObject {
				continue
			}
			if entry.tag != entry.value.Object.ID {
				return ErrInvalidState
			}
			if object, exists := state.objects[entry.value.Object.ID]; exists &&
				(object.kind != entry.value.Object.Kind || object.owner.kind != ownerMap || object.owner.parent != target || object.owner.key != key) {
				return ErrInvalidState
			}
		}
	}
	for target, nodes := range state.arrays {
		if object, exists := state.objects[target]; exists && object.kind != KindArray {
			return ErrTypeMismatch
		}
		for _, node := range nodes {
			if node.value.Kind != ValueObject {
				continue
			}
			if node.id != node.value.Object.ID {
				return ErrInvalidState
			}
			if object, exists := state.objects[node.value.Object.ID]; exists &&
				(object.kind != node.value.Object.Kind || object.owner.kind != ownerArray || object.owner.parent != target || object.owner.position != node.id) {
				return ErrInvalidState
			}
		}
	}
	return nil
}

func validateObjectGraph(state documentState, options Options, allowPending bool) error {
	for _, object := range state.objects {
		if object.owner.kind == ownerRoot {
			continue // Roots replaced by a newer root remain retained but unreachable.
		}
		parent, exists := state.objects[object.owner.parent]
		if !exists {
			if allowPending {
				continue
			}
			return ErrIncompleteState
		}
		if (object.owner.kind == ownerMap && parent.kind != KindMap) || (object.owner.kind == ownerArray && parent.kind != KindArray) {
			return ErrTypeMismatch
		}
	}
	for id := range state.objects {
		depth := 0
		seen := make(map[ObjectID]struct{}, 4)
		for current := id; current.Valid(); {
			if _, exists := seen[current]; exists {
				return ErrInvalidState
			}
			seen[current] = struct{}{}
			object := state.objects[current]
			if object.owner.kind == ownerRoot {
				break
			}
			parent, exists := state.objects[object.owner.parent]
			if !exists {
				if allowPending {
					break
				}
				return ErrIncompleteState
			}
			depth++
			if depth > options.MaxDepth {
				return ErrResourceLimit
			}
			current = parent.id
		}
	}
	return nil
}

func pendingState(state documentState) (int, int) {
	count, bytes := 0, 0
	add := func(size int) { count++; bytes += size }
	for _, root := range state.roots {
		if _, exists := state.objects[root.id]; !exists {
			add(rootRecordSize(root))
		}
	}
	for _, object := range state.objects {
		if object.owner.kind != ownerRoot {
			if _, exists := state.objects[object.owner.parent]; !exists {
				add(objectDeclSize(object))
			}
		}
	}
	for target, entries := range state.maps {
		for key, entry := range entries {
			missing := false
			if _, exists := state.objects[target]; !exists {
				missing = true
			}
			if entry.present && entry.value.Kind == ValueObject {
				if _, exists := state.objects[entry.value.Object.ID]; !exists {
					missing = true
				}
			}
			if missing {
				add(mapEntrySize(target, key, entry))
			}
		}
	}
	for target, nodes := range state.arrays {
		for _, node := range nodes {
			missing := false
			if _, exists := state.objects[target]; !exists {
				missing = true
			}
			if node.parent.Valid() {
				if _, exists := nodes[node.parent]; !exists {
					missing = true
				}
			}
			if node.value.Kind == ValueObject {
				if _, exists := state.objects[node.value.Object.ID]; !exists {
					missing = true
				}
			}
			if missing {
				add(arrayNodeSize(target, node))
			}
		}
	}
	return count, bytes
}

func acyclicArray(nodes map[ObjectID]arrayNode) bool {
	// A single three-colour walk proves all local parent chains acyclic in
	// O(nodes) retained work. The prior per-node seen map made a wide array's
	// validation quadratic and amplified every unrelated map-field update.
	colours := make(map[ObjectID]uint8, len(nodes))
	var visit func(ObjectID) bool
	visit = func(id ObjectID) bool {
		switch colours[id] {
		case 1:
			return false
		case 2:
			return true
		}
		node, exists := nodes[id]
		if !exists {
			return true // A delta may name an external RGA parent.
		}
		colours[id] = 1
		if node.parent.Valid() && !visit(node.parent) {
			return false
		}
		colours[id] = 2
		return true
	}
	for id := range nodes {
		if !visit(id) {
			return false
		}
	}
	return true
}

func joinState(current, incoming documentState, options Options) (documentState, bool, crdt.Tag, error) {
	if err := validateState(current, options, true); err != nil {
		return documentState{}, false, crdt.Tag{}, err
	}
	if err := validateState(incoming, options, true); err != nil {
		return documentState{}, false, crdt.Tag{}, err
	}
	// Candidate state is copy-on-write by section and target object. Validation
	// below can still inspect a complete prospective state before installation,
	// while one card-field update no longer duplicates an unrelated workboard.
	candidate := current
	rootsCopied := false
	objectsCopied := false
	mapsCopied := false
	arraysCopied := false
	tombstonesCopied := false
	copiedMapTargets := make(map[ObjectID]struct{}, len(incoming.maps))
	copiedArrayTargets := make(map[ObjectID]struct{}, len(incoming.arrays))
	copiedTombstoneTargets := make(map[ObjectID]struct{}, len(incoming.tombstones))
	ensureRoots := func() {
		if rootsCopied {
			return
		}
		candidate.roots = cloneRootRecords(candidate.roots)
		rootsCopied = true
	}
	ensureObjects := func() {
		if objectsCopied {
			return
		}
		candidate.objects = cloneObjectDecls(candidate.objects)
		objectsCopied = true
	}
	ensureMapEntries := func(target ObjectID) map[string]mapEntry {
		if !mapsCopied {
			candidate.maps = cloneMapOuter(candidate.maps)
			mapsCopied = true
		}
		if _, copied := copiedMapTargets[target]; !copied {
			candidate.maps[target] = cloneMapEntryRecords(candidate.maps[target])
			copiedMapTargets[target] = struct{}{}
		}
		return candidate.maps[target]
	}
	ensureArrayNodes := func(target ObjectID) map[ObjectID]arrayNode {
		if !arraysCopied {
			candidate.arrays = cloneArrayOuter(candidate.arrays)
			arraysCopied = true
		}
		if _, copied := copiedArrayTargets[target]; !copied {
			candidate.arrays[target] = cloneArrayNodeRecords(candidate.arrays[target])
			copiedArrayTargets[target] = struct{}{}
		}
		return candidate.arrays[target]
	}
	ensureTombstones := func(target ObjectID) map[ObjectID]struct{} {
		if !tombstonesCopied {
			candidate.tombstones = cloneTombstoneOuter(candidate.tombstones)
			tombstonesCopied = true
		}
		if _, copied := copiedTombstoneTargets[target]; !copied {
			candidate.tombstones[target] = cloneTombstoneRecords(candidate.tombstones[target])
			copiedTombstoneTargets[target] = struct{}{}
		}
		return candidate.tombstones[target]
	}
	changed := false
	for name, root := range incoming.roots {
		if previous, exists := candidate.roots[name]; exists {
			if previous.id == root.id && previous.kind != root.kind {
				return documentState{}, false, crdt.Tag{}, ErrTagConflict
			}
			if previous.id.Compare(root.id) >= 0 {
				continue
			}
		}
		ensureRoots()
		candidate.roots[name] = root
		changed = true
	}
	for id, object := range incoming.objects {
		if previous, exists := candidate.objects[id]; exists {
			if previous != object {
				return documentState{}, false, crdt.Tag{}, ErrTagConflict
			}
			continue
		}
		ensureObjects()
		candidate.objects[id] = object
		changed = true
	}
	for target, entries := range incoming.maps {
		for key, entry := range entries {
			currentEntries := candidate.maps[target]
			if previous, exists := currentEntries[key]; exists {
				if previous.tag == entry.tag {
					if !sameMapEntry(previous, entry) {
						return documentState{}, false, crdt.Tag{}, ErrTagConflict
					}
					continue
				}
				if previous.tag.Compare(entry.tag) > 0 {
					continue
				}
			}
			ensureMapEntries(target)[key] = mapEntry{tag: entry.tag, present: entry.present, value: cloneValue(entry.value)}
			changed = true
		}
	}
	for target, nodes := range incoming.arrays {
		for id, node := range nodes {
			currentNodes := candidate.arrays[target]
			if previous, exists := currentNodes[id]; exists {
				if !sameArrayNode(previous, node) {
					return documentState{}, false, crdt.Tag{}, ErrTagConflict
				}
				continue
			}
			ensureArrayNodes(target)[id] = arrayNode{id: node.id, parent: node.parent, value: cloneValue(node.value)}
			changed = true
		}
	}
	for target, tombstones := range incoming.tombstones {
		for id := range tombstones {
			if _, exists := candidate.tombstones[target][id]; !exists {
				ensureTombstones(target)[id] = struct{}{}
				changed = true
			}
		}
	}
	if !changed {
		return current, false, crdt.Tag{}, nil
	}
	if err := validateState(candidate, options, true); err != nil {
		return documentState{}, false, crdt.Tag{}, err
	}
	greatest := greatestStateTag(incoming)
	return candidate, true, greatest, nil
}

func cloneRootRecords(values map[string]rootRecord) map[string]rootRecord {
	result := make(map[string]rootRecord, len(values)+1)
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneObjectDecls(values map[ObjectID]objectDecl) map[ObjectID]objectDecl {
	result := make(map[ObjectID]objectDecl, len(values)+1)
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneMapOuter(values map[ObjectID]map[string]mapEntry) map[ObjectID]map[string]mapEntry {
	result := make(map[ObjectID]map[string]mapEntry, len(values)+1)
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneMapEntryRecords(values map[string]mapEntry) map[string]mapEntry {
	result := make(map[string]mapEntry, len(values)+1)
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneArrayOuter(values map[ObjectID]map[ObjectID]arrayNode) map[ObjectID]map[ObjectID]arrayNode {
	result := make(map[ObjectID]map[ObjectID]arrayNode, len(values)+1)
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneArrayNodeRecords(values map[ObjectID]arrayNode) map[ObjectID]arrayNode {
	result := make(map[ObjectID]arrayNode, len(values)+1)
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneTombstoneOuter(values map[ObjectID]map[ObjectID]struct{}) map[ObjectID]map[ObjectID]struct{} {
	result := make(map[ObjectID]map[ObjectID]struct{}, len(values)+1)
	for key, value := range values {
		result[key] = value
	}
	return result
}

func cloneTombstoneRecords(values map[ObjectID]struct{}) map[ObjectID]struct{} {
	result := make(map[ObjectID]struct{}, len(values)+1)
	for key := range values {
		result[key] = struct{}{}
	}
	return result
}

func sameMapEntry(left, right mapEntry) bool {
	return left.tag == right.tag && left.present == right.present && sameValue(left.value, right.value)
}

func sameArrayNode(left, right arrayNode) bool {
	return left.id == right.id && left.parent == right.parent && sameValue(left.value, right.value)
}

func sameValue(left, right Value) bool {
	if left.Kind != right.Kind || left.Object != right.Object || len(left.Bytes) != len(right.Bytes) {
		return false
	}
	for index := range left.Bytes {
		if left.Bytes[index] != right.Bytes[index] {
			return false
		}
	}
	return true
}

func greatestStateTag(state documentState) crdt.Tag {
	var greatest crdt.Tag
	found := false
	consider := func(tag crdt.Tag) {
		if !found || greatest.Compare(tag) < 0 {
			greatest, found = tag, true
		}
	}
	for _, root := range state.roots {
		consider(root.id)
	}
	for _, entries := range state.maps {
		for _, entry := range entries {
			consider(entry.tag)
		}
	}
	for _, nodes := range state.arrays {
		for id := range nodes {
			consider(id)
		}
	}
	return greatest
}

func documentFrontier(state documentState) map[string]crdt.Tag {
	frontier := make(map[string]crdt.Tag)
	consider := func(tag crdt.Tag) {
		if current, exists := frontier[tag.ReplicaID]; !exists || current.Compare(tag) < 0 {
			frontier[tag.ReplicaID] = tag
		}
	}
	for _, root := range state.roots {
		consider(root.id)
	}
	for _, entries := range state.maps {
		for _, entry := range entries {
			consider(entry.tag)
		}
	}
	for _, nodes := range state.arrays {
		for id := range nodes {
			consider(id)
		}
	}
	return frontier
}

func greatestFrontierTag(frontier map[string]crdt.Tag) (crdt.Tag, bool) {
	var greatest crdt.Tag
	found := false
	for _, tag := range frontier {
		if !found || greatest.Compare(tag) < 0 {
			greatest, found = tag, true
		}
	}
	return greatest, found
}

func tagKey(tag crdt.Tag) string {
	return tag.ReplicaID + "\x00" + strconv.FormatUint(tag.WallTime, 10) + "\x00" + strconv.FormatUint(tag.Logical, 10)
}

func sortedObjectIDs(values map[ObjectID]objectDecl) []ObjectID {
	ids := make([]ObjectID, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Compare(ids[j]) < 0 })
	return ids
}
