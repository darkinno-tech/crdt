package documenttree

import (
	"sort"

	"github.com/im10furry/crdt"
	frame "github.com/im10furry/crdt/encoding"
)

// MarshalBinary returns a canonical bounded document-tree delta frame.
func (d Delta) MarshalBinary() ([]byte, error) {
	return d.MarshalBinaryWithLimits(frame.DefaultLimits())
}

// MarshalBinaryWithLimits enforces the exact output frame budget selected by
// the local group before returning a delta for an outbox.
func (d Delta) MarshalBinaryWithLimits(limits frame.DecoderLimits) ([]byte, error) {
	return marshalState(crdt.TypeIDDocumentTreeDelta, d.state, DefaultOptions(), limits, true)
}

// UnmarshalDelta decodes one bounded canonical delta.
func UnmarshalDelta(data []byte) (Delta, error) {
	return UnmarshalDeltaWithLimits(data, frame.DefaultLimits())
}

// UnmarshalDeltaWithLimits decodes a delta with the default retained-value
// policy. ApplyDelta always rechecks it against the receiver's local limits.
func UnmarshalDeltaWithLimits(data []byte, limits frame.DecoderLimits) (Delta, error) {
	return UnmarshalDeltaWithOptions(data, DefaultOptions(), limits)
}

// UnmarshalDeltaWithOptions validates one delta before it can enter a
// receiver-owned pending queue.
func UnmarshalDeltaWithOptions(data []byte, options Options, limits frame.DecoderLimits) (Delta, error) {
	state, err := unmarshalState(data, crdt.TypeIDDocumentTreeDelta, options, limits, true)
	if err != nil {
		return Delta{}, err
	}
	return Delta{state: state}, nil
}

func validateDocumentState(data []byte) error {
	_, err := unmarshalState(data, crdt.TypeIDDocumentTreeState, DefaultOptions(), frame.DefaultLimits(), false)
	return err
}

