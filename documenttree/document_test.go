package documenttree

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
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
	comments, delta, err := activity.InsertMap(0)
	if err != nil {
		t.Fatal(err)
	}
	changes = append(changes, delta)
	delta, err = comments.Set("body", []byte("fully replicated"))
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
	resultActivity, ok := resultRoot.Array("activity")
	if !ok || resultActivity.Len() != 1 {
		t.Fatalf("activity = %#v, len=%d", resultActivity, resultActivity.Len())
	}
	resultComments, ok := resultActivity.Map(0)
	if !ok {
		t.Fatal("fully nested comments map is missing")
	}
	if value, found := resultComments.Get("body"); !found || string(value.Bytes) != "fully replicated" {
		t.Fatalf("fully nested comments = %#v, %t", value, found)
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

func TestDocumentTreeOutputBudgetRejectsAtomicallyAndReusesLocalFrame(t *testing.T) {
	limits := frame.DefaultLimits()
	limits.MaxTags = 4
	document, err := NewWithOptionsAndOutputLimits("writer", DefaultOptions(), limits)
	if err != nil {
		t.Fatal(err)
	}
	root, rootDelta, err := document.CreateRootMap("workspace")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := document.MarshalLocalDelta(rootDelta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalDeltaWithOptions(encoded, DefaultOptions(), limits); err != nil {
		t.Fatalf("local frame decode = %v", err)
	}
	canonical, err := document.MarshalDeltaWithLimits(rootDelta, limits)
	if err != nil || !bytes.Equal(encoded, canonical) {
		t.Fatalf("local frame = %x, canonical = %x, err = %v", encoded, canonical, err)
	}
	encoded[0] ^= 0xff
	owned, err := document.MarshalLocalDelta(rootDelta)
	if err != nil || !bytes.Equal(owned, canonical) {
		t.Fatalf("local frame ownership = %x, want %x, err = %v", owned, canonical, err)
	}

	stateBefore, err := document.MarshalBinaryWithLimits(limits)
	if err != nil {
		t.Fatal(err)
	}
	clockBefore := document.ClockState()
	if _, _, err := root.CreateMap("nested"); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("nested output budget = %v", err)
	}
	stateAfter, err := document.MarshalBinaryWithLimits(limits)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stateAfter, stateBefore) || document.ClockState() != clockBefore {
		t.Fatalf("rejected local delta changed document\n got=%x %#v\nwant=%x %#v", stateAfter, document.ClockState(), stateBefore, clockBefore)
	}
	if _, ok := root.Map("nested"); ok {
		t.Fatal("rejected nested map is visible")
	}

	invalid := frame.DefaultLimits()
	invalid.MaxFrameBytes = 1
	if _, err := NewFromClockWithOptionsAndOutputLimits(clock.State{ReplicaID: "writer"}, DefaultOptions(), invalid); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("invalid output budget = %v", err)
	}

	unbounded := mustDocument(t, "unbounded")
	_, unboundedDelta, err := unbounded.CreateRootMap("workspace")
	if err != nil {
		t.Fatal(err)
	}
	wire, err := unbounded.MarshalLocalDelta(unboundedDelta)
	if err != nil {
		t.Fatal(err)
	}
	want, err := unboundedDelta.MarshalBinary()
	if err != nil || !bytes.Equal(wire, want) {
		t.Fatalf("unbounded local frame = %x, want %x, err = %v", wire, want, err)
	}

	var nilDocument *Document
	if _, err := nilDocument.MarshalDeltaWithLimits(Delta{}, limits); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil delta marshal = %v", err)
	}
	if _, err := nilDocument.MarshalLocalDelta(Delta{}); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil local marshal = %v", err)
	}

	bare := &Document{options: DefaultOptions()}
	if _, _, err := bare.CreateRootMap("workspace"); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("missing clock root = %v", err)
	}
	if err := bare.ApplyDelta(Delta{}); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("missing clock apply = %v", err)
	}
	if _, _, err := bare.MarshalBinaryWithClockStateAndLimits(limits); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("missing clock checkpoint = %v", err)
	}
	if _, err := bare.SnapshotCurrentStateWithLimits(limits); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("missing clock snapshot = %v", err)
	}
	if err := bare.applyLocalLocked(nil, nil); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("nil local delta = %v", err)
	}
	if err := bare.applyLocalLocked(&Delta{}, nil); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil local clock = %v", err)
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
