package documenttree

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

func TestDocumentTreePublicAPIEdgesAndAllValueKinds(t *testing.T) {
	if _, err := NewWithOptions("writer", Options{}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid options = %v", err)
	}
	if _, err := NewFromClockWithOptions(clock.State{}, DefaultOptions()); !errors.Is(err, ErrInvalidReplica) {
		t.Fatalf("invalid clock replica = %v", err)
	}
	var nilDocument *Document
	if nilDocument.ClockState() != (clock.State{}) || nilDocument.State().Type != "document-tree" {
		t.Fatal("nil document diagnostics")
	}
	if _, _, err := nilDocument.CreateRootMap("root"); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil root create = %v", err)
	}
	if err := nilDocument.ApplyDelta(Delta{}); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil apply = %v", err)
	}
	if err := nilDocument.Merge(nil); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil merge = %v", err)
	}
	if _, ok := nilDocument.RootMap("root"); ok {
		t.Fatal("nil document root lookup")
	}

	document := mustDocument(t, "writer")
	if _, _, err := document.CreateRootMap(" bad"); !errors.Is(err, ErrInvalidRoot) {
		t.Fatalf("invalid root = %v", err)
	}
	root, _, err := document.CreateRootMap("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if root.ID() == (ObjectID{}) {
		t.Fatal("root ID is empty")
	}
	if same, delta, err := document.CreateRootMap("workspace"); err != nil || delta.state.roots != nil || same.ID() != root.ID() {
		t.Fatalf("idempotent root = %#v %#v %v", same, delta, err)
	}
	if _, _, err := document.CreateRootArray("workspace"); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("root kind mismatch = %v", err)
	}
	if _, ok := document.Map(ObjectID{}); ok {
		t.Fatal("zero map ID accepted")
	}
	if _, ok := document.Array(ObjectID{}); ok {
		t.Fatal("zero array ID accepted")
	}

	if _, err := root.Set("title", []byte("roadmap")); err != nil {
		t.Fatal(err)
	}
	if _, err := root.SetSubdocument("notes", "subdoc-notes"); err != nil {
		t.Fatal(err)
	}
	if keys := root.Keys(); len(keys) != 2 || keys[0] != "notes" || keys[1] != "title" {
		t.Fatalf("map keys = %#v", keys)
	}
	if _, ok := root.Get("missing"); ok {
		t.Fatal("missing map entry")
	}
	if _, err := root.Set("", []byte("bad")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("invalid key = %v", err)
	}
	if _, err := root.SetSubdocument("bad", "\n"); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("invalid subdocument ID = %v", err)
	}

	array, _, err := root.CreateArray("items")
	if err != nil {
		t.Fatal(err)
	}
	if array.ID() == (ObjectID{}) || array.Len() != 0 {
		t.Fatalf("array = %#v len=%d", array, array.Len())
	}
	if _, err := array.Insert(0, []byte("scalar")); err != nil {
		t.Fatal(err)
	}
	childArray, _, err := array.InsertArray(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := childArray.Insert(0, []byte("nested")); err != nil {
		t.Fatal(err)
	}
	childMap, _, err := array.InsertMap(2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := childMap.Set("done", []byte("false")); err != nil {
		t.Fatal(err)
	}
	if _, err := array.InsertSubdocument(3, "subdoc-comments"); err != nil {
		t.Fatal(err)
	}
	if _, ok := array.Array(1); !ok {
		t.Fatal("array child lookup")
	}
	if _, ok := array.Map(2); !ok {
		t.Fatal("map child lookup")
	}
	if _, ok := array.Array(0); ok {
		t.Fatal("scalar reported as array")
	}
	if _, ok := array.Map(0); ok {
		t.Fatal("scalar reported as map")
	}
	if _, ok := array.Get(-1); ok {
		t.Fatal("negative array read")
	}
	if _, err := array.Insert(-1, []byte("bad")); !errors.Is(err, ErrRange) {
		t.Fatalf("negative insert = %v", err)
	}
	if _, err := array.Delete(3, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := array.Delete(3, 1); !errors.Is(err, ErrRange) {
		t.Fatalf("delete out of range = %v", err)
	}
	if _, err := root.Delete("title"); err != nil {
		t.Fatal(err)
	}
	if _, ok := root.Get("title"); ok {
		t.Fatal("deleted map value visible")
	}
	if _, err := json.Marshal(document); err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(Delta{}); err != nil {
		t.Fatal(err)
	}

	registry, err := NewRegistry(DefaultRegistryOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Sync(document); err != nil {
		t.Fatal(err)
	}
	if registry.Unload("missing") {
		t.Fatal("unloaded missing reference")
	}
	if _, ok := registry.Load("missing"); ok {
		t.Fatal("loaded missing reference")
	}
	if _, ok := registry.Load("subdoc-notes"); !ok || !registry.Unload("subdoc-notes") || registry.Loaded("subdoc-notes") {
		t.Fatal("registry local lifecycle")
	}
	if _, err := NewRegistry(RegistryOptions{}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid registry options = %v", err)
	}
	var nilRegistry *Registry
	if err := nilRegistry.Sync(document); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil registry sync = %v", err)
	}
	if nilRegistry.Unload("x") || nilRegistry.Loaded("x") || len(nilRegistry.Available()) != 0 {
		t.Fatal("nil registry methods")
	}
}

func TestDocumentTreeInternalValidationAndWireBranchCoverage(t *testing.T) {
	options := DefaultOptions()
	if validNameWithLimit("\xff", 4) || validNameWithLimit(" x", 4) || validNameWithLimit("\n", 4) || !validNameWithLimit("ok", 4) {
		t.Fatal("name validation")
	}
	if validStoredValue(Value{}, options) || validStoredValue(Value{Kind: ValueBytes, Object: ObjectRef{ID: ObjectID{ReplicaID: "x"}}}, options) {
		t.Fatal("stored value validation")
	}
	if !validStoredValue(Bytes([]byte("x")), options) || !validStoredValue(Subdocument("child"), options) {
		t.Fatal("valid stored values")
	}
	if validOwner(objectOwner{}, options) || !validOwner(objectOwner{kind: ownerRoot, rootName: "root"}, options) {
		t.Fatal("owner validation")
	}

	first := ObjectID{ReplicaID: "one", WallTime: 1}
	second := ObjectID{ReplicaID: "two", WallTime: 2}
	state := newDocumentState()
	state.roots["root"] = rootRecord{name: "root", id: first, kind: KindMap}
	state.objects[first] = objectDecl{id: first, kind: KindMap, owner: objectOwner{kind: ownerRoot, rootName: "root"}}
	state.objects[second] = objectDecl{id: second, kind: KindArray, owner: objectOwner{kind: ownerMap, parent: first, key: "array"}}
	state.maps[first] = map[string]mapEntry{
		"array":  {tag: second, present: true, value: Value{Kind: ValueObject, Object: ObjectRef{ID: second, Kind: KindArray}}},
		"subdoc": {tag: ObjectID{ReplicaID: "three", WallTime: 3}, present: true, value: Subdocument("subdoc")},
	}
	position := ObjectID{ReplicaID: "four", WallTime: 4}
	state.arrays[second] = map[ObjectID]arrayNode{position: {id: position, value: Bytes([]byte("value"))}}
	state.tombstones[second] = map[ObjectID]struct{}{position: {}}
	if err := validateState(state, options, false); err != nil {
		t.Fatalf("valid state = %v", err)
	}
	if got := sortedObjectIDs(state.objects); len(got) != 2 || got[0] != first {
		t.Fatalf("sorted object IDs = %#v", got)
	}
	if !sameValue(Bytes([]byte("x")), Bytes([]byte("x"))) || sameValue(Bytes([]byte("x")), Bytes([]byte("y"))) {
		t.Fatal("value equality")
	}
	if len(cloneTombstoneOuter(state.tombstones)) != 1 || len(cloneTombstoneRecords(state.tombstones[second])) != 1 {
		t.Fatal("tombstone cloning")
	}
	deepClone := state.clone()
	if len(deepClone.tombstones[second]) != 1 {
		t.Fatalf("state tombstone clone = %#v", deepClone.tombstones)
	}
	if stateTagUses(state) == 0 || documentFrontier(state)["four"] != position {
		t.Fatal("state tag accounting")
	}

	encoded, err := marshalState(crdt.TypeIDDocumentTreeState, state, options, frame.DefaultLimits(), false)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := unmarshalState(encoded, crdt.TypeIDDocumentTreeState, options, frame.DefaultLimits(), false)
	if err != nil || !reflectDocumentState(decoded, state) {
		t.Fatalf("state round trip = %v", err)
	}
	if _, err := unmarshalState(encoded, crdt.TypeIDDocumentTreeDelta, options, frame.DefaultLimits(), true); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("wrong expected type = %v", err)
	}
	if _, err := marshalState(999, state, options, frame.DefaultLimits(), false); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("unknown frame type = %v", err)
	}
	tight := frame.DefaultLimits()
	tight.MaxTags = 1
	if _, err := marshalState(crdt.TypeIDDocumentTreeState, state, options, tight, false); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tag budget = %v", err)
	}
	if _, err := UnmarshalDelta([]byte("bad")); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("bad frame = %v", err)
	}

	cycle := map[ObjectID]arrayNode{
		first:  {id: first, parent: second, value: Bytes(nil)},
		second: {id: second, parent: first, value: Bytes(nil)},
	}
	if acyclicArray(cycle) {
		t.Fatal("array cycle accepted")
	}
	conflict := Delta{state: newDocumentState()}
	conflict.state.maps[first] = map[string]mapEntry{"array": {tag: second, present: true, value: Bytes([]byte("different"))}}
	if _, _, _, err := joinState(state, conflict.state, options); !errors.Is(err, ErrTagConflict) {
		t.Fatalf("same tag conflict = %v", err)
	}

	for _, owner := range []objectOwner{
		{kind: ownerRoot, rootName: "root"},
		{kind: ownerMap, parent: first, key: "key"},
		{kind: ownerArray, parent: first, position: second},
	} {
		object := objectDecl{id: second, kind: KindMap, owner: owner}
		wire := appendObjectDecl(nil, object)
		got, next, ok := readObjectDecl(wire, 0, options, frame.DefaultLimits())
		if !ok || next != len(wire) || got.id != object.id || got.owner.kind != owner.kind {
			t.Fatalf("object owner wire = %#v %d %t", got, next, ok)
		}
	}
	for _, value := range []Value{Bytes([]byte("v")), {Kind: ValueObject, Object: ObjectRef{ID: second, Kind: KindMap}}, Subdocument("subdoc")} {
		wire := appendValue(nil, value)
		got, next, ok := readValue(wire, 0, options, frame.DefaultLimits())
		if !ok || next != len(wire) || !sameValue(got, value) {
			t.Fatalf("value wire = %#v %d %t", got, next, ok)
		}
	}
	if _, _, ok := readValue([]byte{0}, 0, options, frame.DefaultLimits()); ok {
		t.Fatal("zero value kind decoded")
	}
	if _, _, ok := readMapRecord([]byte{0}, 0, options, frame.DefaultLimits()); ok {
		t.Fatal("short map record decoded")
	}
	if _, _, ok := readArrayRecord([]byte{0}, 0, options, frame.DefaultLimits()); ok {
		t.Fatal("short array record decoded")
	}
}