// marshalState encodes state and delta payloads in the same canonical section
// order. A delta is allowed to name an external parent; a state never is.
func marshalState(typeID uint64, state documentState, options Options, limits frame.DecoderLimits, allowPending bool) ([]byte, error) {
	if typeID != crdt.TypeIDDocumentTreeState && typeID != crdt.TypeIDDocumentTreeDelta {
		return nil, frame.ErrInvalidFrame
	}
	if err := validateState(state, options, allowPending); err != nil {
		return nil, err
	}
	roots := sortedRootRecords(state.roots)
	objects := sortedObjectDecls(state.objects)
	entries := sortedMapRecords(state.maps)
	nodes := sortedArrayRecords(state.arrays)
	tombstones := sortedTombstoneRecords(state.tombstones)
	if total := len(roots) + len(objects) + len(entries) + len(nodes) + len(tombstones); total > limits.MaxElements {
		return nil, frame.ErrFrameLimit
	}
	if stateTagUses(state) > limits.MaxTags {
		return nil, frame.ErrFrameLimit
	}
	payloadSize := frame.UvarintSize(uint64(len(roots)))
	for _, root := range roots {
		if err := addBytesSize(&payloadSize, len(root.name), limits); err != nil {
			return nil, err
		}
		if err := addRawSize(&payloadSize, frame.UvarintSize(uint64(root.kind))+frame.TagSize(root.id), limits); err != nil {
			return nil, err
		}
	}
	if err := addRawSize(&payloadSize, frame.UvarintSize(uint64(len(objects))), limits); err != nil {
		return nil, err
	}
	for _, object := range objects {
		if err := addObjectDeclSize(&payloadSize, object, limits); err != nil {
			return nil, err
		}
	}
	if err := addRawSize(&payloadSize, frame.UvarintSize(uint64(len(entries))), limits); err != nil {
		return nil, err
	}
	for _, record := range entries {
		if err := addMapRecordSize(&payloadSize, record, limits); err != nil {
			return nil, err
		}
	}
	if err := addRawSize(&payloadSize, frame.UvarintSize(uint64(len(nodes))), limits); err != nil {
		return nil, err
	}
	for _, record := range nodes {
		if err := addArrayRecordSize(&payloadSize, record, limits); err != nil {
			return nil, err
		}
	}
	if err := addRawSize(&payloadSize, frame.UvarintSize(uint64(len(tombstones))), limits); err != nil {
		return nil, err
	}
	for _, record := range tombstones {
		if len(record.target.ReplicaID) > limits.MaxStringBytes || len(record.id.ReplicaID) > limits.MaxStringBytes {
			return nil, frame.ErrFrameLimit
		}
		if err := addRawSize(&payloadSize, frame.TagSize(record.target)+frame.TagSize(record.id), limits); err != nil {
			return nil, err
		}
	}
	return frame.MarshalFrameWithPayloadAndLimits(typeID, "", payloadSize, limits, func(payload []byte) error {
		output := frame.AppendUvarint(payload[:0], uint64(len(roots)))
		for _, root := range roots {
			output = appendBytes(output, []byte(root.name))
			output = frame.AppendUvarint(output, uint64(root.kind))
			output = frame.AppendTag(output, root.id)
		}
		output = frame.AppendUvarint(output, uint64(len(objects)))
		for _, object := range objects {
			output = appendObjectDecl(output, object)
		}
		output = frame.AppendUvarint(output, uint64(len(entries)))
		for _, record := range entries {
			output = frame.AppendTag(output, record.target)
			output = appendBytes(output, []byte(record.key))
			output = frame.AppendTag(output, record.entry.tag)
			if record.entry.present {
				output = frame.AppendUvarint(output, 1)
				output = appendValue(output, record.entry.value)
			} else {
				output = frame.AppendUvarint(output, 0)
			}
		}
		output = frame.AppendUvarint(output, uint64(len(nodes)))
		for _, record := range nodes {
			output = frame.AppendTag(output, record.target)
			output = frame.AppendTag(output, record.node.id)
			if record.node.parent.Valid() {
				output = frame.AppendUvarint(output, 1)
				output = frame.AppendTag(output, record.node.parent)
			} else {
				output = frame.AppendUvarint(output, 0)
			}
			output = appendValue(output, record.node.value)
		}
		output = frame.AppendUvarint(output, uint64(len(tombstones)))
		for _, record := range tombstones {
			output = frame.AppendTag(output, record.target)
			output = frame.AppendTag(output, record.id)
		}
		if len(output) != payloadSize {
			return frame.ErrInvalidFrame
		}
		return nil
	})
}

func unmarshalState(data []byte, typeID uint64, options Options, limits frame.DecoderLimits, allowPending bool) (documentState, error) {
	if !options.valid() {
		return documentState{}, ErrResourceLimit
	}
	decoded, err := frame.UnmarshalFrameView(data, limits)
	if err != nil {
		return documentState{}, err
	}
	if decoded.TypeID != typeID || decoded.CodecID != "" {
		return documentState{}, frame.ErrInvalidFrame
	}
	return unmarshalPayload(decoded.Payload, options, limits, allowPending)
}

// boundedCount converts only after proving that an attacker-controlled count
// fits both the host int and every supplied local/frame budget.
func boundedCount(count uint64, bounds ...int) (int, bool) {
	if count > uint64(^uint(0)>>1) {
		return 0, false
	}
	value := int(count)
	for _, bound := range bounds {
		if bound < 0 || value > bound {
			return 0, false
		}
	}
	return value, true
}

func decodedKind(value uint64) (Kind, bool) {
	switch value {
	case uint64(KindMap):
		return KindMap, true
	case uint64(KindArray):
		return KindArray, true
	default:
		return 0, false
	}
}

