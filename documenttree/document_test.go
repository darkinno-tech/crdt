package documenttree

import (
	"bytes"
	"errors"
	"math/rand"
	"reflect"
	"testing"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/replica"
	"github.com/DarkInno/crdt/snapshot"
)

func TestDocumentTreeNestedMapArrayConvergesAcrossDuplicateReorderedFrames(t *testing.T) {
	alice := mustDocument(t, "alice")
	bob := mustDocument(t, "bob")
	carol := mustDocument(t, "carol")

	root, rootDelta, err := alice.CreateRootMap("workspace")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []*Document{bob, carol} {
		if err := target.ApplyDelta(rootDelta); err != nil {
			t.Fatal(err)
		}
	}
	bobRoot, ok := bob.RootMap("workspace")
	if !ok {
		t.Fatal("bob root is missing")
	}
	carolRoot, ok := carol.RootMap("workspace")
	if !ok {
		t.Fatal("carol root is missing")
	}

	changes := make([]Delta, 0, 12)
	board, delta, err := root.CreateMap("board")
	if err != nil {
		t.Fatal(err)
	}
	changes = append(changes, delta)
	cards, delta, err := board.CreateArray("cards")
	if err != nil {
		t.Fatal(err)
	}
	changes = append(changes, delta)
	card, delta, err := cards.InsertMap(0)
	if err != nil {
		t.Fatal(err)
	}
	changes = append(changes, delta)
	for key, value := range map[string]string{"id": "card-1", "title": "nested protocol", "state": "draft"} {
		delta, err = card.Set(key, []byte(value))
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, delta)
	}
	labels, delta, err := card.CreateArray("labels")
	if err != nil {
		t.Fatal(err)
	}
	changes = append(changes, delta)
	for _, label := range []string{"protocol", "security"} {
		delta, err = labels.Insert(labels.Len(), []byte(label))
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, delta)
	}
	delta, err = bobRoot.Set("owner", []byte("bob"))
	if err != nil {
		t.Fatal(err)
	}
	changes = append(changes, delta)
	activity, delta, err := carolRoot.CreateArray("activity")
	if err != nil {
		t.Fatal(err)
	}
	changes = append(changes, delta)
	delta, err = activity.InsertSubdocument(0, "workspace-card-1-comments")
	if err != nil {
		t.Fatal(err)
	}
	changes = append(changes, delta)

	for index, target := range []*Document{alice, bob, carol} {
		deliverDocumentTreeDeltas(t, target, changes, int64(20260731+index))
	}

	var expected []byte
	for index, target := range []*Document{alice, bob, carol} {
		state, err := target.MarshalBinary()
		if err != nil {
			t.Fatalf("replica %d snapshot: %v", index, err)
		}
		if index == 0 {
			expected = state
		} else if !bytes.Equal(state, expected) {
			t.Fatalf("replica %d diverged\n got: %x\nwant: %x", index, state, expected)
		}
	}

	resultRoot, ok := bob.RootMap("workspace")
	if !ok {
		t.Fatal("converged root missing")
	}
	resultBoard, ok := resultRoot.Map("board")
	if !ok {
		t.Fatal("nested board missing")
	}
	resultCards, ok := resultBoard.Array("cards")
	if !ok || resultCards.Len() != 1 {
		t.Fatalf("cards = %#v, len=%d", resultCards, resultCards.Len())
	}
	resultCard, ok := resultCards.Map(0)
	if !ok {
		t.Fatal("nested card missing")
	}
	if value, ok := resultCard.Get("state"); !ok || string(value.Bytes) != "draft" {
		t.Fatalf("card state = %#v, %t", value, ok)
	}
	resultLabels, ok := resultCard.Array("labels")
	if !ok || resultLabels.Len() != 2 {
		t.Fatalf("labels = %#v, len=%d", resultLabels, resultLabels.Len())
	}
	if value, ok := resultLabels.Get(1); !ok || string(value.Bytes) != "security" {
		t.Fatalf("second label = %#v, %t", value, ok)
	}
	if references := carol.Subdocuments(); !reflect.DeepEqual(references, []SubdocumentRef{{ID: "workspace-card-1-comments"}}) {
		t.Fatalf("Subdocuments() = %#v", references)
	}
}

