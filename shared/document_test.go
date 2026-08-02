package shared

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/DarkInno/crdt/clock"
	"github.com/DarkInno/crdt/documenttree"
	frame "github.com/DarkInno/crdt/encoding"
)

func TestDocumentConvergesThroughNamedMapAndArrayUpdates(t *testing.T) {
	options := testOptions()
	alice := mustNewDocument(t, "alice", options)
	bob := mustNewDocument(t, "bob", options)
	carol := mustNewDocument(t, "carol", options)

	updates := make([][]byte, 0, 16)
	for _, document := range []*Document{alice, bob, carol} {
		document := document
		if _, err := document.OnUpdate(func(update []byte) {
			updates = append(updates, update)
		}); err != nil {
			t.Fatal(err)
		}
	}

	workspace, err := alice.Map("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.SetString("title", "Q3 roadmap"); err != nil {
		t.Fatal(err)
	}
	cards, err := workspace.CreateArray("cards")
	if err != nil {
		t.Fatal(err)
	}
	card, err := cards.InsertMap(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := card.SetJSON("card", map[string]any{"id": "card-1", "done": false}); err != nil {
		t.Fatal(err)
	}

	for _, target := range []*Document{bob, carol} {
		for _, update := range updates {
			if err := target.ApplyUpdate(update); err != nil {
				t.Fatalf("bootstrap update: %v", err)
			}
		}
	}
	updates = updates[:0]

	bobWorkspace, err := bob.Map("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := bobWorkspace.SetJSON("owner", map[string]string{"id": "bob"}); err != nil {
		t.Fatal(err)
	}
	carolWorkspace, err := carol.Map("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := carolWorkspace.SetString("status", "review"); err != nil {
		t.Fatal(err)
	}

	shuffled := append([][]byte(nil), updates...)
	random := rand.New(rand.NewSource(20260801))
	random.Shuffle(len(shuffled), func(left, right int) { shuffled[left], shuffled[right] = shuffled[right], shuffled[left] })
	for _, target := range []*Document{alice, bob, carol} {
		for index := len(shuffled) - 1; index >= 0; index-- {
			if err := target.ApplyUpdate(shuffled[index]); err != nil {
				t.Fatalf("replica update %d: %v", index, err)
			}
			if index%2 == 0 {
				if err := target.ApplyUpdate(shuffled[index]); err != nil {
					t.Fatalf("duplicate replica update %d: %v", index, err)
				}
			}
		}
	}

	var expected []byte
	for index, document := range []*Document{alice, bob, carol} {
		checkpoint, err := document.Checkpoint()
		if err != nil {
			t.Fatalf("checkpoint replica %d: %v", index, err)
		}
		if index == 0 {
			expected = checkpoint.State
		} else if !bytes.Equal(checkpoint.State, expected) {
			t.Fatalf("replica %d diverged\n got: %x\nwant: %x", index, checkpoint.State, expected)
		}
	}

	result, err := bob.Map("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := result.String("title"); !ok || got != "Q3 roadmap" {
		t.Fatalf("title = %q, %t", got, ok)
	}
	if got, ok := result.String("status"); !ok || got != "review" {
		t.Fatalf("status = %q, %t", got, ok)
	}
	var owner struct {
		ID string `json:"id"`
	}
	if found, err := result.JSON("owner", &owner); err != nil || !found || owner.ID != "bob" {
		t.Fatalf("owner = %#v, found=%t, err=%v", owner, found, err)
	}
	resultCards, ok := result.Array("cards")
	if !ok || resultCards.Len() != 1 {
		t.Fatalf("cards = %#v, len=%d", resultCards, resultCards.Len())
	}
	resultCard, ok := resultCards.Map(0)
	if !ok {
		t.Fatal("card is missing")
	}
	var payload struct {
		ID   string `json:"id"`
		Done bool   `json:"done"`
	}
	if found, err := resultCard.JSON("card", &payload); err != nil || !found || payload.ID != "card-1" || payload.Done {
		t.Fatalf("card = %#v, found=%t, err=%v", payload, found, err)
	}
}

func TestDocumentRejectsMalformedUpdateAtomically(t *testing.T) {
	document := mustNewDocument(t, "receiver", testOptions())
	workspace, err := document.Map("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.SetString("title", "safe"); err != nil {
		t.Fatal(err)
	}
	before, err := document.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := document.ApplyUpdate([]byte("not-a-frame")); err == nil {
		t.Fatal("malformed update was accepted")
	}
	after, err := document.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.State, after.State) || before.ClockState != after.ClockState {
		t.Fatalf("rejected update changed checkpoint\n got: %#v\nwant: %#v", after, before)
	}
}

