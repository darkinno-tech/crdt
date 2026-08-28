package richtext

import (
	"errors"
	"reflect"
	"testing"

	"github.com/darkinno-tech/crdt/text"
)

func TestFormatAnchoredTracksRelativeBoundaries(t *testing.T) {
	document := mustDocument(t, "anchors")
	if _, err := document.Insert(0, "abcd"); err != nil {
		t.Fatal(err)
	}
	start, err := document.AnchorAt(1)
	if err != nil {
		t.Fatal(err)
	}
	end, err := document.AnchorAt(3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := document.Insert(0, "X"); err != nil {
		t.Fatal(err)
	}
	if _, err := document.FormatAnchored(start, end, []AttributeChange{{Key: AttributeBold, Value: "true"}}); err != nil {
		t.Fatal(err)
	}
	if got := document.String(); got != "Xabcd" {
		t.Fatalf("String() = %q, want Xabcd", got)
	}
	for offset, want := range []bool{false, false, true, true, false} {
		attributes, ok := document.AttributesAt(offset)
		if !ok || (attributes[AttributeBold] == "true") != want {
			t.Fatalf("AttributesAt(%d) = %#v, %t; bold = %t", offset, attributes, ok, want)
		}
	}
	if _, err := document.FormatAnchored(end, start, nil); !errors.Is(err, text.ErrRange) {
		t.Fatalf("reversed anchors = %v, want %v", err, text.ErrRange)
	}
}

func TestSemanticInlineEmbedAndBlockFormattingRoundTrip(t *testing.T) {
	document := mustDocument(t, "semantic")
	if _, err := document.Insert(0, "Title\nbody"); err != nil {
		t.Fatal(err)
	}
	if _, err := document.SetBold(0, 5, true); err != nil {
		t.Fatal(err)
	}
	if _, err := document.SetItalic(1, 3, true); err != nil {
		t.Fatal(err)
	}
	if _, err := document.FormatBlocks(2, 1, BlockFormat{Kind: "heading", Level: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := document.InsertEmbed(document.Len(), Embed{Kind: "image", Data: `{"asset":"logo-7","alt":"Logo"}`}); err != nil {
		t.Fatal(err)
	}

	if got, ok := document.BlockFormatAt(0); !ok || got != (BlockFormat{Kind: "heading", Level: 2}) {
		t.Fatalf("BlockFormatAt(0) = %#v, %t", got, ok)
	}
	if got, ok := document.BlockFormatAt(6); ok || got != (BlockFormat{}) {
		t.Fatalf("BlockFormatAt(body) = %#v, %t", got, ok)
	}
	embed, ok := document.EmbedAt(document.Len() - 1)
	if !ok || embed != (Embed{Kind: "image", Data: `{"asset":"logo-7","alt":"Logo"}`}) {
		t.Fatalf("EmbedAt = %#v, %t", embed, ok)
	}

	state, err := document.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	restored := mustDocument(t, "semantic-restored")
	if err := restored.UnmarshalBinary(state); err != nil {
		t.Fatal(err)
	}
	if got, want := restored.Spans(), document.Spans(); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored spans = %#v, want %#v", got, want)
	}
	if got, ok := restored.EmbedAt(restored.Len() - 1); !ok || got != embed {
		t.Fatalf("restored EmbedAt = %#v, %t", got, ok)
	}
}

func TestSemanticValidationFailsBeforeMutation(t *testing.T) {
	document := mustDocument(t, "semantic-invalid")
	if _, err := document.Insert(0, "body"); err != nil {
		t.Fatal(err)
	}
	before := document.String()
	for _, embed := range []Embed{
		{Kind: "image", Data: `[]`},
		{Kind: "IMAGE", Data: `{}`},
		{Kind: "image", Data: `{"unterminated"`},
	} {
		if _, err := document.InsertEmbed(0, embed); !errors.Is(err, ErrInvalidSemantic) {
			t.Fatalf("InsertEmbed(%#v) = %v, want %v", embed, err, ErrInvalidSemantic)
		}
	}
	if _, err := document.FormatBlocks(0, 1, BlockFormat{Kind: "heading", Level: 7}); !errors.Is(err, ErrInvalidSemantic) {
		t.Fatalf("invalid heading = %v, want %v", err, ErrInvalidSemantic)
	}
	if got := document.String(); got != before {
		t.Fatalf("invalid semantic mutations changed text = %q, want %q", got, before)
	}
	if _, err := document.InsertWithAttributes(0, ObjectReplacementCharacter, Attributes{
		AttributeEmbedKind: "image", AttributeEmbedData: `[]`,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := document.EmbedAt(0); ok {
		t.Fatal("EmbedAt accepted generic malformed embed metadata")
	}
}

func TestSemanticBoundaryAndParsingPaths(t *testing.T) {
	document := mustDocument(t, "semantic-boundaries")
	if _, err := document.Insert(0, "a\nb"); err != nil {
		t.Fatal(err)
	}
	if _, err := document.SetBold(0, 1, false); err != nil {
		t.Fatal(err)
	}
	if _, err := document.FormatBlocks(0, 0, BlockFormat{Kind: "paragraph"}); err != nil {
		t.Fatal(err)
	}
	if _, err := document.FormatBlocks(3, 1, BlockFormat{Kind: "paragraph"}); !errors.Is(err, ErrInvalidSemantic) {
		t.Fatalf("out-of-range block = %v, want %v", err, ErrInvalidSemantic)
	}
	if _, err := document.Format(0, 1, []AttributeChange{{Key: AttributeBlock, Value: "heading:0"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := document.BlockFormatAt(0); ok {
		t.Fatal("BlockFormatAt accepted invalid raw block attribute")
	}
	for _, offset := range []int{-1, 1, 3} {
		if _, ok := document.EmbedAt(offset); ok {
			t.Fatalf("EmbedAt(%d) unexpectedly succeeded", offset)
		}
	}
	var nilDocument *Document
	if _, err := nilDocument.FormatBlocks(0, 0, BlockFormat{Kind: "paragraph"}); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil FormatBlocks = %v, want %v", err, ErrNilDocument)
	}
	if _, err := nilDocument.FormatAnchored(text.Anchor{}, text.Anchor{}, nil); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil FormatAnchored = %v, want %v", err, ErrNilDocument)
	}
	if _, err := document.FormatAnchored(text.Anchor{}, text.Anchor{}, nil); !errors.Is(err, text.ErrInvalidAnchor) {
		t.Fatalf("invalid FormatAnchored = %v, want %v", err, text.ErrInvalidAnchor)
	}

	for _, test := range []struct {
		value string
		want  BlockFormat
		ok    bool
	}{
		{value: "quote", want: BlockFormat{Kind: "quote"}, ok: true},
		{value: "heading:6", want: BlockFormat{Kind: "heading", Level: 6}, ok: true},
		{value: "heading:7"},
		{value: "quote:1"},
		{value: "heading:1:extra"},
	} {
		got, ok := parseBlockFormat(test.value)
		if got != test.want || ok != test.ok {
			t.Fatalf("parseBlockFormat(%q) = %#v, %t; want %#v, %t", test.value, got, ok, test.want, test.ok)
		}
	}
	if got := blockLevel(10); got != "10" {
		t.Fatalf("blockLevel(10) = %q, want 10", got)
	}
	if validSemanticKind("bad space") || !validSemanticKind("valid-kind_2") {
		t.Fatal("semantic kind validation did not enforce the identifier grammar")
	}
	if start, end := paragraphBounds([]rune("a\nb"), 2, 3); start != 2 || end != 3 {
		t.Fatalf("final paragraph bounds = %d, %d; want 2, 3", start, end)
	}
	if start, end := paragraphBounds([]rune("a\nb"), 2, 2); start != 2 || end != 3 {
		t.Fatalf("collapsed paragraph bounds = %d, %d; want 2, 3", start, end)
	}
}

func TestSemanticFormatConvergesAfterShuffledDelivery(t *testing.T) {
	alice, bob := mustDocument(t, "semantic-alice"), mustDocument(t, "semantic-bob")
	seed, err := alice.Insert(0, "One\nTwo")
	if err != nil {
		t.Fatal(err)
	}
	if err := bob.ApplyDelta(seed); err != nil {
		t.Fatal(err)
	}
	bold, err := alice.SetBold(0, 3, true)
	if err != nil {
		t.Fatal(err)
	}
	block, err := bob.FormatBlocks(4, 1, BlockFormat{Kind: "quote"})
	if err != nil {
		t.Fatal(err)
	}
	embed, err := bob.InsertEmbed(bob.Len(), Embed{Kind: "mention", Data: `{"id":"u-7"}`})
	if err != nil {
		t.Fatal(err)
	}
	changes := []Delta{seed, bold, block, embed}
	deliverRichTextChanges(t, alice, changes, 41)
	deliverRichTextChanges(t, bob, changes, 99)
	left, err := alice.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	right, err := bob.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(right) {
		t.Fatal("semantic replicas did not produce equal canonical state")
	}
}

func TestSemanticBlocksProjectClearAndDetectConflicts(t *testing.T) {
	document := mustDocument(t, "semantic-blocks")
	if _, err := document.Insert(0, "Title\n\nbody"); err != nil {
		t.Fatal(err)
	}
	if _, err := document.FormatBlocks(0, 0, BlockFormat{Kind: "heading", Level: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := document.FormatBlocks(7, 0, BlockFormat{Kind: "quote"}); err != nil {
		t.Fatal(err)
	}
	want := []Block{
		{Text: "Title", Format: BlockFormat{Kind: "heading", Level: 1}, Formatted: true},
		{Text: ""},
		{Text: "body", Format: BlockFormat{Kind: "quote"}, Formatted: true},
	}
	if got := document.Blocks(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Blocks() = %#v, want %#v", got, want)
	}
	if _, err := document.ClearBlocks(0, 0); err != nil {
		t.Fatal(err)
	}
	if got := document.Blocks()[0]; got.Formatted {
		t.Fatalf("cleared title block = %#v", got)
	}
	if _, err := document.Format(7, 1, []AttributeChange{{Key: AttributeBlock, Value: "code"}}); err != nil {
		t.Fatal(err)
	}
	if got := document.Blocks()[2]; got.Formatted {
		t.Fatalf("mixed block conflict was hidden: %#v", got)
	}

	empty := mustDocument(t, "semantic-empty-blocks")
	if got := empty.Blocks(); got != nil {
		t.Fatalf("empty Blocks() = %#v, want nil", got)
	}
}

func TestSemanticAnchoredBlocksAndExplicitInsertion(t *testing.T) {
	document := mustDocument(t, "semantic-anchored-blocks")
	if _, err := document.Insert(0, "one\ntwo"); err != nil {
		t.Fatal(err)
	}
	start, err := document.AnchorAt(4)
	if err != nil {
		t.Fatal(err)
	}
	end, err := document.AnchorAt(document.Len())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := document.Insert(0, "X"); err != nil {
		t.Fatal(err)
	}
	if _, err := document.FormatBlocksAnchored(start, end, BlockFormat{Kind: "code"}); err != nil {
		t.Fatal(err)
	}
	if got, want := document.Blocks()[1], (Block{Text: "two", Format: BlockFormat{Kind: "code"}, Formatted: true}); got != want {
		t.Fatalf("anchored block = %#v, want %#v", got, want)
	}
	if _, err := document.ClearBlocksAnchored(start, end); err != nil {
		t.Fatal(err)
	}
	if got := document.Blocks()[1]; got.Formatted {
		t.Fatalf("anchored clear block = %#v", got)
	}
	if _, err := document.InsertWithBlockFormat(document.Len(), "!", Attributes{AttributeBold: "true"}, BlockFormat{Kind: "paragraph"}); err != nil {
		t.Fatal(err)
	}
	if attributes, ok := document.AttributesAt(document.Len() - 1); !ok || attributes[AttributeBlock] != "paragraph" || attributes[AttributeBold] != "true" {
		t.Fatalf("explicit block insertion attributes = %#v, %t", attributes, ok)
	}
	if _, err := document.InsertWithBlockFormat(0, "x", Attributes{AttributeBlock: "quote"}, BlockFormat{Kind: "paragraph"}); !errors.Is(err, ErrInvalidSemantic) {
		t.Fatalf("reserved block override = %v, want %v", err, ErrInvalidSemantic)
	}
}