func TestDocumentTreeRecoveryNilHandlesAndValidationFailures(t *testing.T) {
	if got, want := StableFrameType(), (crdt.FrameType{StateID: crdt.TypeIDDocumentTreeState, DeltaID: crdt.TypeIDDocumentTreeDelta, SemanticsVersion: SemanticsVersion, UsesHLC: true}); got != want {
		t.Fatalf("StableFrameType() = %#v", got)
	}
	source, err := New("source")
	if err != nil {
		t.Fatal(err)
	}
	array, _, err := source.CreateRootArray("items")
	if err != nil {
		t.Fatal(err)
	}
	if current, ok := source.RootArray("items"); !ok || current.ID() != array.ID() {
		t.Fatalf("RootArray = %#v, %t", current, ok)
	}
	if _, ok := source.RootMap("items"); ok {
		t.Fatal("array root exposed as map")
	}
	if _, err := array.Insert(0, []byte("one")); err != nil {
		t.Fatal(err)
	}
	state, clockState, err := source.MarshalBinaryWithClockState()
	if err != nil || clockState != source.ClockState() {
		t.Fatalf("MarshalBinaryWithClockState = %v %#v", err, clockState)
	}
	tight := frame.DefaultLimits()
	tight.MaxFrameBytes = 1
	if _, _, err := source.MarshalBinaryWithClockStateAndLimits(tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tight state output = %v", err)
	}
	target := mustDocument(t, "target")
	if err := target.UnmarshalBinary(state); err != nil {
		t.Fatal(err)
	}
	if err := target.Merge(source); err != nil {
		t.Fatal(err)
	}
	if err := target.Merge(nil); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil merge = %v", err)
	}
	if err := target.UnmarshalBinary([]byte("short")); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("bad state input = %v", err)
	}
	saved := mustSnapshot(t, source)
	restored, err := NewFromSnapshotWithOptions(saved, DefaultOptions(), frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := restored.RootArray("items"); !ok {
		t.Fatal("restored root array")
	}
	if _, err := NewFromSnapshotWithOptions(saved, Options{}, frame.DefaultLimits()); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid restore options = %v", err)
	}
	if _, err := NewFromSnapshotWithOptions(snapshot.Snapshot{}, DefaultOptions(), frame.DefaultLimits()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid snapshot type = %v", err)
	}
	if err := source.validateValue(Value{Kind: ValueObject}); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("public object injection = %v", err)
	}
	over := DefaultOptions()
	over.MaxValueBytes = 1
	limited := mustDocumentWithOptions(t, "limited", over)
	if err := limited.validateValue(Bytes([]byte("too long"))); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("value limit = %v", err)
	}

	var nilMap *Map
	if nilMap.ID() != (ObjectID{}) {
		t.Fatal("nil map ID")
	}
	if _, err := nilMap.Set("x", nil); !errors.Is(err, ErrNilDocument) {
		t.Fatal(err)
	}
	if _, err := nilMap.SetSubdocument("x", "sub"); !errors.Is(err, ErrNilDocument) {
		t.Fatal(err)
	}
	if _, err := nilMap.Delete("x"); !errors.Is(err, ErrNilDocument) {
		t.Fatal(err)
	}
	if _, _, err := nilMap.CreateMap("x"); !errors.Is(err, ErrNilDocument) {
		t.Fatal(err)
	}
	if _, _, err := nilMap.CreateArray("x"); !errors.Is(err, ErrNilDocument) {
		t.Fatal(err)
	}
	if _, ok := nilMap.Get("x"); ok || len(nilMap.Keys()) != 0 {
		t.Fatal("nil map reads")
	}

	var nilArray *Array
	if nilArray.ID() != (ObjectID{}) || nilArray.Len() != 0 {
		t.Fatal("nil array diagnostics")
	}
	if _, err := nilArray.Insert(0, nil); !errors.Is(err, ErrNilDocument) {
		t.Fatal(err)
	}
	if _, err := nilArray.InsertSubdocument(0, "sub"); !errors.Is(err, ErrNilDocument) {
		t.Fatal(err)
	}
	if _, _, err := nilArray.InsertMap(0); !errors.Is(err, ErrNilDocument) {
		t.Fatal(err)
	}
	if _, _, err := nilArray.InsertArray(0); !errors.Is(err, ErrNilDocument) {
		t.Fatal(err)
	}
	if _, err := nilArray.Delete(0, 1); !errors.Is(err, ErrNilDocument) {
		t.Fatal(err)
	}
	if _, ok := nilArray.Get(0); ok {
		t.Fatal("nil array get")
	}

	if compareTombstoneRecord(tombstoneRecord{target: ObjectID{ReplicaID: "a", WallTime: 1}, id: ObjectID{ReplicaID: "a", WallTime: 2}}, tombstoneRecord{target: ObjectID{ReplicaID: "b", WallTime: 1}, id: ObjectID{ReplicaID: "b", WallTime: 2}}) >= 0 {
		t.Fatal("tombstone ordering")
	}
	if rootRecordSize(rootRecord{name: "root", id: ObjectID{ReplicaID: "a", WallTime: 1}, kind: KindMap}) == 0 {
		t.Fatal("root record size")
	}
	if objectDeclSize(objectDecl{id: ObjectID{ReplicaID: "a", WallTime: 1}, kind: KindMap, owner: objectOwner{kind: ownerArray, parent: ObjectID{ReplicaID: "b", WallTime: 2}, position: ObjectID{ReplicaID: "a", WallTime: 1}}}) == 0 {
		t.Fatal("object record size")
	}
	if min(1, 2) != 1 || min(2, 1) != 1 {
		t.Fatal("min")
	}
}