func TestDocumentTreePendingParentRejectsSnapshotThenRecovers(t *testing.T) {
	source := mustDocument(t, "source")
	root, rootDelta, err := source.CreateRootMap("workspace")
	if err != nil {
		t.Fatal(err)
	}
	child, childDelta, err := root.CreateMap("child")
	if err != nil {
		t.Fatal(err)
	}
	setDelta, err := child.Set("status", []byte("waiting"))
	if err != nil {
		t.Fatal(err)
	}

	target := mustDocument(t, "target")
	for _, delta := range []Delta{setDelta, childDelta} {
		if err := target.ApplyDelta(delta); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := target.MarshalBinary(); !errors.Is(err, ErrIncompleteState) {
		t.Fatalf("MarshalBinary(pending) = %v, want %v", err, ErrIncompleteState)
	}
	if err := target.ApplyDelta(rootDelta); err != nil {
		t.Fatal(err)
	}
	state, err := target.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if restored, err := NewFromSnapshot(mustSnapshot(t, target)); err != nil {
		t.Fatal(err)
	} else if next, _, err := restored.CreateRootMap("another"); err != nil || !next.ID().Valid() {
		t.Fatalf("recovered local root = %#v, %v", next, err)
	}
	if len(state) == 0 {
		t.Fatal("completed state was empty")
	}
}

func TestDocumentTreeRejectsTypeConfusionAndChildAliasingWithoutMutation(t *testing.T) {
	document := mustDocument(t, "writer")
	array, _, err := document.CreateRootArray("items")
	if err != nil {
		t.Fatal(err)
	}
	before, err := document.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	badType := Delta{state: newDocumentState()}
	badType.state.maps[array.id] = map[string]mapEntry{
		"wrong": {tag: ObjectID{ReplicaID: "attacker", WallTime: 1}, present: true, value: Bytes([]byte("x"))},
	}
	if err := document.ApplyDelta(badType); !errors.Is(err, ErrTypeMismatch) {
		t.Fatalf("array-as-map delta = %v", err)
	}
	after, err := document.MarshalBinary()
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("type rejection mutated state: %v\n got: %x\nwant: %x", err, after, before)
	}

	root, _, err := document.CreateRootMap("workspace")
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := root.CreateMap("first")
	if err != nil {
		t.Fatal(err)
	}
	before, err = document.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	alias := Delta{state: newDocumentState()}
	alias.state.maps[root.id] = map[string]mapEntry{
		"second": {tag: child.id, present: true, value: Value{Kind: ValueObject, Object: ObjectRef{ID: child.id, Kind: KindMap}}},
	}
	if err := document.ApplyDelta(alias); !errors.Is(err, ErrTagConflict) {
		t.Fatalf("aliased child = %v, want %v", err, ErrTagConflict)
	}
	after, err = document.MarshalBinary()
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("alias rejection mutated state: %v", err)
	}
}