func unmarshalPayload(payload []byte, options Options, limits frame.DecoderLimits, allowPending bool) (documentState, error) {
	state := newDocumentState()
	position := 0
	records := 0
	count, next, ok := frame.ReadUvarint(payload, position)
	rootRecords, countOK := boundedCount(count, limits.MaxElements, options.MaxRoots)
	if !ok || !countOK {
		return documentState{}, frame.ErrInvalidFrame
	}
	records = rootRecords
	position = next
	previousName := ""
	for index := uint64(0); index < count; index++ {
		name, next, ok := readString(payload, position, min(limits.MaxStringBytes, options.MaxKeyBytes))
		if !ok || (index > 0 && previousName >= name) {
			return documentState{}, frame.ErrInvalidFrame
		}
		position = next
		kindValue, next, ok := frame.ReadUvarint(payload, position)
		kind, validKind := decodedKind(kindValue)
		if !ok || !validKind {
			return documentState{}, frame.ErrInvalidFrame
		}
		position = next
		id, next, ok := frame.ReadTag(payload, position, limits.MaxStringBytes)
		if !ok {
			return documentState{}, frame.ErrInvalidFrame
		}
		position = next
		state.roots[name] = rootRecord{name: name, id: id, kind: kind}
		previousName = name
	}
	count, next, ok = frame.ReadUvarint(payload, position)
	objectRecords, countOK := boundedCount(count, limits.MaxElements-records, options.MaxObjects)
	if !ok || !countOK {
		return documentState{}, frame.ErrInvalidFrame
	}
	records += objectRecords
	position = next
	var previousObject ObjectID
	for index := uint64(0); index < count; index++ {
		object, next, ok := readObjectDecl(payload, position, options, limits)
		if !ok || (index > 0 && previousObject.Compare(object.id) >= 0) {
			return documentState{}, frame.ErrInvalidFrame
		}
		position = next
		state.objects[object.id] = object
		previousObject = object.id
	}
	count, next, ok = frame.ReadUvarint(payload, position)
	mapRecords, countOK := boundedCount(count, limits.MaxElements-records, options.MaxMapEntries)
	if !ok || !countOK {
		return documentState{}, frame.ErrInvalidFrame
	}
	records += mapRecords
	position = next
	var previousMap mapRecord
	for index := uint64(0); index < count; index++ {
		record, next, ok := readMapRecord(payload, position, options, limits)
		if !ok || (index > 0 && compareMapRecord(previousMap, record) >= 0) {
			return documentState{}, frame.ErrInvalidFrame
		}
		position = next
		if state.maps[record.target] == nil {
			state.maps[record.target] = make(map[string]mapEntry)
		}
		state.maps[record.target][record.key] = record.entry
		previousMap = record
	}
	count, next, ok = frame.ReadUvarint(payload, position)
	arrayRecords, countOK := boundedCount(count, limits.MaxElements-records, options.MaxArrayNodes)
	if !ok || !countOK {
		return documentState{}, frame.ErrInvalidFrame
	}
	records += arrayRecords
	position = next
	var previousNode arrayRecord
	for index := uint64(0); index < count; index++ {
		record, next, ok := readArrayRecord(payload, position, options, limits)
		if !ok || (index > 0 && compareArrayRecord(previousNode, record) >= 0) {
			return documentState{}, frame.ErrInvalidFrame
		}
		position = next
		if state.arrays[record.target] == nil {
			state.arrays[record.target] = make(map[ObjectID]arrayNode)
		}
		state.arrays[record.target][record.node.id] = record.node
		previousNode = record
	}
	count, next, ok = frame.ReadUvarint(payload, position)
	_, countOK = boundedCount(count, limits.MaxElements-records, options.MaxArrayTombstones)
	if !ok || !countOK {
		return documentState{}, frame.ErrInvalidFrame
	}
	position = next
	var previousTomb tombstoneRecord
	for index := uint64(0); index < count; index++ {
		target, next, ok := frame.ReadTag(payload, position, limits.MaxStringBytes)
		if !ok {
			return documentState{}, frame.ErrInvalidFrame
		}
		position = next
		id, next, ok := frame.ReadTag(payload, position, limits.MaxStringBytes)
		if !ok {
			return documentState{}, frame.ErrInvalidFrame
		}
		position = next
		record := tombstoneRecord{target: target, id: id}
		if (index > 0 && compareTombstoneRecord(previousTomb, record) >= 0) || target == id {
			return documentState{}, frame.ErrInvalidFrame
		}
		if state.tombstones[target] == nil {
			state.tombstones[target] = make(map[ObjectID]struct{})
		}
		state.tombstones[target][id] = struct{}{}
		previousTomb = record
	}
	if position != len(payload) {
		return documentState{}, frame.ErrInvalidFrame
	}
	if err := validateState(state, options, allowPending); err != nil {
		return documentState{}, err
	}
	if stateTagUses(state) > limits.MaxTags {
		return documentState{}, frame.ErrInvalidFrame
	}
	// Decode + encode comparison closes alternate but semantically equivalent
	// payload representations (including section ordering) before mutation.
	reencoded, err := marshalState(crdt.TypeIDDocumentTreeDelta, state, options, limits, allowPending)
	if err != nil {
		return documentState{}, err
	}
	frameValue, err := frame.UnmarshalFrameView(reencoded, limits)
	if err != nil || string(frameValue.Payload) != string(payload) {
		return documentState{}, frame.ErrInvalidFrame
	}
	return state, nil
}