func TestDocumentTreeMalformedCandidatesAndWireSizing(t *testing.T) {
	options := DefaultOptions()
	first := ObjectID{ReplicaID: "one", WallTime: 1}
	second := ObjectID{ReplicaID: "two", WallTime: 2}

	missingRoot := newDocumentState()
	missingRoot.roots["root"] = rootRecord{name: "root", id: first, kind: KindMap}
	if err := validateState(missingRoot, options, false); !errors.Is(err, ErrIncompleteState) {
		t.Fatalf("missing root object = %v", err)
	}
	tightRoots := options
	tightRoots.MaxRoots = 1
	tooManyRoots := missingRoot.clone()
	tooManyRoots.roots["other"] = rootRecord{name: "other", id: second, kind: KindMap}
	if err := validateState(tooManyRoots, tightRoots, true); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("root limit = %v", err)
	}

	wrongParent := newDocumentState()
	wrongParent.objects[first] = objectDecl{id: first, kind: KindArray, owner: objectOwner{kind: ownerRoot, rootName: "root"}}
	wrongParent.objects[second] = objectDecl{id: second, kind: KindMap, owner: objectOwner{kind: ownerMap, parent: first, key: "child"}}
	if err := validateState(wrongParent, options, true); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("map child of array = %v", err)
	}

	wrongReference := newDocumentState()
	wrongReference.objects[first] = objectDecl{id: first, kind: KindMap, owner: objectOwner{kind: ownerRoot, rootName: "root"}}
	wrongReference.objects[second] = objectDecl{id: second, kind: KindMap, owner: objectOwner{kind: ownerMap, parent: first, key: "child"}}
	wrongReference.maps[first] = map[string]mapEntry{"child": {tag: ObjectID{ReplicaID: "third", WallTime: 3}, present: true, value: Value{Kind: ValueObject, Object: ObjectRef{ID: second, Kind: KindMap}}}}
	if err := validateState(wrongReference, options, true); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("child reference tag mismatch = %v", err)
	}

	tagCollision := newDocumentState()
	tagCollision.roots["root"] = rootRecord{name: "root", id: first, kind: KindMap}
	tagCollision.maps[second] = map[string]mapEntry{"key": {tag: first, present: true, value: Bytes(nil)}}
	if err := validateState(tagCollision, options, true); !errors.Is(err, ErrTagConflict) {
		t.Fatalf("global tag collision = %v", err)
	}

	ownerCycle := newDocumentState()
	ownerCycle.objects[first] = objectDecl{id: first, kind: KindMap, owner: objectOwner{kind: ownerMap, parent: second, key: "a"}}
	ownerCycle.objects[second] = objectDecl{id: second, kind: KindMap, owner: objectOwner{kind: ownerMap, parent: first, key: "b"}}
	if err := validateState(ownerCycle, options, true); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("object owner cycle = %v", err)
	}

	document := mustDocument(t, "writer")
	unknown := ObjectID{ReplicaID: "unknown", WallTime: 9}
	unknownMap := &Map{document: document, id: unknown}
	if _, err := unknownMap.Set("x", nil); !errors.Is(err, ErrUnknownObject) {
		t.Fatalf("unknown map set = %v", err)
	}
	if _, err := unknownMap.Delete("x"); !errors.Is(err, ErrUnknownObject) {
		t.Fatalf("unknown map delete = %v", err)
	}
	if _, _, err := unknownMap.CreateMap("x"); !errors.Is(err, ErrUnknownObject) {
		t.Fatalf("unknown map child = %v", err)
	}
	unknownArray := &Array{document: document, id: unknown}
	if _, err := unknownArray.Insert(0, nil); !errors.Is(err, ErrUnknownObject) {
		t.Fatalf("unknown array insert = %v", err)
	}
	if _, err := unknownArray.Delete(-1, 1); !errors.Is(err, ErrRange) {
		t.Fatalf("negative delete = %v", err)
	}

	limits := frame.DefaultLimits()
	limits.MaxStringBytes = 1
	shortFirst := ObjectID{ReplicaID: "a", WallTime: 1}
	shortSecond := ObjectID{ReplicaID: "b", WallTime: 2}
	for _, object := range []objectDecl{
		{id: shortFirst, kind: KindMap, owner: objectOwner{kind: ownerRoot, rootName: "too-long"}},
		{id: shortFirst, kind: KindMap, owner: objectOwner{kind: ownerMap, parent: shortSecond, key: "too-long"}},
		{id: shortFirst, kind: KindMap, owner: objectOwner{kind: ownerArray, parent: ObjectID{ReplicaID: "too-long", WallTime: 2}, position: shortFirst}},
	} {
		size := 0
		if err := addObjectDeclSize(&size, object, limits); !errors.Is(err, frame.ErrFrameLimit) {
			t.Fatalf("object size limit = %v", err)
		}
	}
	size := 0
	if err := addMapRecordSize(&size, mapRecord{target: shortFirst, key: "too-long", entry: mapEntry{tag: shortSecond, present: true, value: Bytes([]byte("too-long"))}}, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("map record size = %v", err)
	}
	size = 0
	if err := addArrayRecordSize(&size, arrayRecord{target: shortFirst, node: arrayNode{id: shortSecond, parent: shortFirst, value: Subdocument("too-long")}}, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("array record size = %v", err)
	}
	size = 0
	if err := addValueSize(&size, Value{}, frame.DefaultLimits()); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("invalid value size = %v", err)
	}
	if err := addBytesSize(&size, 2, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("bytes size = %v", err)
	}
	if err := addRawSize(&size, frame.DefaultLimits().MaxPayload+1, frame.DefaultLimits()); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("payload overflow = %v", err)
	}

	for _, object := range []objectDecl{
		{id: shortFirst, kind: KindMap, owner: objectOwner{kind: ownerRoot, rootName: "r"}},
		{id: shortFirst, kind: KindMap, owner: objectOwner{kind: ownerMap, parent: shortSecond, key: "k"}},
		{id: shortFirst, kind: KindMap, owner: objectOwner{kind: ownerArray, parent: shortSecond, position: shortFirst}},
	} {
		size = 0
		if err := addObjectDeclSize(&size, object, frame.DefaultLimits()); err != nil || size == 0 {
			t.Fatalf("normal object size = %d, %v", size, err)
		}
	}
	for _, value := range []Value{Bytes([]byte("v")), {Kind: ValueObject, Object: ObjectRef{ID: shortFirst, Kind: KindMap}}, Subdocument("sub")} {
		size = 0
		if err := addValueSize(&size, value, frame.DefaultLimits()); err != nil || size == 0 {
			t.Fatalf("normal value size = %d, %v", size, err)
		}
	}
	for _, record := range []mapRecord{
		{target: shortFirst, key: "k", entry: mapEntry{tag: shortSecond}},
		{target: shortFirst, key: "k", entry: mapEntry{tag: shortSecond, present: true, value: Value{Kind: ValueObject, Object: ObjectRef{ID: shortSecond, Kind: KindMap}}}},
		{target: shortFirst, key: "k", entry: mapEntry{tag: shortSecond, present: true, value: Subdocument("sub")}},
	} {
		size = 0
		if err := addMapRecordSize(&size, record, frame.DefaultLimits()); err != nil || size == 0 {
			t.Fatalf("normal map size = %d, %v", size, err)
		}
	}
	for _, record := range []arrayRecord{
		{target: shortFirst, node: arrayNode{id: shortSecond, value: Bytes([]byte("v"))}},
		{target: shortFirst, node: arrayNode{id: shortSecond, parent: shortFirst, value: Value{Kind: ValueObject, Object: ObjectRef{ID: shortSecond, Kind: KindMap}}}},
		{target: shortFirst, node: arrayNode{id: shortSecond, parent: shortFirst, value: Subdocument("sub")}},
	} {
		size = 0
		if err := addArrayRecordSize(&size, record, frame.DefaultLimits()); err != nil || size == 0 {
			t.Fatalf("normal array size = %d, %v", size, err)
		}
	}
}