func TestDocumentUpdateHandlersReceiveIndependentOwnedFrames(t *testing.T) {
	document := mustNewDocument(t, "writer", testOptions())
	var first, second []byte
	if _, err := document.OnUpdate(func(update []byte) {
		first = update
		for index := range update {
			update[index] = 0
		}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := document.OnUpdate(func(update []byte) { second = update }); err != nil {
		t.Fatal(err)
	}
	_, err := document.Map("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 || len(second) == 0 || bytes.Equal(first, second) || !bytes.Equal(first, make([]byte, len(first))) {
		t.Fatalf("independent handler bytes first=%x second=%x", first, second)
	}
	if err := document.ApplyUpdate(second); err != nil {
		t.Fatalf("second handler received an invalid frame: %v", err)
	}
}

func TestDocumentCheckpointRestoresClockAndState(t *testing.T) {
	document := mustNewDocument(t, "writer", testOptions())
	workspace, err := document.Map("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.SetString("title", "before restart"); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := document.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Restore(checkpoint, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	restoredWorkspace, err := restored.Map("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := restoredWorkspace.SetString("title", "after restart"); err != nil {
		t.Fatal(err)
	}
	if got, ok := restoredWorkspace.String("title"); !ok || got != "after restart" {
		t.Fatalf("restored title = %q, %t", got, ok)
	}
}

func TestDocumentErrorsAndOptions(t *testing.T) {
	if _, err := NewWithOptions("writer", Options{DocumentOptions: documenttree.DefaultOptions(), FrameLimits: frame.DecoderLimits{MaxFrameBytes: 1}}); err == nil {
		t.Fatal("invalid frame limits were accepted")
	}
	var nilDocument *Document
	if _, err := nilDocument.Map("workspace"); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil document map = %v", err)
	}
	if _, err := nilDocument.OnUpdate(func([]byte) {}); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil document handler = %v", err)
	}
	document := mustNewDocument(t, "writer", testOptions())
	if _, err := document.OnUpdate(nil); !errors.Is(err, ErrNilUpdateHandler) {
		t.Fatalf("nil handler = %v", err)
	}
	var nilMap *Map
	if err := nilMap.Set("key", nil); !errors.Is(err, ErrNilMap) {
		t.Fatalf("nil map set = %v", err)
	}
	var nilArray *Array
	if err := nilArray.Insert(0, nil); !errors.Is(err, ErrNilArray) {
		t.Fatalf("nil array insert = %v", err)
	}
	if profile := Profile(); profile.ID != "document/tree-v2" || profile.RequiresCodecID {
		t.Fatalf("profile = %#v", profile)
	}
}

func TestDocumentConvenienceMethodsAndErrorPaths(t *testing.T) {
	options := DefaultOptions()
	options.FrameLimits = testOptions().FrameLimits
	document, err := New("writer")
	if err != nil {
		t.Fatal(err)
	}
	if document.Options().FrameLimits.MaxFrameBytes != frame.DefaultLimits().MaxFrameBytes {
		t.Fatalf("default frame limits = %#v", document.Options().FrameLimits)
	}
	limited, err := NewWithLimits("limited", options.FrameLimits)
	if err != nil {
		t.Fatal(err)
	}
	if got := limited.Options().FrameLimits; got != options.FrameLimits {
		t.Fatalf("limited options = %#v, want %#v", got, options.FrameLimits)
	}
	document = mustNewDocument(t, "writer", options)

	var updates [][]byte
	stop, err := document.OnUpdate(func(update []byte) { updates = append(updates, update) })
	if err != nil {
		t.Fatal(err)
	}
	root, err := document.Map("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if state := document.State(); state.Type != "document-tree" || state.ReplicaID != "writer" {
		t.Fatalf("state = %#v", state)
	}
	if _, err := document.Map("workspace"); err != nil {
		t.Fatal(err)
	}
	if err := root.Set("raw", []byte("value")); err != nil {
		t.Fatal(err)
	}
	if got, ok := root.Get("raw"); !ok || string(got) != "value" {
		t.Fatalf("raw = %q, %t", got, ok)
	}
	if got, ok := root.Get("missing"); ok || got != nil {
		t.Fatalf("missing raw = %q, %t", got, ok)
	}
	if err := root.Set("not-utf8", []byte{0xff}); err != nil {
		t.Fatal(err)
	}
	if got, ok := root.String("not-utf8"); ok || got != "" {
		t.Fatalf("invalid UTF-8 string = %q, %t", got, ok)
	}
	if err := root.SetJSON("metadata", map[string]int{"priority": 3}); err != nil {
		t.Fatal(err)
	}
	var metadata map[string]int
	if found, err := root.JSON("metadata", &metadata); err != nil || !found || metadata["priority"] != 3 {
		t.Fatalf("metadata = %#v, found=%t, err=%v", metadata, found, err)
	}
	if found, err := root.JSON("missing", &metadata); found || err != nil {
		t.Fatalf("missing JSON = found=%t, err=%v", found, err)
	}
	if err := root.SetString("bad-json", "{"); err != nil {
		t.Fatal(err)
	}
	if found, err := root.JSON("bad-json", &metadata); !found || err == nil {
		t.Fatalf("bad JSON = found=%t, err=%v", found, err)
	}
	if err := root.SetJSON("unsupported", func() {}); err == nil {
		t.Fatal("unsupported JSON was accepted")
	}

	nestedMap, err := root.CreateMap("nested-map")
	if err != nil {
		t.Fatal(err)
	}
	if err := nestedMap.SetString("child", "yes"); err != nil {
		t.Fatal(err)
	}
	if got, ok := root.Map("nested-map"); !ok || got == nil {
		t.Fatal("nested map is missing")
	}
	if found, err := root.JSON("nested-map", &metadata); found || !errors.Is(err, ErrValueKind) {
		t.Fatalf("nested map JSON = found=%t, err=%v", found, err)
	}
	nestedArray, err := root.CreateArray("nested-array")
	if err != nil {
		t.Fatal(err)
	}
	if err := nestedArray.InsertString(0, "nested"); err != nil {
		t.Fatal(err)
	}
	if got, ok := root.Array("nested-array"); !ok || got == nil {
		t.Fatal("nested array is missing")
	}
	if err := root.Delete("raw"); err != nil {
		t.Fatal(err)
	}
	if got, ok := root.Get("raw"); ok || got != nil {
		t.Fatalf("deleted raw = %q, %t", got, ok)
	}
	if keys := root.Keys(); len(keys) == 0 || keys[0] == "" {
		t.Fatalf("keys = %#v", keys)
	}

	array, err := document.Array("items")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := document.Array("items"); err != nil {
		t.Fatal(err)
	}
	if err := array.InsertString(0, "first"); err != nil {
		t.Fatal(err)
	}
	if err := array.InsertJSON(1, map[string]bool{"done": false}); err != nil {
		t.Fatal(err)
	}
	if got, ok := array.String(0); !ok || got != "first" {
		t.Fatalf("array string = %q, %t", got, ok)
	}
	if got, ok := array.Get(99); ok || got != nil {
		t.Fatalf("out-of-range get = %q, %t", got, ok)
	}
	if got, ok := array.String(99); ok || got != "" {
		t.Fatalf("out-of-range string = %q, %t", got, ok)
	}
	var arrayJSON map[string]bool
	if found, err := array.JSON(1, &arrayJSON); err != nil || !found || arrayJSON["done"] {
		t.Fatalf("array JSON = %#v, found=%t, err=%v", arrayJSON, found, err)
	}
	if found, err := array.JSON(99, &arrayJSON); found || err != nil {
		t.Fatalf("missing array JSON = found=%t, err=%v", found, err)
	}
	childMap, err := array.InsertMap(array.Len())
	if err != nil {
		t.Fatal(err)
	}
	if err := childMap.SetString("id", "child"); err != nil {
		t.Fatal(err)
	}
	if got, ok := array.Map(2); !ok || got == nil {
		t.Fatal("array child map is missing")
	}
	if found, err := array.JSON(2, &arrayJSON); found || !errors.Is(err, ErrValueKind) {
		t.Fatalf("array child JSON = found=%t, err=%v", found, err)
	}
	childArray, err := array.InsertArray(array.Len())
	if err != nil {
		t.Fatal(err)
	}
	if err := childArray.InsertString(0, "child-array"); err != nil {
		t.Fatal(err)
	}
	if got, ok := array.Array(3); !ok || got == nil {
		t.Fatal("array child array is missing")
	}
	if err := array.Delete(0, 1); err != nil {
		t.Fatal(err)
	}
	if err := array.Insert(-1, []byte("bad")); !errors.Is(err, documenttree.ErrRange) {
		t.Fatalf("negative insert = %v", err)
	}
	if err := array.Delete(99, 1); !errors.Is(err, documenttree.ErrRange) {
		t.Fatalf("out-of-range delete = %v", err)
	}
	if err := array.InsertJSON(0, func() {}); err == nil {
		t.Fatal("unsupported array JSON was accepted")
	}

	beforeEmptyEmit := len(updates)
	if err := document.emit(documenttree.Delta{}); err != nil {
		t.Fatal(err)
	}
	if len(updates) != beforeEmptyEmit {
		t.Fatal("empty delta emitted an update")
	}
	stop()
	stop()
	beforeStop := len(updates)
	if err := root.SetString("after-stop", "ignored"); err != nil {
		t.Fatal(err)
	}
	if len(updates) != beforeStop {
		t.Fatal("unsubscribed handler received an update")
	}

	if _, err := New(" "); err == nil {
		t.Fatal("invalid replica was accepted")
	}
	if _, err := NewWithOptions("writer", Options{DocumentOptions: documenttree.Options{}, FrameLimits: options.FrameLimits}); !errors.Is(err, documenttree.ErrResourceLimit) {
		t.Fatalf("invalid document options = %v", err)
	}
	if _, err := Restore(Checkpoint{State: []byte("bad"), ClockState: clock.State{ReplicaID: "writer"}}, options); err == nil {
		t.Fatal("invalid checkpoint was accepted")
	}
}

func TestDocumentNilAndBoundaryHandles(t *testing.T) {
	var nilDocument *Document
	if got := nilDocument.Options(); got != (Options{}) {
		t.Fatalf("nil options = %#v", got)
	}
	if _, err := nilDocument.Array("items"); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil array root = %v", err)
	}
	if err := nilDocument.ApplyUpdate(nil); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil apply = %v", err)
	}
	if _, err := nilDocument.Checkpoint(); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil checkpoint = %v", err)
	}
	if got := nilDocument.State(); got.Type != "shared-document" {
		t.Fatalf("nil state = %#v", got)
	}

	var nilMap *Map
	if err := nilMap.Delete("key"); !errors.Is(err, ErrNilMap) {
		t.Fatalf("nil map delete = %v", err)
	}
	if _, err := nilMap.CreateMap("key"); !errors.Is(err, ErrNilMap) {
		t.Fatalf("nil map create map = %v", err)
	}
	if _, err := nilMap.CreateArray("key"); !errors.Is(err, ErrNilMap) {
		t.Fatalf("nil map create array = %v", err)
	}
	if err := nilMap.SetString("key", "value"); !errors.Is(err, ErrNilMap) {
		t.Fatalf("nil map set string = %v", err)
	}
	if err := nilMap.SetJSON("key", map[string]bool{"ok": true}); !errors.Is(err, ErrNilMap) {
		t.Fatalf("nil map set JSON = %v", err)
	}
	if got, ok := nilMap.Get("key"); got != nil || ok {
		t.Fatalf("nil map get = %q, %t", got, ok)
	}
	if got, ok := nilMap.String("key"); got != "" || ok {
		t.Fatalf("nil map string = %q, %t", got, ok)
	}
	if found, err := nilMap.JSON("key", &struct{}{}); found || !errors.Is(err, ErrNilMap) {
		t.Fatalf("nil map JSON = found=%t, err=%v", found, err)
	}
	if got, ok := nilMap.Map("key"); got != nil || ok {
		t.Fatalf("nil map child map = %#v, %t", got, ok)
	}
	if got, ok := nilMap.Array("key"); got != nil || ok {
		t.Fatalf("nil map child array = %#v, %t", got, ok)
	}
	if keys := nilMap.Keys(); keys != nil {
		t.Fatalf("nil map keys = %#v", keys)
	}

	var nilArray *Array
	if err := nilArray.InsertString(0, "value"); !errors.Is(err, ErrNilArray) {
		t.Fatalf("nil array insert string = %v", err)
	}
	if err := nilArray.InsertJSON(0, map[string]bool{"ok": true}); !errors.Is(err, ErrNilArray) {
		t.Fatalf("nil array insert JSON = %v", err)
	}
	if err := nilArray.Delete(0, 1); !errors.Is(err, ErrNilArray) {
		t.Fatalf("nil array delete = %v", err)
	}
	if _, err := nilArray.InsertMap(0); !errors.Is(err, ErrNilArray) {
		t.Fatalf("nil array insert map = %v", err)
	}
	if _, err := nilArray.InsertArray(0); !errors.Is(err, ErrNilArray) {
		t.Fatalf("nil array insert array = %v", err)
	}
	if got := nilArray.Len(); got != 0 {
		t.Fatalf("nil array len = %d", got)
	}
	if got, ok := nilArray.Get(0); got != nil || ok {
		t.Fatalf("nil array get = %q, %t", got, ok)
	}
	if got, ok := nilArray.String(0); got != "" || ok {
		t.Fatalf("nil array string = %q, %t", got, ok)
	}
	if found, err := nilArray.JSON(0, &struct{}{}); found || !errors.Is(err, ErrNilArray) {
		t.Fatalf("nil array JSON = found=%t, err=%v", found, err)
	}
	if got, ok := nilArray.Map(0); got != nil || ok {
		t.Fatalf("nil array child map = %#v, %t", got, ok)
	}
	if got, ok := nilArray.Array(0); got != nil || ok {
		t.Fatalf("nil array child array = %#v, %t", got, ok)
	}

	document := mustNewDocument(t, "writer", testOptions())
	if _, err := document.Map(""); !errors.Is(err, documenttree.ErrInvalidRoot) {
		t.Fatalf("empty root map = %v", err)
	}
	if _, err := document.Array(""); !errors.Is(err, documenttree.ErrInvalidRoot) {
		t.Fatalf("empty root array = %v", err)
	}
	root, err := document.Map("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Set("", []byte("bad")); !errors.Is(err, documenttree.ErrInvalidKey) {
		t.Fatalf("empty map key = %v", err)
	}
	if err := root.Delete(""); !errors.Is(err, documenttree.ErrInvalidKey) {
		t.Fatalf("empty map delete key = %v", err)
	}
	if _, err := root.CreateMap(""); !errors.Is(err, documenttree.ErrInvalidKey) {
		t.Fatalf("empty child map key = %v", err)
	}
	if _, err := root.CreateArray(""); !errors.Is(err, documenttree.ErrInvalidKey) {
		t.Fatalf("empty child array key = %v", err)
	}
	if got, ok := root.Map("missing"); got == nil || ok {
		t.Fatalf("missing child map = %#v, %t", got, ok)
	}
	if got, ok := root.Array("missing"); got == nil || ok {
		t.Fatalf("missing child array = %#v, %t", got, ok)
	}

	array, err := document.Array("items")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := array.InsertMap(-1); !errors.Is(err, documenttree.ErrRange) {
		t.Fatalf("negative insert map = %v", err)
	}
	if _, err := array.InsertArray(-1); !errors.Is(err, documenttree.ErrRange) {
		t.Fatalf("negative insert array = %v", err)
	}
	if got, ok := array.Map(0); got == nil || ok {
		t.Fatalf("missing array map = %#v, %t", got, ok)
	}
	if got, ok := array.Array(0); got == nil || ok {
		t.Fatalf("missing array array = %#v, %t", got, ok)
	}

	if _, err := Restore(Checkpoint{}, Options{}); err == nil {
		t.Fatal("restore accepted invalid frame options")
	}
	if _, err := Restore(Checkpoint{}, Options{DocumentOptions: testOptions().DocumentOptions, FrameLimits: testOptions().FrameLimits}); err == nil {
		t.Fatal("restore accepted missing clock state")
	}

	tooSmall := testOptions()
	tooSmall.FrameLimits.MaxTags = 1
	limited, err := NewWithOptions("limited", tooSmall)
	if err != nil {
		t.Fatal(err)
	}
	before, err := limited.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limited.Map("workspace"); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("root output budget = %v", err)
	}
	after, err := limited.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint after rejected root = %v", err)
	}
	if !bytes.Equal(before.State, after.State) || before.ClockState != after.ClockState || limited.State().ElementCount != 0 {
		t.Fatalf("rejected root changed document\n got=%#v\nwant=%#v", after, before)
	}

	childLimited := testOptions()
	childLimited.FrameLimits.MaxTags = 4
	child, err := NewWithOptions("child-limited", childLimited)
	if err != nil {
		t.Fatal(err)
	}
	root, err = child.Map("workspace")
	if err != nil {
		t.Fatal(err)
	}
	updates := 0
	stop, err := child.OnUpdate(func([]byte) { updates++ })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	before, err = child.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.CreateMap("nested"); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("child output budget = %v", err)
	}
	after, err = child.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint after rejected child = %v", err)
	}
	if !bytes.Equal(before.State, after.State) || before.ClockState != after.ClockState {
		t.Fatalf("rejected child changed document\n got=%#v\nwant=%#v", after, before)
	}
	if nested, ok := root.Map("nested"); nested == nil || ok {
		t.Fatalf("rejected child is visible: %#v, %t", nested, ok)
	}
	if updates != 0 {
		t.Fatalf("rejected child emitted %d updates", updates)
	}
}

