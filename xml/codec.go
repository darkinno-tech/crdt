package xml

import (
	"bytes"
	"encoding/binary"

	frame "github.com/darkinno-tech/crdt/encoding"
)

const nodeCodecID = "github.com/darkinno-tech/crdt/xml-fragment-node/v1"

type nodeCodec struct{}

func (nodeCodec) ID() string { return nodeCodecID }

func (nodeCodec) Marshal(node Node) ([]byte, error) {
	if err := validateNode(node, 1, new(nodeBudget)); err != nil {
		return nil, err
	}
	encoded := make([]byte, 0, 128)
	return appendNode(encoded, node), nil
}

func (nodeCodec) Unmarshal(data []byte) (Node, error) {
	reader := nodeReader{data: data}
	node, err := reader.node(1, new(nodeBudget))
	if err != nil || reader.position != len(data) {
		return Node{}, ErrInvalidNode
	}
	if err := validateNode(node, 1, new(nodeBudget)); err != nil {
		return Node{}, err
	}
	return node, nil
}

type nodeBudget struct {
	nodes      int
	attributes int
}

func validateNode(node Node, depth int, budget *nodeBudget) error {
	if depth > maxDepth {
		return ErrResourceLimit
	}
	budget.nodes++
	if budget.nodes > maxNodeCount {
		return ErrResourceLimit
	}
	switch node.Kind {
	case TextNode:
		if node.Name != "" || len(node.Attributes) != 0 || len(node.Children) != 0 || !validXMLString(node.Text) {
			return ErrInvalidNode
		}
	case ElementNode:
		if !validName(node.Name) || node.Text != "" {
			return ErrInvalidNode
		}
		for index, attribute := range node.Attributes {
			budget.attributes++
			if budget.attributes > maxAttributes {
				return ErrResourceLimit
			}
			if !validName(attribute.Name) || !validXMLString(attribute.Value) || (index > 0 && node.Attributes[index-1].Name >= attribute.Name) {
				return ErrInvalidNode
			}
		}
		for _, child := range node.Children {
			if err := validateNode(child, depth+1, budget); err != nil {
				return err
			}
		}
	default:
		return ErrInvalidNode
	}
	return nil
}

func appendNode(output []byte, node Node) []byte {
	output = binary.AppendUvarint(output, uint64(node.Kind))
	switch node.Kind {
	case TextNode:
		return appendString(output, node.Text)
	case ElementNode:
		output = appendString(output, node.Name)
		output = binary.AppendUvarint(output, uint64(len(node.Attributes)))
		for _, attribute := range node.Attributes {
			output = appendString(output, attribute.Name)
			output = appendString(output, attribute.Value)
		}
		output = binary.AppendUvarint(output, uint64(len(node.Children)))
		for _, child := range node.Children {
			output = appendNode(output, child)
		}
		return output
	default:
		return output
	}
}

func appendString(output []byte, value string) []byte {
	output = binary.AppendUvarint(output, uint64(len(value)))
	return append(output, value...)
}

type nodeReader struct {
	data     []byte
	position int
}

func (r *nodeReader) node(depth int, budget *nodeBudget) (Node, error) {
	if depth > maxDepth {
		return Node{}, ErrResourceLimit
	}
	kind, err := r.uvarint()
	if err != nil {
		return Node{}, ErrInvalidNode
	}
	budget.nodes++
	if budget.nodes > maxNodeCount {
		return Node{}, ErrResourceLimit
	}
	switch NodeKind(kind) {
	case TextNode:
		text, err := r.string()
		if err != nil {
			return Node{}, err
		}
		return Node{Kind: TextNode, Text: text}, nil
	case ElementNode:
		name, err := r.string()
		if err != nil {
			return Node{}, err
		}
		attributeCount, err := r.uvarint()
		if err != nil || attributeCount > maxAttributes || attributeCount > uint64(len(r.data)-r.position) {
			return Node{}, ErrInvalidNode
		}
		node := Node{Kind: ElementNode, Name: name, Attributes: make([]Attribute, int(attributeCount))}
		for index := range node.Attributes {
			attributeName, err := r.string()
			if err != nil {
				return Node{}, err
			}
			attributeValue, err := r.string()
			if err != nil {
				return Node{}, err
			}
			node.Attributes[index] = Attribute{Name: attributeName, Value: attributeValue}
			budget.attributes++
			if budget.attributes > maxAttributes {
				return Node{}, ErrResourceLimit
			}
		}
		childCount, err := r.uvarint()
		if err != nil || childCount > maxNodeCount || childCount > uint64(len(r.data)-r.position) {
			return Node{}, ErrInvalidNode
		}
		node.Children = make([]Node, int(childCount))
		for index := range node.Children {
			child, err := r.node(depth+1, budget)
			if err != nil {
				return Node{}, err
			}
			node.Children[index] = child
		}
		return node, nil
	default:
		return Node{}, ErrInvalidNode
	}
}

func (r *nodeReader) uvarint() (uint64, error) {
	if r.position >= len(r.data) {
		return 0, ErrInvalidNode
	}
	value, size := binary.Uvarint(r.data[r.position:])
	if size <= 0 {
		return 0, ErrInvalidNode
	}
	canonical := binary.AppendUvarint(nil, value)
	if !bytes.Equal(canonical, r.data[r.position:r.position+size]) {
		return 0, ErrInvalidNode
	}
	r.position += size
	return value, nil
}

func (r *nodeReader) string() (string, error) {
	length, err := r.uvarint()
	if err != nil || length > uint64(len(r.data)-r.position) {
		return "", ErrInvalidNode
	}
	start := r.position
	r.position += int(length)
	return string(r.data[start:r.position]), nil
}

func defaultXMLFrameLimits() frame.DecoderLimits { return frame.DefaultLimits() }