type mapRecord struct {
	target ObjectID
	key    string
	entry  mapEntry
}

type arrayRecord struct {
	target ObjectID
	node   arrayNode
}

type tombstoneRecord struct {
	target ObjectID
	id     ObjectID
}

func sortedRootRecords(roots map[string]rootRecord) []rootRecord {
	result := make([]rootRecord, 0, len(roots))
	for _, root := range roots {
		result = append(result, root)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].name < result[j].name })
	return result
}

func sortedObjectDecls(values map[ObjectID]objectDecl) []objectDecl {
	result := make([]objectDecl, 0, len(values))
	for _, object := range values {
		result = append(result, object)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].id.Compare(result[j].id) < 0 })
	return result
}

func sortedMapRecords(values map[ObjectID]map[string]mapEntry) []mapRecord {
	result := make([]mapRecord, 0, countMapEntries(documentState{maps: values}))
	for target, entries := range values {
		for key, entry := range entries {
			result = append(result, mapRecord{target: target, key: key, entry: entry})
		}
	}
	sort.Slice(result, func(i, j int) bool { return compareMapRecord(result[i], result[j]) < 0 })
	return result
}

func sortedArrayRecords(values map[ObjectID]map[ObjectID]arrayNode) []arrayRecord {
	result := make([]arrayRecord, 0, countArrayNodes(documentState{arrays: values}))
	for target, nodes := range values {
		for _, node := range nodes {
			result = append(result, arrayRecord{target: target, node: node})
		}
	}
	sort.Slice(result, func(i, j int) bool { return compareArrayRecord(result[i], result[j]) < 0 })
	return result
}

func sortedTombstoneRecords(values map[ObjectID]map[ObjectID]struct{}) []tombstoneRecord {
	result := make([]tombstoneRecord, 0, countTombstones(documentState{tombstones: values}))
	for target, tombstones := range values {
		for id := range tombstones {
			result = append(result, tombstoneRecord{target: target, id: id})
		}
	}
	sort.Slice(result, func(i, j int) bool { return compareTombstoneRecord(result[i], result[j]) < 0 })
	return result
}

func compareMapRecord(left, right mapRecord) int {
	if compared := left.target.Compare(right.target); compared != 0 {
		return compared
	}
	if left.key < right.key {
		return -1
	}
	if left.key > right.key {
		return 1
	}
	return 0
}

func compareArrayRecord(left, right arrayRecord) int {
	if compared := left.target.Compare(right.target); compared != 0 {
		return compared
	}
	return left.node.id.Compare(right.node.id)
}

func compareTombstoneRecord(left, right tombstoneRecord) int {
	if compared := left.target.Compare(right.target); compared != 0 {
		return compared
	}
	return left.id.Compare(right.id)
}

func appendBytes(output, value []byte) []byte {
	output = frame.AppendUvarint(output, uint64(len(value)))
	return append(output, value...)
}

