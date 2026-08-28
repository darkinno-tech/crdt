package xml

import (
	"bytes"
	stdxml "encoding/xml"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/darkinno-tech/crdt/list"
)

func TestFragmentPublicLifecycleWireAndRecovery(t *testing.T) {
	invalidOptions := list.DefaultOptions()
	invalidOptions.MaxNodes = 0
	if _, err := NewWithOptions("writer", invalidOptions); !errors.Is(err, list.ErrResourceLimit) {
		t.Fatalf("NewWithOptions(invalid) = %v", err)
	}

	fragment := mustFragment(t, "writer")
	root := Node{
		Kind:       ElementNode,
		Name:       "root",
		Attributes: []Attribute{{Name: "id", Value: "1"}},
		Children:   []Node{{Kind: TextNode, Text: "content"}},
	}
	base, err := fragment.Append([]Node{root})
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := fragment.Insert(0, []Node{{Kind: TextNode, Text: "prefix"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := fragment.RenderFragment(); err != nil || got != "prefix<root id=\"1\">content</root>" {
		t.Fatalf("RenderFragment() = %q, %v", got, err)
	}
	nodes, err := fragment.Nodes()
	if err != nil || len(nodes) != 2 || !reflect.DeepEqual(nodes[1], root) {
		t.Fatalf("Nodes() = %#v, %v", nodes, err)
	}
	positions := fragment.Positions()
	if len(positions) != 2 || positions[0] == positions[1] {
		t.Fatalf("Positions() = %#v", positions)
	}
	if state := fragment.State(); state.Type != "xml-fragment" || state.ElementCount != 2 {
		t.Fatalf("State() = %#v", state)
	}

	deltaBytes, err := MarshalDelta(prefix)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalDelta(deltaBytes)
	if err != nil {
		t.Fatal(err)
	}
	target := mustFragment(t, "target")
	if err := target.ApplyDelta(base); err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
	if got, err := target.RenderFragment(); err != nil || got != "prefix<root id=\"1\">content</root>" {
		t.Fatalf("delta recovery = %q, %v", got, err)
	}

	other := mustFragment(t, "other")
	if _, err := other.Append([]Node{{Kind: ElementNode, Name: "other"}}); err != nil {
		t.Fatal(err)
	}
	if err := target.Merge(other); err != nil {
		t.Fatal(err)
	}
	if got := target.State().ElementCount; got != 3 {
		t.Fatalf("merged count = %d", got)
	}
	if _, err := target.Delete(2, 1); err != nil {
		t.Fatal(err)
	}
	deletedRendering, err := target.RenderFragment()
	if err != nil || target.State().ElementCount != 2 {
		t.Fatalf("delete result = %q, %v", deletedRendering, err)
	}

	stateBytes, err := target.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	restored := mustFragment(t, "restored")
	if err := restored.UnmarshalBinary(stateBytes); err != nil {
		t.Fatal(err)
	}
	if got, err := restored.RenderFragment(); err != nil || got != deletedRendering {
		t.Fatalf("wire recovery = %q, %v", got, err)
	}
	saved, err := restored.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	fromSnapshot, err := NewFromSnapshotWithOptions(saved, list.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := fromSnapshot.RenderFragment(); err != nil || got != deletedRendering {
		t.Fatalf("snapshot recovery = %q, %v", got, err)
	}
}

func TestFragmentRejectsInvalidPublicInputs(t *testing.T) {
	fragment := mustFragment(t, "writer")
	if _, err := fragment.Insert(-1, []Node{{Kind: TextNode, Text: "x"}}); !errors.Is(err, list.ErrRange) {
		t.Fatalf("negative insert = %v", err)
	}
	if _, err := fragment.Delete(0, 1); !errors.Is(err, list.ErrRange) {
		t.Fatalf("out-of-range delete = %v", err)
	}
	if err := fragment.ApplyDelta(Delta{}); !errors.Is(err, list.ErrInvalidDelta) {
		t.Fatalf("invalid delta = %v", err)
	}
	if err := fragment.UnmarshalBinary([]byte("not a frame")); err == nil {
		t.Fatal("UnmarshalBinary accepted malformed state")
	}
	if _, err := MarshalDelta(Delta{}); !errors.Is(err, list.ErrInvalidDelta) {
		t.Fatalf("MarshalDelta(invalid) = %v", err)
	}
	if _, err := UnmarshalDelta([]byte("not a frame")); err == nil {
		t.Fatal("UnmarshalDelta accepted malformed frame")
	}
	if _, err := fragment.RenderFragment(); err != nil {
		t.Fatal(err)
	}
	if _, err := RenderDocument(Node{Kind: TextNode, Text: "root"}); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("RenderDocument(text) = %v", err)
	}
	if _, err := renderNodes([]Node{{Kind: ElementNode, Name: "bad name"}}); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("RenderFragment(invalid) = %v", err)
	}

	var nilFragment *Fragment
	if _, err := nilFragment.Append([]Node{{Kind: TextNode, Text: "x"}}); !errors.Is(err, list.ErrNilList) {
		t.Fatalf("nil Append = %v", err)
	}
	if _, err := nilFragment.Insert(0, nil); !errors.Is(err, list.ErrNilList) {
		t.Fatalf("nil Insert = %v", err)
	}
	if _, err := nilFragment.Delete(0, 0); !errors.Is(err, list.ErrNilList) {
		t.Fatalf("nil Delete = %v", err)
	}
	if err := nilFragment.ApplyDelta(Delta{}); !errors.Is(err, list.ErrNilList) {
		t.Fatalf("nil ApplyDelta = %v", err)
	}
	if err := nilFragment.Merge(fragment); !errors.Is(err, list.ErrNilList) {
		t.Fatalf("nil Merge = %v", err)
	}
	if nodes, err := nilFragment.Nodes(); nodes != nil || !errors.Is(err, list.ErrNilList) {
		t.Fatalf("nil Nodes = %#v, %v", nodes, err)
	}
	if nilFragment.Positions() != nil || nilFragment.State().Type != "xml-fragment" {
		t.Fatal("nil fragment diagnostics did not fail closed")
	}
	if _, err := nilFragment.MarshalBinary(); !errors.Is(err, list.ErrNilList) {
		t.Fatalf("nil MarshalBinary = %v", err)
	}
	if err := nilFragment.UnmarshalBinary(nil); !errors.Is(err, list.ErrNilList) {
		t.Fatalf("nil UnmarshalBinary = %v", err)
	}
	if _, err := nilFragment.SnapshotCurrentState(); !errors.Is(err, list.ErrNilList) {
		t.Fatalf("nil SnapshotCurrentState = %v", err)
	}
}

func TestXMLValidationAndParsingBoundaries(t *testing.T) {
	for name, valid := range map[string]bool{
		"root": true, "Root": true, "_root": true, "a-1.b": true, "1root": false, "bad name": false,
	} {
		if got := validName(name); got != valid {
			t.Fatalf("validName(%q) = %t, want %t", name, got, valid)
		}
	}
	for value, valid := range map[string]bool{
		"text\tline":                     true,
		"bad\x00":                        false,
		string([]byte{0xff}):             false,
		string(rune(0xfffe)):             false,
		string(rune(0xffff)):             false,
		string([]byte{0xed, 0xa0, 0x80}): false,
	} {
		if got := validXMLString(value); got != valid {
			t.Fatalf("validXMLString(%q) = %t, want %t", value, got, valid)
		}
	}
	attributes := []Attribute{{Name: "z", Value: "last"}, {Name: "a", Value: "first"}}
	node := Node{Kind: ElementNode, Name: "root", Attributes: attributes}
	if err := canonicalizeAttributes(&node); err != nil || node.Attributes[0].Name != "a" {
		t.Fatalf("canonicalizeAttributes = %#v, %v", node.Attributes, err)
	}
	if err := canonicalizeAttributes(&Node{Attributes: []Attribute{{Name: "a"}, {Name: "a"}}}); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("duplicate attributes = %v", err)
	}
	if err := validateNode(Node{Kind: TextNode, Text: "x"}, 1, &nodeBudget{nodes: maxNodeCount}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("node count limit = %v", err)
	}
	if err := validateNode(Node{Kind: ElementNode, Name: "root", Attributes: []Attribute{{Name: "a"}}}, 1, &nodeBudget{attributes: maxAttributes}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("attribute limit = %v", err)
	}

	codec := nodeCodec{}
	validText := Node{Kind: TextNode, Text: "text"}
	if encoded, err := codec.Marshal(validText); err != nil {
		t.Fatal(err)
	} else if decoded, err := codec.Unmarshal(encoded); err != nil || !reflect.DeepEqual(decoded, validText) {
		t.Fatalf("text codec round trip = %#v, %v", decoded, err)
	}
	for _, invalid := range []Node{
		{Kind: TextNode, Name: "unexpected", Text: "text"},
		{Kind: TextNode, Text: string([]byte{0xff})},
		{Kind: ElementNode, Text: "not allowed"},
		{Kind: ElementNode, Name: "root", Attributes: []Attribute{{Name: "b"}, {Name: "a"}}},
		{Kind: NodeKind(99)},
	} {
		if _, err := codec.Marshal(invalid); !errors.Is(err, ErrInvalidNode) {
			t.Fatalf("Marshal(%#v) = %v", invalid, err)
		}
	}
	deep := Node{Kind: TextNode, Text: "leaf"}
	for depth := 0; depth <= maxDepth; depth++ {
		deep = Node{Kind: ElementNode, Name: "n", Children: []Node{deep}}
	}
	if _, err := codec.Marshal(deep); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("deep node = %v", err)
	}
	if _, err := codec.Unmarshal([]byte{0x81, 0x00}); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("noncanonical uvarint = %v", err)
	}
	if _, err := codec.Unmarshal([]byte{byte(ElementNode), 0x01, 'x', 0x01}); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("truncated element = %v", err)
	}

	parsed, err := ParseDocument([]byte("<?xml version=\"1.0\"?><root><child/>tail</root> \n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := RenderDocument(parsed); err != nil || got != "<root><child></child>tail</root>" {
		t.Fatalf("parsed rendering = %q, %v", got, err)
	}
	for _, input := range [][]byte{
		nil,
		[]byte("text"),
		[]byte("<root><?process test?></root>"),
		[]byte("<root bad:name=\"x\"/>"),
		[]byte("<root>\x00</root>"),
		bytes.Repeat([]byte("x"), maxDocumentBytes+1),
	} {
		if _, err := ParseDocument(input); err == nil {
			t.Fatalf("ParseDocument(%q) succeeded", input[:minXMLTestLength(len(input), 32)])
		}
	}
	tooDeep := "<n>" + strings.Repeat("<n>", maxDepth) + "x" + strings.Repeat("</n>", maxDepth+1)
	if _, err := ParseDocument([]byte(tooDeep)); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("deep document = %v", err)
	}
	for _, document := range []string{
		" <root/>",
		"<root/><!-- trailing -->",
		"<root/>\u00a0",
		"<root/>\u2003",
		"<?xml version=\"1.0\"?>",
	} {
		if _, err := ParseDocument([]byte(document)); !errors.Is(err, ErrInvalidDocument) {
			t.Fatalf("ParseDocument(%q) = %v", document, err)
		}
	}
	decoder := stdxml.NewDecoder(strings.NewReader("<root a=\"x\"/>"))
	token, err := decoder.Token()
	if err != nil {
		t.Fatal(err)
	}
	start, ok := token.(stdxml.StartElement)
	if !ok {
		t.Fatalf("first token = %T", token)
	}
	if _, err := parseElement(decoder, start, 1, &parseBudget{nodes: maxNodeCount}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("parse node limit = %v", err)
	}
	decoder = stdxml.NewDecoder(strings.NewReader("<root a=\"x\"/>"))
	token, err = decoder.Token()
	if err != nil {
		t.Fatal(err)
	}
	start = token.(stdxml.StartElement)
	if _, err := parseElement(decoder, start, 1, &parseBudget{attributes: maxAttributes}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("parse attribute limit = %v", err)
	}
}