func TestDocumentTreeSubdocumentRegistryIsLocalAndBounded(t *testing.T) {
	document := mustDocument(t, "writer")
	root, _, err := document.CreateRootMap("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.SetSubdocument("notes", "subdoc-notes"); err != nil {
		t.Fatal(err)
	}
	items, _, err := root.CreateArray("items")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := items.InsertSubdocument(0, "subdoc-comments"); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(DefaultRegistryOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Sync(document); err != nil {
		t.Fatal(err)
	}
	if got, want := registry.Available(), []SubdocumentRef{{ID: "subdoc-comments"}, {ID: "subdoc-notes"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Available() = %#v, want %#v", got, want)
	}
	if loaded, ok := registry.Load("subdoc-notes"); !ok || loaded.ID != "subdoc-notes" || !registry.Loaded("subdoc-notes") {
		t.Fatalf("Load = %#v, %t, loaded=%t", loaded, ok, registry.Loaded("subdoc-notes"))
	}
	if _, err := root.Delete("notes"); err != nil {
		t.Fatal(err)
	}
	if err := registry.Sync(document); err != nil {
		t.Fatal(err)
	}
	if registry.Loaded("subdoc-notes") || len(registry.Available()) != 1 {
		t.Fatalf("registry after parent removal = loaded=%t refs=%#v", registry.Loaded("subdoc-notes"), registry.Available())
	}
}

func TestDocumentTreeFrameManifestAndSnapshotRecovery(t *testing.T) {
	manifest, err := replica.NewManifest("workspace/42", "example.com/workspace/v1", 1, replica.Protocol{
		StateID: crdt.TypeIDDocumentTreeState, DeltaID: crdt.TypeIDDocumentTreeDelta, SemanticsVersion: SemanticsVersion,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	source := mustDocument(t, "source")
	root, delta, err := source.CreateRootMap("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.Set("title", []byte("framed")); err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	target := mustDocument(t, "target")
	frontier, err := replica.NewFrontier(nil)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := replica.NewInbox(manifest, frontier, 8, frame.DefaultLimits().MaxFrameBytes, func(bytes []byte) error {
		change, err := UnmarshalDelta(bytes)
		if err != nil {
			return err
		}
		return target.ApplyDelta(change)
	})
	if err != nil {
		t.Fatal(err)
	}
	change, err := replica.NewChange(manifest, replica.Dot{Actor: "source", Counter: 1}, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if delivery, err := inbox.Receive(change); err != nil || len(delivery.Applied) != 1 {
		t.Fatalf("Receive = %#v, %v", delivery, err)
	}
	recovered, err := NewFromSnapshot(mustSnapshot(t, source))
	if err != nil {
		t.Fatal(err)
	}
	if root, ok := recovered.RootMap("workspace"); !ok || root == nil {
		t.Fatal("recovered root missing")
	}
}

func TestDocumentTreeWireRoundTripAndLimits(t *testing.T) {
	document := mustDocument(t, "writer")
	root, delta, err := document.CreateRootMap("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.Set("title", []byte("canonical")); err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalDelta(encoded)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := decoded.MarshalBinary()
	if err != nil || !bytes.Equal(reencoded, encoded) {
		t.Fatalf("delta round trip = %v\n got: %x\nwant: %x", err, reencoded, encoded)
	}
	limits := frame.DefaultLimits()
	limits.MaxFrameBytes = 1
	if _, err := delta.MarshalBinaryWithLimits(limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tight delta limit = %v", err)
	}
	if _, err := UnmarshalDeltaWithLimits(encoded, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tight delta decode = %v", err)
	}
}

func TestDocumentTreeLocalLimitsRejectBeforeAdvancingClockOrState(t *testing.T) {
	options := DefaultOptions()
	options.MaxObjects = 1
	document, err := NewWithOptions("writer", options)
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := document.CreateRootMap("workspace")
	if err != nil {
		t.Fatal(err)
	}
	clockBefore := document.ClockState()
	stateBefore, err := document.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := root.CreateMap("too-many"); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("object limit = %v", err)
	}
	if stateAfter, err := document.MarshalBinary(); err != nil || !bytes.Equal(stateAfter, stateBefore) {
		t.Fatalf("object limit changed state: %v", err)
	}
	if clockAfter := document.ClockState(); clockAfter != clockBefore {
		t.Fatalf("object limit advanced clock: got %#v, want %#v", clockAfter, clockBefore)
	}

	depthOptions := DefaultOptions()
	depthOptions.MaxDepth = 1
	depth := mustDocumentWithOptions(t, "depth", depthOptions)
	depthRoot, _, err := depth.CreateRootMap("workspace")
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := depthRoot.CreateMap("child")
	if err != nil {
		t.Fatal(err)
	}
	clockBefore = depth.ClockState()
	if _, _, err := child.CreateMap("too-deep"); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("depth limit = %v", err)
	}
	if clockAfter := depth.ClockState(); clockAfter != clockBefore {
		t.Fatalf("depth limit advanced clock: got %#v, want %#v", clockAfter, clockBefore)
	}
}

func deliverDocumentTreeDeltas(t testing.TB, target *Document, changes []Delta, seed int64) {
	t.Helper()
	frames := make([][]byte, 0, len(changes)*2)
	for _, change := range changes {
		encoded, err := change.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, encoded, encoded)
	}
	random := rand.New(rand.NewSource(seed))
	random.Shuffle(len(frames), func(left, right int) { frames[left], frames[right] = frames[right], frames[left] })
	for _, encoded := range frames {
		change, err := UnmarshalDelta(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if err := target.ApplyDelta(change); err != nil {
			t.Fatal(err)
		}
	}
}

func mustDocument(t testing.TB, replicaID string) *Document {
	return mustDocumentWithOptions(t, replicaID, DefaultOptions())
}

func mustDocumentWithOptions(t testing.TB, replicaID string, options Options) *Document {
	t.Helper()
	document, err := NewWithOptions(replicaID, options)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func mustSnapshot(t testing.TB, document *Document) snapshot.Snapshot {
	t.Helper()
	saved, err := document.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	return saved
}