func TestDocumentConcurrentLocalUpdates(t *testing.T) {
	document := mustNewDocument(t, "writer", testOptions())
	const workers = 16
	var updates [][]byte
	var updatesMu sync.Mutex
	if _, err := document.OnUpdate(func(update []byte) {
		updatesMu.Lock()
		updates = append(updates, update)
		updatesMu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}
	workspace, err := document.Map("workspace")
	if err != nil {
		t.Fatal(err)
	}
	receiver := mustNewDocument(t, "receiver", testOptions())
	updatesMu.Lock()
	rootUpdates := append([][]byte(nil), updates...)
	updates = updates[:0]
	updatesMu.Unlock()
	if len(rootUpdates) != 1 {
		t.Fatalf("root updates = %d, want 1", len(rootUpdates))
	}
	if err := receiver.ApplyUpdate(rootUpdates[0]); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			if err := workspace.SetString(fmt.Sprintf("key-%02d", worker), fmt.Sprintf("value-%02d", worker)); err != nil {
				t.Errorf("set %d: %v", worker, err)
			}
		}(worker)
	}
	group.Wait()
	updatesMu.Lock()
	deferred := append([][]byte(nil), updates...)
	updatesMu.Unlock()
	if len(deferred) != workers {
		t.Fatalf("updates = %d, want %d", len(deferred), workers)
	}
	for _, update := range deferred {
		if err := receiver.ApplyUpdate(update); err != nil {
			t.Fatal(err)
		}
	}
	remote, err := receiver.Map("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(remote.Keys()); got != workers {
		t.Fatalf("remote keys = %d, want %d", got, workers)
	}
}

