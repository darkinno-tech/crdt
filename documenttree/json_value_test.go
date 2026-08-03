package documenttree

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestDocumentTreeJSONScalarsConvergeWithNestedObjects(t *testing.T) {
	alice := mustDocument(t, "alice")
	bob := mustDocument(t, "bob")
	carol := mustDocument(t, "carol")

	root, rootDelta, err := alice.CreateRootMap("dashboard")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []*Document{bob, carol} {
		if err := target.ApplyDelta(rootDelta); err != nil {
			t.Fatal(err)
		}
	}
	bobRoot, ok := bob.RootMap("dashboard")
	if !ok {
		t.Fatal("bob root is missing")
	}
	carolRoot, ok := carol.RootMap("dashboard")
	if !ok {
		t.Fatal("carol root is missing")
	}

	changes := make([]Delta, 0, 8)
	for _, mutation := range []func() (Delta, error){
		func() (Delta, error) { return root.SetJSON("title", "Safety review") },
		func() (Delta, error) { return root.SetJSON("open", true) },
		func() (Delta, error) { return bobRoot.SetJSON("count", json.Number("9007199254740993")) },
		func() (Delta, error) { return carolRoot.SetJSON("owner", nil) },
	} {
		delta, err := mutation()
		if err != nil {
			t.Fatal(err)
		}
		changes = append(changes, delta)
	}
	sections, delta, err := root.CreateArray("sections")
	if err != nil {
		t.Fatal(err)
	}
	changes = append(changes, delta)
	delta, err = sections.InsertJSON(0, "protocol")
	if err != nil {
		t.Fatal(err)
	}
	changes = append(changes, delta)
	section, delta, err := sections.InsertMap(1)
	if err != nil {
		t.Fatal(err)
	}
	changes = append(changes, delta)
	delta, err = section.SetJSON("name", "validation")
	if err != nil {
		t.Fatal(err)
	}
	changes = append(changes, delta)

	for index, target := range []*Document{alice, bob, carol} {
		deliverDocumentTreeDeltas(t, target, changes, int64(20260803+index))
	}

	var expected []byte
	for index, target := range []*Document{alice, bob, carol} {
		state, err := target.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			expected = state
		} else if !bytes.Equal(state, expected) {
			t.Fatalf("replica %d diverged", index)
		}
	}
	result, ok := carol.RootMap("dashboard")
	if !ok {
		t.Fatal("converged root missing")
	}
	if value, found, err := result.GetJSON("count"); err != nil || !found || value != json.Number("9007199254740993") {
		t.Fatalf("count = %#v, %t, %v", value, found, err)
	}
	resultSections, ok := result.Array("sections")
	if !ok {
		t.Fatal("sections missing")
	}
	if value, found, err := resultSections.GetJSON(0); err != nil || !found || value != "protocol" {
		t.Fatalf("sections[0] = %#v, %t, %v", value, found, err)
	}
	resultSection, ok := resultSections.Map(1)
	if !ok {
		t.Fatal("structured section missing")
	}
	if value, found, err := resultSection.GetJSON("name"); err != nil || !found || value != "validation" {
		t.Fatalf("section name = %#v, %t, %v", value, found, err)
	}
	if _, found, err := resultSections.GetJSON(-1); found || err != nil {
		t.Fatalf("negative array JSON lookup = found=%t, err=%v", found, err)
	}
	if _, found, err := result.GetJSON("missing"); found || err != nil {
		t.Fatalf("missing map JSON lookup = found=%t, err=%v", found, err)
	}
}

func TestDocumentTreeJSONScalarBoundaryIsCanonicalAndAtomic(t *testing.T) {
	document := mustDocument(t, "writer")
	root, _, err := document.CreateRootMap("workspace")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.SetJSON("number", json.RawMessage(" 9007199254740993 ")); err != nil {
		t.Fatal(err)
	}
	value, found := root.Get("number")
	if !found || string(value.Bytes) != "9007199254740993" {
		t.Fatalf("canonical JSON bytes = %q, found=%t", value.Bytes, found)
	}

	stateBefore, err := document.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	clockBefore := document.ClockState()
	for _, invalid := range []any{
		json.RawMessage("{\"nested\":true}"),
		json.RawMessage("[1,2,3]"),
		json.RawMessage("not-json"),
	} {
		if _, err := root.SetJSON("rejected", invalid); !errors.Is(err, ErrInvalidJSON) {
			t.Fatalf("SetJSON(%q) error = %v, want %v", invalid, err, ErrInvalidJSON)
		}
		stateAfter, err := document.MarshalBinary()
		if err != nil || !bytes.Equal(stateAfter, stateBefore) || document.ClockState() != clockBefore {
			t.Fatalf("rejected JSON mutated state: %v", err)
		}
	}
	if _, err := root.Set("raw", []byte(" true ")); err != nil {
		t.Fatal(err)
	}
	if value, found, err := root.GetJSON("raw"); !found || value != nil || !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("GetJSON(raw) = %#v, %t, %v", value, found, err)
	}
	child, _, err := root.CreateMap("child")
	if err != nil || child == nil {
		t.Fatalf("CreateMap(child) = %#v, %v", child, err)
	}
	if value, found, err := root.GetJSON("child"); !found || value != nil || !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("GetJSON(child) = %#v, %t, %v", value, found, err)
	}
	array, _, err := document.CreateRootArray("items")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := array.InsertJSON(0, json.RawMessage("[1]")); !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("InsertJSON(object) = %v, want %v", err, ErrInvalidJSON)
	}
	if _, _, err := array.InsertMap(0); err != nil {
		t.Fatal(err)
	}
	if value, found, err := array.GetJSON(0); !found || value != nil || !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("GetJSON(object) = %#v, %t, %v", value, found, err)
	}
}

func TestDocumentTreeJSONScalarParserRejectsNonCanonicalAndBoundedInput(t *testing.T) {
	options := DefaultOptions()
	if value, err := unmarshalJSONScalar([]byte("true"), options); err != nil || value != true {
		t.Fatalf("unmarshal canonical scalar = %#v, %v", value, err)
	}
	for _, encoded := range [][]byte{
		[]byte("true false"),
		[]byte("[1]"),
		[]byte("\"unterminated"),
		[]byte("}"),
	} {
		if _, err := unmarshalJSONScalar(encoded, options); !errors.Is(err, ErrInvalidJSON) {
			t.Fatalf("unmarshal %q = %v, want %v", encoded, err, ErrInvalidJSON)
		}
	}
	if _, err := marshalJSONScalar(math.Inf(1), options); !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("marshal infinity = %v, want %v", err, ErrInvalidJSON)
	}
	tight := options
	tight.MaxValueBytes = 3
	if _, err := marshalJSONScalar("long", tight); !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("marshal oversized scalar = %v, want %v", err, ErrInvalidJSON)
	}
	if validJSONShape([]byte("[]"), 8, 0) || validJSONShape([]byte("[[[]]]"), 8, 2) {
		t.Fatal("JSON shape accepted a zero-depth or over-depth container")
	}
}
