package xml

import "testing"

func TestRenderDocumentRejectsNestedInvalidNode(t *testing.T) {
	root := Node{
		Kind: ElementNode,
		Name: "root",
		Children: []Node{{
			Kind: ElementNode,
			Name: "child",
			Attributes: []Attribute{{
				Name: "bad name",
			}},
		}},
	}
	if _, err := RenderDocument(root); err != ErrInvalidNode {
		t.Fatalf("RenderDocument(nested invalid node) = %v, want %v", err, ErrInvalidNode)
	}
}

func BenchmarkRenderDocumentDeepTree(b *testing.B) {
	node := benchmarkDeepTree()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := RenderDocument(node); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkDeepTree() Node {
	node := Node{Kind: TextNode, Text: "leaf"}
	for depth := 1; depth < maxDepth; depth++ {
		node = Node{Kind: ElementNode, Name: "n", Children: []Node{node}}
	}
	return node
}