func TestDocumentBoundedLoopbackHTTP(t *testing.T) {
	options := testOptions()
	options.FrameLimits.MaxFrameBytes = 8 << 10
	options.FrameLimits.MaxPayload = 7 << 10
	receiver := mustNewDocument(t, "receiver", options)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/updates" {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		body := http.MaxBytesReader(writer, request.Body, int64(options.FrameLimits.MaxFrameBytes))
		defer func() {
			if err := body.Close(); err != nil {
				return
			}
		}()
		update, err := io.ReadAll(body)
		if err != nil {
			http.Error(writer, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		if err := receiver.ApplyUpdate(update); err != nil {
			http.Error(writer, "invalid update", http.StatusBadRequest)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	source := mustNewDocument(t, "source", options)
	updates := make([][]byte, 0, 4)
	if _, err := source.OnUpdate(func(update []byte) { updates = append(updates, update) }); err != nil {
		t.Fatal(err)
	}
	board, err := source.Map("board")
	if err != nil {
		t.Fatal(err)
	}
	if err := board.SetString("title", "loopback"); err != nil {
		t.Fatal(err)
	}
	items, err := board.CreateArray("items")
	if err != nil {
		t.Fatal(err)
	}
	if err := items.InsertString(0, "one"); err != nil {
		t.Fatal(err)
	}
	for index := len(updates) - 1; index >= 0; index-- {
		response := postUpdate(t, server.URL, updates[index], "Bearer test-token")
		if response.StatusCode != http.StatusNoContent {
			closeHTTPBody(t, response.Body)
			t.Fatalf("update %d status = %d", index, response.StatusCode)
		}
		closeHTTPBody(t, response.Body)
	}
	received, err := receiver.Map("board")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := received.String("title"); !ok || got != "loopback" {
		t.Fatalf("received title = %q, %t", got, ok)
	}
	receivedItems, ok := received.Array("items")
	if !ok {
		t.Fatal("received items are missing")
	}
	if got, ok := receivedItems.String(0); !ok || got != "one" {
		t.Fatalf("received item = %q, %t", got, ok)
	}

	before, err := receiver.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := postUpdate(t, server.URL, updates[0], "")
	if unauthorized.StatusCode != http.StatusUnauthorized {
		closeHTTPBody(t, unauthorized.Body)
		t.Fatalf("unauthorized status = %d", unauthorized.StatusCode)
	}
	closeHTTPBody(t, unauthorized.Body)
	tooLarge := postUpdate(t, server.URL, bytes.Repeat([]byte{'x'}, options.FrameLimits.MaxFrameBytes+1), "Bearer test-token")
	if tooLarge.StatusCode != http.StatusRequestEntityTooLarge {
		closeHTTPBody(t, tooLarge.Body)
		t.Fatalf("oversized status = %d", tooLarge.StatusCode)
	}
	closeHTTPBody(t, tooLarge.Body)
	after, err := receiver.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.State, after.State) || before.ClockState != after.ClockState {
		t.Fatalf("rejected HTTP request changed receiver\n got=%#v\nwant=%#v", after, before)
	}
}

func postUpdate(tb testing.TB, endpoint string, update []byte, authorization string) *http.Response {
	tb.Helper()
	request, err := http.NewRequest(http.MethodPost, endpoint+"/updates", bytes.NewReader(update))
	if err != nil {
		tb.Fatal(err)
	}
	request.Header.Set("Authorization", authorization)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		tb.Fatal(err)
	}
	return response
}

func closeHTTPBody(tb testing.TB, body io.Closer) {
	tb.Helper()
	if err := body.Close(); err != nil {
		tb.Fatal(err)
	}
}

func BenchmarkDocumentMapSetAndApplyUpdate(b *testing.B) {
	options := testOptions()
	source := mustNewDocument(b, "source", options)
	target := mustNewDocument(b, "target", options)
	var update []byte
	if _, err := source.OnUpdate(func(value []byte) { update = value }); err != nil {
		b.Fatal(err)
	}
	workspace, err := source.Map("workspace")
	if err != nil {
		b.Fatal(err)
	}
	if err := target.ApplyUpdate(update); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := workspace.SetString("status", fmt.Sprintf("review-%d", index&1)); err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(update)))
		if err := target.ApplyUpdate(update); err != nil {
			b.Fatal(err)
		}
	}
}

