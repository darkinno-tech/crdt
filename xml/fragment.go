// Package xml provides a bounded, deterministic XML fragment CRDT.
//
// A fragment is an ordered RGA of immutable XML nodes. Editing a node means
// inserting a replacement and deleting the old node; this deliberately avoids
// pretending that concurrent attribute writes have a proven merge rule. Node
// insertions, deletions, duplicate delivery, and out-of-order delivery use the
// generic list RGA protocol and remain convergent.
package xml

import (
	"bytes"
	stdxml "encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"unicode/utf8"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/list"
	"github.com/DarkInno/crdt/snapshot"
)

var (
	ErrInvalidNode     = errors.New("xml: invalid node")
	ErrInvalidDocument = errors.New("xml: invalid XML document")
	ErrResourceLimit   = errors.New("xml: resource limit exceeded")
)

// NodeKind identifies either an element or a text node.
type NodeKind uint8

const (
	ElementNode NodeKind = 1
	TextNode    NodeKind = 2
)

// Attribute is one element attribute. Attribute names are ASCII XML names,
// unique within a node, and stored in canonical lexical order.
type Attribute struct {
	Name  string
	Value string
}

// Node is an immutable-by-convention XML value. Insert copies its canonical
// representation, and Values returns newly decoded values, so callers can
// safely mutate their own slices after either call.
//
// Namespace prefixes, DTDs, comments, processing instructions, and mixed
// non-text token classes are intentionally unsupported in v1. Text and child
// nodes may be mixed inside an element.
type Node struct {
	Kind       NodeKind
	Name       string
	Attributes []Attribute
	Text       string
	Children   []Node
}

// Fragment is an ordered, collaboratively replicated XML fragment.
type Fragment struct {
	list *list.RGA[Node]
}

// Delta is a joinable fragment mutation.
type Delta = list.Delta

// Position is a stable fragment node identity.
type Position = crdt.Tag

// New creates a fragment with the generic list's conservative default limits.
func New(replicaID string) (*Fragment, error) {
	return NewWithOptions(replicaID, list.DefaultOptions())
}

// NewWithOptions creates a fragment with explicit retained-state limits.
func NewWithOptions(replicaID string, options list.Options) (*Fragment, error) {
	value, err := list.NewWithOptions(replicaID, nodeCodec{}, options)
	if err != nil {
		return nil, err
	}
	return &Fragment{list: value}, nil
}

// Insert adds nodes before visible node offset.
func (f *Fragment) Insert(offset int, nodes []Node) (Delta, error) {
	if f == nil || f.list == nil {
		return Delta{}, list.ErrNilList
	}
	return f.list.Insert(offset, nodes)
}

// Append adds nodes after the current visible tail.
func (f *Fragment) Append(nodes []Node) (Delta, error) {
	if f == nil || f.list == nil {
		return Delta{}, list.ErrNilList
	}
	return f.list.Append(nodes)
}

// Delete removes count visible nodes beginning at offset.
func (f *Fragment) Delete(offset, count int) (Delta, error) {
	if f == nil || f.list == nil {
		return Delta{}, list.ErrNilList
	}
	return f.list.Delete(offset, count)
}

// ApplyDelta integrates one decoded fragment mutation.
func (f *Fragment) ApplyDelta(delta Delta) error {
	if f == nil || f.list == nil {
		return list.ErrNilList
	}
	return f.list.ApplyDelta(delta)
}

// Merge joins every retained node and deletion from other.
func (f *Fragment) Merge(other *Fragment) error {
	if f == nil || f.list == nil || other == nil || other.list == nil {
		return list.ErrNilList
	}
	return f.list.Merge(other.list)
}

// Nodes returns the visible fragment node projection.
func (f *Fragment) Nodes() ([]Node, error) {
	if f == nil || f.list == nil {
		return nil, list.ErrNilList
	}
	return f.list.Values()
}

// Positions returns visible stable node IDs in fragment order.
func (f *Fragment) Positions() []Position {
	if f == nil || f.list == nil {
		return nil
	}
	return f.list.Positions()
}

// State returns a value-free diagnostic summary.
func (f *Fragment) State() crdt.StateSnapshot {
	if f == nil || f.list == nil {
		return crdt.StateSnapshot{Type: "xml-fragment"}
	}
	state := f.list.State()
	state.Type = "xml-fragment"
	return state
}

// MarshalBinary encodes a canonical complete fragment state frame.
func (f *Fragment) MarshalBinary() ([]byte, error) {
	if f == nil || f.list == nil {
		return nil, list.ErrNilList
	}
	return f.list.MarshalBinary()
}