func appendObjectDecl(output []byte, object objectDecl) []byte {
	output = frame.AppendTag(output, object.id)
	output = frame.AppendUvarint(output, uint64(object.kind))
	output = frame.AppendUvarint(output, uint64(object.owner.kind))
	switch object.owner.kind {
	case ownerRoot:
		return appendBytes(output, []byte(object.owner.rootName))
	case ownerMap:
		output = frame.AppendTag(output, object.owner.parent)
		return appendBytes(output, []byte(object.owner.key))
	case ownerArray:
		return frame.AppendTag(output, object.owner.parent)
	default:
		return output
	}
}

func appendValue(output []byte, value Value) []byte {
	output = frame.AppendUvarint(output, uint64(value.Kind))
	switch value.Kind {
	case ValueBytes:
		return appendBytes(output, value.Bytes)
	case ValueObject:
		output = frame.AppendUvarint(output, uint64(value.Object.Kind))
		return frame.AppendTag(output, value.Object.ID)
	default:
		return output
	}
}

func readObjectDecl(payload []byte, position int, options Options, limits frame.DecoderLimits) (objectDecl, int, bool) {
	id, next, ok := frame.ReadTag(payload, position, limits.MaxStringBytes)
	if !ok {
		return objectDecl{}, position, false
	}
	kind, next, ok := frame.ReadUvarint(payload, next)
	objectKind, validKind := decodedKind(kind)
	if !ok || !validKind {
		return objectDecl{}, position, false
	}
	owner, next, ok := frame.ReadUvarint(payload, next)
	if !ok {
		return objectDecl{}, position, false
	}
	object := objectDecl{id: id, kind: objectKind}
	switch owner {
	case uint64(ownerRoot):
		object.owner.kind = ownerRoot
		name, position, ok := readString(payload, next, min(limits.MaxStringBytes, options.MaxKeyBytes))
		if !ok {
			return objectDecl{}, position, false
		}
		object.owner.rootName = name
		return object, position, true
	case uint64(ownerMap):
		object.owner.kind = ownerMap
		parent, position, ok := frame.ReadTag(payload, next, limits.MaxStringBytes)
		if !ok {
			return objectDecl{}, position, false
		}
		key, position, ok := readString(payload, position, min(limits.MaxStringBytes, options.MaxKeyBytes))
		if !ok {
			return objectDecl{}, position, false
		}
		object.owner.parent, object.owner.key = parent, key
		return object, position, true
	case uint64(ownerArray):
		object.owner.kind = ownerArray
		parent, position, ok := frame.ReadTag(payload, next, limits.MaxStringBytes)
		if !ok {
			return objectDecl{}, position, false
		}
		object.owner.parent, object.owner.position = parent, id
		return object, position, true
	default:
		return objectDecl{}, position, false
	}
}

func readMapRecord(payload []byte, position int, options Options, limits frame.DecoderLimits) (mapRecord, int, bool) {
	target, next, ok := frame.ReadTag(payload, position, limits.MaxStringBytes)
	if !ok {
		return mapRecord{}, position, false
	}
	key, next, ok := readString(payload, next, min(limits.MaxStringBytes, options.MaxKeyBytes))
	if !ok {
		return mapRecord{}, position, false
	}
	tag, next, ok := frame.ReadTag(payload, next, limits.MaxStringBytes)
	if !ok {
		return mapRecord{}, position, false
	}
	present, next, ok := frame.ReadUvarint(payload, next)
	if !ok || present > 1 {
		return mapRecord{}, position, false
	}
	record := mapRecord{target: target, key: key, entry: mapEntry{tag: tag, present: present == 1}}
	if present == 0 {
		return record, next, true
	}
	value, next, ok := readValue(payload, next, options, limits)
	if !ok {
		return mapRecord{}, position, false
	}
	record.entry.value = value
	return record, next, true
}