func FuzzDocumentApplyUpdate(f *testing.F) {
	source := mustNewDocument(f, "seed", testOptions())
	var valid []byte
	if _, err := source.OnUpdate(func(update []byte) { valid = update }); err != nil {
		f.Fatal(err)
	}
	workspace, err := source.Map("workspace")
	if err != nil {
		f.Fatal(err)
	}
	if err := workspace.SetString("title", "seed"); err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Add([]byte("not-a-frame"))
	f.Add(bytes.Repeat([]byte{0xff}, 128))
	f.Fuzz(func(t *testing.T, update []byte) {
		document := mustNewDocument(t, "receiver", testOptions())
		root, err := document.Map("workspace")
		if err != nil {
			t.Fatal(err)
		}
		if err := root.SetString("safe", "value"); err != nil {
			t.Fatal(err)
		}
		before, err := document.Checkpoint()
		if err != nil {
			t.Fatal(err)
		}
		if err := document.ApplyUpdate(update); err != nil {
			after, checkpointErr := document.Checkpoint()
			if checkpointErr != nil || !bytes.Equal(before.State, after.State) || before.ClockState != after.ClockState {
				t.Fatalf("rejection changed state: checkpoint=%v\n got=%#v\nwant=%#v", checkpointErr, after, before)
			}
		}
	})
}

func testOptions() Options {
	return Options{
		DocumentOptions: documenttree.DefaultOptions(),
		FrameLimits: frame.DecoderLimits{
			MaxFrameBytes:  64 << 10,
			MaxPayload:     60 << 10,
			MaxCodecID:     64,
			MaxElements:    512,
			MaxTags:        512,
			MaxStringBytes: 1024,
		},
	}
}

func mustNewDocument(tb testing.TB, replicaID string, options Options) *Document {
	tb.Helper()
	document, err := NewWithOptions(replicaID, options)
	if err != nil {
		tb.Fatalf("NewWithOptions(%q): %v", replicaID, err)
	}
	return document
}