func TestDocumentTreeValidateStateRejectsEveryRecordClass(t *testing.T) {
	options := DefaultOptions()
	validTag := ObjectID{ReplicaID: "valid", WallTime: 1}
	assertInvalid := func(name string, state documentState, want error) {
		t.Helper()
		if err := validateState(state, options, true); !errors.Is(err, want) {
			t.Fatalf("%s = %v, want %v", name, err, want)
		}
	}
	invalidOptions := options
	invalidOptions.MaxRoots = 0
	if err := validateState(newDocumentState(), invalidOptions, true); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid options = %v", err)
	}

	state := newDocumentState()
	state.roots["root"] = rootRecord{name: "other", id: validTag, kind: KindMap}
	assertInvalid("root name mismatch", state, ErrInvalidDelta)
	state = newDocumentState()
	state.objects[validTag] = objectDecl{id: validTag, kind: Kind(99), owner: objectOwner{kind: ownerRoot, rootName: "root"}}
	assertInvalid("object kind", state, ErrInvalidDelta)
	state = newDocumentState()
	state.maps[ObjectID{}] = map[string]mapEntry{"key": {tag: validTag, present: true, value: Bytes(nil)}}
	assertInvalid("map target", state, ErrInvalidDelta)
	state = newDocumentState()
	state.maps[validTag] = map[string]mapEntry{"": {tag: validTag, present: true, value: Bytes(nil)}}
	assertInvalid("map key", state, ErrInvalidDelta)
	state = newDocumentState()
	state.maps[validTag] = map[string]mapEntry{"key": {tag: validTag, present: true, value: Value{Kind: ValueBytes, Subdocument: SubdocumentRef{ID: "bad"}}}}
	assertInvalid("map value", state, ErrInvalidDelta)
	state = newDocumentState()
	state.arrays[ObjectID{}] = map[ObjectID]arrayNode{validTag: {id: validTag, value: Bytes(nil)}}
	assertInvalid("array target", state, ErrInvalidDelta)
	state = newDocumentState()
	state.arrays[validTag] = map[ObjectID]arrayNode{validTag: {id: validTag, parent: validTag, value: Bytes(nil)}}
	assertInvalid("array self parent", state, ErrInvalidDelta)
	state = newDocumentState()
	state.tombstones[ObjectID{}] = map[ObjectID]struct{}{validTag: {}}
	assertInvalid("tombstone target", state, ErrInvalidDelta)
	state = newDocumentState()
	state.tombstones[validTag] = map[ObjectID]struct{}{{}: {}}
	assertInvalid("tombstone id", state, ErrInvalidDelta)

	root := ObjectID{ReplicaID: "root", WallTime: 1}
	child := ObjectID{ReplicaID: "child", WallTime: 2}
	state = newDocumentState()
	state.roots["root"] = rootRecord{name: "root", id: root, kind: KindMap}
	state.objects[root] = objectDecl{id: root, kind: KindArray, owner: objectOwner{kind: ownerRoot, rootName: "root"}}
	assertInvalid("root declaration kind", state, ErrInvalidState)
	state = newDocumentState()
	state.objects[root] = objectDecl{id: root, kind: KindMap, owner: objectOwner{kind: ownerRoot, rootName: "root"}}
	state.arrays[root] = map[ObjectID]arrayNode{child: {id: child, value: Bytes(nil)}}
	assertInvalid("array target kind", state, ErrTypeMismatch)
	state = newDocumentState()
	state.objects[root] = objectDecl{id: root, kind: KindArray, owner: objectOwner{kind: ownerRoot, rootName: "root"}}
	state.objects[child] = objectDecl{id: child, kind: KindMap, owner: objectOwner{kind: ownerArray, parent: root, position: child}}
	state.arrays[root] = map[ObjectID]arrayNode{child: {id: child, value: Value{Kind: ValueObject, Object: ObjectRef{ID: child, Kind: KindMap}}}}
	if err := validateState(state, options, true); err != nil {
		t.Fatalf("valid array child = %v", err)
	}
	state.arrays[root][child] = arrayNode{id: child, value: Value{Kind: ValueObject, Object: ObjectRef{ID: root, Kind: KindMap}}}
	assertInvalid("array child ID mismatch", state, ErrInvalidState)

	pending := newDocumentState()
	pending.roots["root"] = rootRecord{name: "root", id: root, kind: KindMap}
	pending.maps[child] = map[string]mapEntry{"key": {tag: child, present: true, value: Bytes(nil)}}
	tightPending := options
	tightPending.MaxPendingOperations = 1
	if err := validateState(pending, tightPending, true); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("pending operation bound = %v", err)
	}
	clone := pending.clone()
	if len(clone.tombstones) != 0 || len(clone.roots) != len(pending.roots) {
		t.Fatalf("state clone = %#v", clone)
	}
}