func readArrayRecord(payload []byte, position int, options Options, limits frame.DecoderLimits) (arrayRecord, int, bool) {
	target, next, ok := frame.ReadTag(payload, position, limits.MaxStringBytes)
	if !ok {
		return arrayRecord{}, position, false
	}
	id, next, ok := frame.ReadTag(payload, next, limits.MaxStringBytes)
	if !ok {
		return arrayRecord{}, position, false
	}
	flag, next, ok := frame.ReadUvarint(payload, next)
	if !ok || flag > 1 {
		return arrayRecord{}, position, false
	}
	node := arrayNode{id: id}
	if flag == 1 {
		parent, position, ok := frame.ReadTag(payload, next, limits.MaxStringBytes)
		if !ok {
			return arrayRecord{}, position, false
		}
		node.parent = parent
		next = position
	}
	value, next, ok := readValue(payload, next, options, limits)
	if !ok {
		return arrayRecord{}, position, false
	}
	node.value = value
	return arrayRecord{target: target, node: node}, next, true
}

func readValue(payload []byte, position int, options Options, limits frame.DecoderLimits) (Value, int, bool) {
	kind, next, ok := frame.ReadUvarint(payload, position)
	if !ok {
		return Value{}, position, false
	}
	switch kind {
	case uint64(ValueBytes):
		bytes, next, ok := frame.ReadBytes(payload, next, min(limits.MaxStringBytes, options.MaxValueBytes))
		if !ok {
			return Value{}, position, false
		}
		return Bytes(bytes), next, true
	case uint64(ValueObject):
		objectKind, next, ok := frame.ReadUvarint(payload, next)
		kind, validKind := decodedKind(objectKind)
		if !ok || !validKind {
			return Value{}, position, false
		}
		id, next, ok := frame.ReadTag(payload, next, limits.MaxStringBytes)
		if !ok {
			return Value{}, position, false
		}
		return Value{Kind: ValueObject, Object: ObjectRef{ID: id, Kind: kind}}, next, true
	default:
		return Value{}, position, false
	}
}

func readString(payload []byte, position, max int) (string, int, bool) {
	value, next, ok := frame.ReadBytes(payload, position, max)
	if !ok {
		return "", position, false
	}
	return string(value), next, true
}

func addObjectDeclSize(size *int, object objectDecl, limits frame.DecoderLimits) error {
	if len(object.id.ReplicaID) > limits.MaxStringBytes {
		return frame.ErrFrameLimit
	}
	if err := addRawSize(size, frame.TagSize(object.id)+frame.UvarintSize(uint64(object.kind))+frame.UvarintSize(uint64(object.owner.kind)), limits); err != nil {
		return err
	}
	switch object.owner.kind {
	case ownerRoot:
		return addBytesSize(size, len(object.owner.rootName), limits)
	case ownerMap:
		if len(object.owner.parent.ReplicaID) > limits.MaxStringBytes {
			return frame.ErrFrameLimit
		}
		if err := addRawSize(size, frame.TagSize(object.owner.parent), limits); err != nil {
			return err
		}
		return addBytesSize(size, len(object.owner.key), limits)
	case ownerArray:
		if len(object.owner.parent.ReplicaID) > limits.MaxStringBytes {
			return frame.ErrFrameLimit
		}
		return addRawSize(size, frame.TagSize(object.owner.parent), limits)
	default:
		return frame.ErrInvalidFrame
	}
}

func addMapRecordSize(size *int, record mapRecord, limits frame.DecoderLimits) error {
	if len(record.target.ReplicaID) > limits.MaxStringBytes || len(record.entry.tag.ReplicaID) > limits.MaxStringBytes {
		return frame.ErrFrameLimit
	}
	if err := addRawSize(size, frame.TagSize(record.target), limits); err != nil {
		return err
	}
	if err := addBytesSize(size, len(record.key), limits); err != nil {
		return err
	}
	if err := addRawSize(size, frame.TagSize(record.entry.tag)+1, limits); err != nil {
		return err
	}
	if record.entry.present {
		return addValueSize(size, record.entry.value, limits)
	}
	return nil
}

func addArrayRecordSize(size *int, record arrayRecord, limits frame.DecoderLimits) error {
	if len(record.target.ReplicaID) > limits.MaxStringBytes || len(record.node.id.ReplicaID) > limits.MaxStringBytes {
		return frame.ErrFrameLimit
	}
	if err := addRawSize(size, frame.TagSize(record.target)+frame.TagSize(record.node.id)+1, limits); err != nil {
		return err
	}
	if record.node.parent.Valid() {
		if len(record.node.parent.ReplicaID) > limits.MaxStringBytes {
			return frame.ErrFrameLimit
		}
		if err := addRawSize(size, frame.TagSize(record.node.parent), limits); err != nil {
			return err
		}
	}
	return addValueSize(size, record.node.value, limits)
}