func TestNodeReaderRejectsTruncationAndResourceBounds(t *testing.T) {
	for _, data := range [][]byte{
		nil,
		bytes.Repeat([]byte{0x80}, 10),
		{byte(NodeKind(99))},
		{byte(TextNode)},
		{byte(ElementNode), 1, 'n', 1},
		{byte(ElementNode), 1, 'n', 1, 1, 'a'},
		{byte(ElementNode), 1, 'n', 0, 1},
	} {
		reader := nodeReader{data: data}
		if _, err := reader.node(1, new(nodeBudget)); !errors.Is(err, ErrInvalidNode) {
			t.Fatalf("nodeReader(%x) = %v", data, err)
		}
	}
	reader := nodeReader{data: []byte{2, 'x'}}
	if _, err := reader.string(); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("truncated string = %v", err)
	}
	reader = nodeReader{data: []byte{byte(TextNode), 1, 'x'}}
	if node, err := reader.node(1, new(nodeBudget)); err != nil || node.Text != "x" {
		t.Fatalf("valid node read = %#v, %v", node, err)
	}
	reader = nodeReader{data: []byte{byte(TextNode), 1, 'x'}}
	if _, err := reader.node(1, &nodeBudget{nodes: maxNodeCount}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("reader node limit = %v", err)
	}
}

func minXMLTestLength(value, maximum int) int {
	if value < maximum {
		return value
	}
	return maximum
}