// UnmarshalBinary validates a complete framed state before replacing f.
func (f *Fragment) UnmarshalBinary(data []byte) error {
	if f == nil || f.list == nil {
		return list.ErrNilList
	}
	return f.list.UnmarshalBinary(data)
}

// SnapshotCurrentState creates an HLC-backed complete fragment snapshot.
func (f *Fragment) SnapshotCurrentState() (snapshot.Snapshot, error) {
	if f == nil || f.list == nil {
		return snapshot.Snapshot{}, list.ErrNilList
	}
	return f.list.SnapshotCurrentState()
}

// NewFromSnapshot restores a complete fragment snapshot with default limits.
func NewFromSnapshot(saved snapshot.Snapshot) (*Fragment, error) {
	return NewFromSnapshotWithOptions(saved, list.DefaultOptions())
}

// NewFromSnapshotWithOptions restores a complete fragment snapshot within
// explicit list retention and decoder limits.
func NewFromSnapshotWithOptions(saved snapshot.Snapshot, options list.Options) (*Fragment, error) {
	value, err := list.NewFromSnapshotWithOptions(saved, nodeCodec{}, options, defaultXMLFrameLimits())
	if err != nil {
		return nil, err
	}
	return &Fragment{list: value}, nil
}

// MarshalDelta returns a canonical fragment delta frame.
func MarshalDelta(delta Delta) ([]byte, error) { return delta.MarshalBinary() }

// UnmarshalDelta validates one bounded canonical fragment delta frame.
func UnmarshalDelta(data []byte) (Delta, error) { return list.UnmarshalDelta(data, nodeCodec{}) }

// RenderFragment returns the concatenated XML rendering of visible top-level
// nodes. A fragment containing multiple elements is intentionally not a full
// XML document; use RenderDocument for one document root.
func (f *Fragment) RenderFragment() (string, error) {
	nodes, err := f.Nodes()
	if err != nil {
		return "", err
	}
	return renderNodes(nodes)
}

// ParseDocument parses one strict, bounded XML document into a node. It
// rejects directives, comments, processing instructions, namespaces, and
// extra top-level content so every accepted result has one deterministic v1
// representation.
func ParseDocument(data []byte) (Node, error) {
	if len(data) == 0 || len(data) > maxDocumentBytes {
		return Node{}, ErrResourceLimit
	}
	decoder := stdxml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = true
	budget := parseBudget{}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return Node{}, ErrInvalidDocument
		}
		if err != nil {
			return Node{}, fmt.Errorf("%w: %v", ErrInvalidDocument, err)
		}
		if declaration, ok := token.(stdxml.ProcInst); ok {
			if declaration.Target == "xml" {
				continue
			}
			return Node{}, ErrInvalidDocument
		}
		start, ok := token.(stdxml.StartElement)
		if !ok {
			return Node{}, ErrInvalidDocument
		}
		node, err := parseElement(decoder, start, 1, &budget)
		if err != nil {
			return Node{}, err
		}
		for {
			token, err = decoder.Token()
			if err == io.EOF {
				return node, nil
			}
			if err != nil {
				return Node{}, fmt.Errorf("%w: %v", ErrInvalidDocument, err)
			}
			if text, ok := token.(stdxml.CharData); ok && xmlWhitespace(text) {
				continue
			}
			return Node{}, ErrInvalidDocument
		}
	}
}

// RenderDocument returns one deterministic XML document. node must be an
// element because XML documents cannot have text as their root.
func RenderDocument(node Node) (string, error) {
	if node.Kind != ElementNode {
		return "", ErrInvalidDocument
	}
	return renderNodes([]Node{node})
}

const (
	maxDocumentBytes = 4 << 20
	maxNodeCount     = 1 << 16
	maxDepth         = 128
	maxAttributes    = 1 << 15
)

type parseBudget struct {
	nodes      int
	attributes int
}

