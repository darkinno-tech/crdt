package xml

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"
)

func TestFragmentConvergesWithDuplicateOutOfOrderDeltas(t *testing.T) {
	left := mustFragment(t, "left")
	right := mustFragment(t, "right")
	first := Node{Kind: ElementNode, Name: "item", Attributes: []Attribute{{Name: "id", Value: "1"}}, Children: []Node{{Kind: TextNode, Text: "one"}}}
	base, err := left.Insert(0, []Node{first})
	if err != nil {
		t.Fatal(err)
	}
	if err := right.ApplyDelta(base); err != nil {
		t.Fatal(err)
	}
	leftEdit, err := left.Append([]Node{{Kind: ElementNode, Name: "left"}})
	if err != nil {
		t.Fatal(err)
	}
	rightEdit, err := right.Append([]Node{{Kind: ElementNode, Name: "right"}})
	if err != nil {
		t.Fatal(err)
	}
	changes := []Delta{base, leftEdit, rightEdit}
	for replica, seed := range map[*Fragment]int64{left: 1, right: 2} {
		frames := make([][]byte, 0, len(changes)*2)
		for _, change := range changes {
			encoded, err := MarshalDelta(change)
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
			if err := replica.ApplyDelta(change); err != nil {
				t.Fatal(err)
			}
		}
	}
	leftXML, err := left.RenderFragment()
	if err != nil {
		t.Fatal(err)
	}
	rightXML, err := right.RenderFragment()
	if err != nil {
		t.Fatal(err)
	}
	if leftXML != rightXML || leftXML == "" {
		t.Fatalf("fragment convergence left=%q right=%q", leftXML, rightXML)
	}
}

func TestParseAndRenderCanonicalDocument(t *testing.T) {
	node, err := ParseDocument([]byte(`<?xml version="1.0" encoding="UTF-8"?><root z="last" a="first">hello<child enabled="yes"/></root>`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := RenderDocument(node)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := encoded, `<root a="first" z="last">hello<child enabled="yes"></child></root>`; got != want {
		t.Fatalf("RenderDocument() = %q, want %q", got, want)
	}
	fragment := mustFragment(t, "writer")
	if _, err := fragment.Append([]Node{node}); err != nil {
		t.Fatal(err)
	}
	saved, err := fragment.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	got, err := recovered.RenderFragment()
	if err != nil || got != encoded {
		t.Fatalf("recovered fragment = %q, %v", got, err)
	}
}

func TestParseDocumentRejectsUnsafeOrUnsupportedXML(t *testing.T) {
	for _, input := range []string{
		`<!DOCTYPE root><root/>`,
		`<root><!-- comment --></root>`,
		`<root xmlns="urn:example"/>`,
		`<root><child></root>`,
		`<root/><second/>`,
	} {
		if _, err := ParseDocument([]byte(input)); !errors.Is(err, ErrInvalidDocument) {
			t.Fatalf("ParseDocument(%q) = %v", input, err)
		}
	}
}

func TestNodeCodecRejectsNonCanonicalOrInvalidNode(t *testing.T) {
	codec := nodeCodec{}
	invalid := Node{Kind: ElementNode, Name: "root", Attributes: []Attribute{{Name: "z", Value: "1"}, {Name: "a", Value: "2"}}}
	if _, err := codec.Marshal(invalid); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("Marshal(noncanonical attributes) = %v", err)
	}
	valid := Node{Kind: ElementNode, Name: "root", Attributes: []Attribute{{Name: "a", Value: "1"}}}
	encoded, err := codec.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	malformed := append(append([]byte(nil), encoded...), 0)
	if _, err := codec.Unmarshal(malformed); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("Unmarshal(trailing) = %v", err)
	}
	if decoded, err := codec.Unmarshal(encoded); err != nil || !bytes.Equal(encoded, mustEncodeNode(t, decoded)) {
		t.Fatalf("canonical round trip = %#v, %v", decoded, err)
	}
}

func mustFragment(t testing.TB, replicaID string) *Fragment {
	t.Helper()
	fragment, err := New(replicaID)
	if err != nil {
		t.Fatal(err)
	}
	return fragment
}

func mustEncodeNode(t testing.TB, node Node) []byte {
	t.Helper()
	encoded, err := (nodeCodec{}).Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func FuzzParseDocument(f *testing.F) {
	f.Add([]byte(`<root><item id="1">seed</item></root>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		node, err := ParseDocument(data)
		if err != nil {
			return
		}
		encoded, err := RenderDocument(node)
		if err != nil {
			t.Fatalf("accepted node did not render: %v", err)
		}
		if _, err := ParseDocument([]byte(encoded)); err != nil {
			t.Fatalf("rendered document did not parse: %v", err)
		}
	})
}