func addValueSize(size *int, value Value, limits frame.DecoderLimits) error {
	if err := addRawSize(size, frame.UvarintSize(uint64(value.Kind)), limits); err != nil {
		return err
	}
	switch value.Kind {
	case ValueBytes:
		return addBytesSize(size, len(value.Bytes), limits)
	case ValueObject:
		if len(value.Object.ID.ReplicaID) > limits.MaxStringBytes {
			return frame.ErrFrameLimit
		}
		return addRawSize(size, frame.UvarintSize(uint64(value.Object.Kind))+frame.TagSize(value.Object.ID), limits)
	default:
		return frame.ErrInvalidFrame
	}
}

func addBytesSize(size *int, length int, limits frame.DecoderLimits) error {
	if length < 0 || length > limits.MaxStringBytes {
		return frame.ErrFrameLimit
	}
	return addRawSize(size, frame.UvarintSize(uint64(length))+length, limits)
}

func addRawSize(size *int, additional int, limits frame.DecoderLimits) error {
	if additional < 0 || additional > limits.MaxPayload-*size {
		return frame.ErrFrameLimit
	}
	*size += additional
	return nil
}

func rootRecordSize(root rootRecord) int {
	return frame.UvarintSize(uint64(len(root.name))) + len(root.name) + frame.UvarintSize(uint64(root.kind)) + frame.TagSize(root.id)
}

func objectDeclSize(object objectDecl) int {
	size := frame.TagSize(object.id) + frame.UvarintSize(uint64(object.kind)) + frame.UvarintSize(uint64(object.owner.kind))
	switch object.owner.kind {
	case ownerRoot:
		return size + frame.UvarintSize(uint64(len(object.owner.rootName))) + len(object.owner.rootName)
	case ownerMap:
		return size + frame.TagSize(object.owner.parent) + frame.UvarintSize(uint64(len(object.owner.key))) + len(object.owner.key)
	case ownerArray:
		return size + frame.TagSize(object.owner.parent)
	default:
		return size
	}
}

func mapEntrySize(target ObjectID, key string, entry mapEntry) int {
	size := frame.TagSize(target) + frame.UvarintSize(uint64(len(key))) + len(key) + frame.TagSize(entry.tag) + 1
	if entry.present {
		size += valueSize(entry.value)
	}
	return size
}

func arrayNodeSize(target ObjectID, node arrayNode) int {
	size := frame.TagSize(target) + frame.TagSize(node.id) + 1 + valueSize(node.value)
	if node.parent.Valid() {
		size += frame.TagSize(node.parent)
	}
	return size
}

func valueSize(value Value) int {
	size := frame.UvarintSize(uint64(value.Kind))
	switch value.Kind {
	case ValueBytes:
		return size + frame.UvarintSize(uint64(len(value.Bytes))) + len(value.Bytes)
	case ValueObject:
		return size + frame.UvarintSize(uint64(value.Object.Kind)) + frame.TagSize(value.Object.ID)
	default:
		return size
	}
}

func min(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func stateTagUses(state documentState) int {
	count := len(state.roots)
	for _, object := range state.objects {
		count++ // Object ID.
		if object.owner.kind == ownerMap || object.owner.kind == ownerArray {
			count++ // Parent object ID.
		}
	}
	for _, entries := range state.maps {
		for _, entry := range entries {
			count += 2 // Target and LWW write tag.
			if entry.present && entry.value.Kind == ValueObject {
				count++
			}
		}
	}
	for _, nodes := range state.arrays {
		for _, node := range nodes {
			count += 2 // Target and position ID.
			if node.parent.Valid() {
				count++
			}
			if node.value.Kind == ValueObject {
				count++
			}
		}
	}
	for _, tombstones := range state.tombstones {
		count += 2 * len(tombstones)
	}
	return count
}