func parseElement(decoder *stdxml.Decoder, start stdxml.StartElement, depth int, budget *parseBudget) (Node, error) {
	if depth > maxDepth || start.Name.Space != "" || !validName(start.Name.Local) {
		return Node{}, ErrInvalidDocument
	}
	budget.nodes++
	if budget.nodes > maxNodeCount {
		return Node{}, ErrResourceLimit
	}
	node := Node{Kind: ElementNode, Name: start.Name.Local, Attributes: make([]Attribute, 0, len(start.Attr))}
	for _, attribute := range start.Attr {
		if attribute.Name.Space != "" || !validName(attribute.Name.Local) || !validXMLString(attribute.Value) {
			return Node{}, ErrInvalidDocument
		}
		budget.attributes++
		if budget.attributes > maxAttributes {
			return Node{}, ErrResourceLimit
		}
		node.Attributes = append(node.Attributes, Attribute{Name: attribute.Name.Local, Value: attribute.Value})
	}
	if err := canonicalizeAttributes(&node); err != nil {
		return Node{}, err
	}
	for {
		token, err := decoder.Token()
		if err != nil {
			return Node{}, fmt.Errorf("%w: %v", ErrInvalidDocument, err)
		}
		switch value := token.(type) {
		case stdxml.StartElement:
			child, err := parseElement(decoder, value, depth+1, budget)
			if err != nil {
				return Node{}, err
			}
			node.Children = append(node.Children, child)
		case stdxml.CharData:
			text := string(value)
			if text == "" {
				continue
			}
			if !validXMLString(text) {
				return Node{}, ErrInvalidDocument
			}
			budget.nodes++
			if budget.nodes > maxNodeCount {
				return Node{}, ErrResourceLimit
			}
			node.Children = append(node.Children, Node{Kind: TextNode, Text: text})
		case stdxml.EndElement:
			if value.Name.Space != "" || value.Name.Local != node.Name {
				return Node{}, ErrInvalidDocument
			}
			return node, nil
		default:
			return Node{}, ErrInvalidDocument
		}
	}
}

func renderNodes(nodes []Node) (string, error) {
	var buffer bytes.Buffer
	encoder := stdxml.NewEncoder(&buffer)
	for _, node := range nodes {
		if err := encodeNode(encoder, node, 1); err != nil {
			return "", err
		}
	}
	if err := encoder.Flush(); err != nil {
		return "", err
	}
	return buffer.String(), nil
}

func encodeNode(encoder *stdxml.Encoder, node Node, depth int) error {
	if err := validateNode(node, depth, new(nodeBudget)); err != nil {
		return err
	}
	switch node.Kind {
	case TextNode:
		return encoder.EncodeToken(stdxml.CharData(node.Text))
	case ElementNode:
		start := stdxml.StartElement{Name: stdxml.Name{Local: node.Name}, Attr: make([]stdxml.Attr, len(node.Attributes))}
		for index, attribute := range node.Attributes {
			start.Attr[index] = stdxml.Attr{Name: stdxml.Name{Local: attribute.Name}, Value: attribute.Value}
		}
		if err := encoder.EncodeToken(start); err != nil {
			return err
		}
		for _, child := range node.Children {
			if err := encodeNode(encoder, child, depth+1); err != nil {
				return err
			}
		}
		return encoder.EncodeToken(start.End())
	default:
		return ErrInvalidNode
	}
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for index := 0; index < len(name); index++ {
		value := name[index]
		if (index == 0 && !nameStart(value)) || (index > 0 && !namePart(value)) {
			return false
		}
	}
	return true
}

func nameStart(value byte) bool {
	switch {
	case value == '_':
		return true
	case value >= 'A' && value <= 'Z':
		return true
	case value >= 'a' && value <= 'z':
		return true
	default:
		return false
	}
}

func namePart(value byte) bool {
	switch {
	case nameStart(value):
		return true
	case value == '-' || value == '.':
		return true
	case value >= '0' && value <= '9':
		return true
	default:
		return false
	}
}

func validXMLString(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, runeValue := range value {
		if !validXMLRune(runeValue) {
			return false
		}
	}
	return true
}

func validXMLRune(value rune) bool {
	return value == '\t' || value == '\n' || value == '\r' ||
		(value >= 0x20 && value <= 0xd7ff) ||
		(value >= 0xe000 && value <= 0xfffd) ||
		(value >= 0x10000 && value <= 0x10ffff)
}

func xmlWhitespace(value []byte) bool {
	for _, byteValue := range value {
		if byteValue != ' ' && byteValue != '\t' && byteValue != '\n' && byteValue != '\r' {
			return false
		}
	}
	return true
}

func canonicalizeAttributes(node *Node) error {
	sort.Slice(node.Attributes, func(left, right int) bool { return node.Attributes[left].Name < node.Attributes[right].Name })
	for index, attribute := range node.Attributes {
		if !validName(attribute.Name) || !validXMLString(attribute.Value) || (index > 0 && node.Attributes[index-1].Name == attribute.Name) {
			return ErrInvalidNode
		}
	}
	return nil
}