func TestDocumentTreeDecoderRejectsDeepMalformedFields(t *testing.T) {
	options := DefaultOptions()
	limits := frame.DefaultLimits()
	first := ObjectID{ReplicaID: "one", WallTime: 1}
	second := ObjectID{ReplicaID: "two", WallTime: 2}

	object := frame.AppendTag(nil, first)
	object = frame.AppendUvarint(object, uint64(KindMap))
	object = frame.AppendUvarint(object, 99)
	if _, _, ok := readObjectDecl(object, 0, options, limits); ok {
		t.Fatal("unknown object owner decoded")
	}

	mapRecordBytes := frame.AppendTag(nil, first)
	mapRecordBytes = appendBytes(mapRecordBytes, []byte("key"))
	mapRecordBytes = frame.AppendTag(mapRecordBytes, second)
	mapRecordBytes = frame.AppendUvarint(mapRecordBytes, 2)
	if _, _, ok := readMapRecord(mapRecordBytes, 0, options, limits); ok {
		t.Fatal("invalid map present flag decoded")
	}
	mapRecordBytes[len(mapRecordBytes)-1] = 1
	mapRecordBytes = frame.AppendUvarint(mapRecordBytes, 0)
	if _, _, ok := readMapRecord(mapRecordBytes, 0, options, limits); ok {
		t.Fatal("invalid map value decoded")
	}

	arrayRecordBytes := frame.AppendTag(nil, first)
	arrayRecordBytes = frame.AppendTag(arrayRecordBytes, second)
	arrayRecordBytes = frame.AppendUvarint(arrayRecordBytes, 2)
	if _, _, ok := readArrayRecord(arrayRecordBytes, 0, options, limits); ok {
		t.Fatal("invalid array parent flag decoded")
	}
	arrayRecordBytes[len(arrayRecordBytes)-1] = 1
	if _, _, ok := readArrayRecord(arrayRecordBytes, 0, options, limits); ok {
		t.Fatal("truncated array parent decoded")
	}

	invalidObjectValue := frame.AppendUvarint(nil, uint64(ValueObject))
	invalidObjectValue = frame.AppendUvarint(invalidObjectValue, 99)
	if _, _, ok := readValue(invalidObjectValue, 0, options, limits); ok {
		t.Fatal("invalid object value kind decoded")
	}
	valueLimit := options
	valueLimit.MaxValueBytes = 1
	tooLongBytes := frame.AppendUvarint(nil, uint64(ValueBytes))
	tooLongBytes = appendBytes(tooLongBytes, []byte("xx"))
	if _, _, ok := readValue(tooLongBytes, 0, valueLimit, limits); ok {
		t.Fatal("over-limit bytes decoded")
	}
	if _, _, ok := readString([]byte{0x80}, 0, 8); ok {
		t.Fatal("truncated string decoded")
	}

	document := mustDocument(t, "writer")
	root, _, err := document.CreateRootArray("items")
	if err != nil {
		t.Fatal(err)
	}
	// Two same-parent positions exercise the deterministic descending sibling
	// ordering branch in the visible RGA projection.
	document.mu.Lock()
	left := ObjectID{ReplicaID: "left", WallTime: 10}
	right := ObjectID{ReplicaID: "right", WallTime: 11}
	document.state.arrays[root.id] = map[ObjectID]arrayNode{
		left:  {id: left, value: Bytes([]byte("left"))},
		right: {id: right, value: Bytes([]byte("right"))},
	}
	visible := document.visibleArrayNodesLocked(root.id)
	document.mu.Unlock()
	if len(visible) != 2 || visible[0].id != right {
		t.Fatalf("sibling order = %#v", visible)
	}
}

func TestDocumentTreeLimitsAndRecoveryEdgePaths(t *testing.T) {
	mapOptions := DefaultOptions()
	mapOptions.MaxMapEntries = 1
	mapDocument := mustDocumentWithOptions(t, "map-writer", mapOptions)
	mapRoot, _, err := mapDocument.CreateRootMap("root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mapRoot.Set("one", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if _, err := mapRoot.Set("two", []byte("2")); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("map entry bound = %v", err)
	}
	if _, err := mapRoot.Delete(""); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("invalid delete key = %v", err)
	}
	if _, _, err := mapRoot.CreateMap(""); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("invalid child key = %v", err)
	}
	if err := mapDocument.validateValue(Value{Kind: ValueKind(99)}); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("invalid value kind = %v", err)
	}
	if mapDocument.validName("\n", mapDocument.options.MaxKeyBytes) {
		t.Fatal("control character accepted as name")
	}

	arrayOptions := DefaultOptions()
	arrayOptions.MaxArrayNodes = 1
	arrayDocument := mustDocumentWithOptions(t, "array-writer", arrayOptions)
	arrayRoot, _, err := arrayDocument.CreateRootArray("root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := arrayRoot.Insert(0, []byte("1")); err != nil {
		t.Fatal(err)
	}
	if _, err := arrayRoot.Insert(1, []byte("2")); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("array node bound = %v", err)
	}
	if _, err := arrayDocument.insertArrayValue(arrayRoot.id, 0, Value{}, Kind(99)); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("invalid child kind = %v", err)
	}
	arrayDocument.mu.Lock()
	if depth := arrayDocument.childDepthLocked(ObjectID{ReplicaID: "missing", WallTime: 1}); depth != 1 {
		arrayDocument.mu.Unlock()
		t.Fatalf("unknown parent depth = %d", depth)
	}
	arrayDocument.mu.Unlock()

	if err := mapDocument.ApplyDelta(Delta{}); err != nil {
		t.Fatalf("empty delta = %v", err)
	}
	if err := mapDocument.Merge(mapDocument); err != nil {
		t.Fatalf("self merge = %v", err)
	}
	if _, err := mapDocument.SnapshotCurrentStateWithLimits(frame.DecoderLimits{MaxFrameBytes: 1}); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tight snapshot output = %v", err)
	}
	state, err := mapDocument.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	clockless, err := snapshot.New(state, documentFrontier(mapDocument.state))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewFromSnapshotWithOptions(clockless, DefaultOptions(), frame.DefaultLimits()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("clockless snapshot restore = %v", err)
	}

	var nilDocument *Document
	if _, err := nilDocument.MarshalBinaryWithLimits(frame.DefaultLimits()); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil marshal = %v", err)
	}
	if _, err := nilDocument.SnapshotCurrentState(); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil snapshot = %v", err)
	}
	if refs := nilDocument.Subdocuments(); refs != nil {
		t.Fatalf("nil subdocuments = %#v", refs)
	}

	registryDocument := mustDocument(t, "registry-writer")
	registryRoot, _, err := registryDocument.CreateRootMap("root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registryRoot.SetSubdocument("a", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := registryRoot.SetSubdocument("b", "b"); err != nil {
		t.Fatal(err)
	}
	limitedRegistry, err := NewRegistry(RegistryOptions{MaxSubdocuments: 1, MaxIDBytes: 16})
	if err != nil {
		t.Fatal(err)
	}
	if err := limitedRegistry.Sync(registryDocument); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("registry reference bound = %v", err)
	}
	registry, err := NewRegistry(DefaultRegistryOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Sync(registryDocument); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Load("a"); !ok {
		t.Fatal("registry load")
	}
	if err := registry.Sync(registryDocument); err != nil || !registry.Loaded("a") {
		t.Fatalf("registry preserves visible loaded reference: %v", err)
	}
}

func reflectDocumentState(left, right documentState) bool {
	leftFrame, leftErr := marshalState(crdt.TypeIDDocumentTreeState, left, DefaultOptions(), frame.DefaultLimits(), false)
	rightFrame, rightErr := marshalState(crdt.TypeIDDocumentTreeState, right, DefaultOptions(), frame.DefaultLimits(), false)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftFrame, rightFrame)
}
