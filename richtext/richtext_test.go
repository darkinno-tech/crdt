package richtext

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
)

func TestFormatProducesPresentationSpans(t *testing.T) {
	document := mustDocument(t, "author")
	if _, err := document.Insert(0, "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := document.Format(1, 3, []AttributeChange{{Key: "bold", Value: "true"}}); err != nil {
		t.Fatal(err)
	}
	want := []Span{
		{Text: "h"},
		{Text: "ell", Attributes: Attributes{"bold": "true"}},
		{Text: "o"},
	}
	if got := document.Spans(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Spans() = %#v, want %#v", got, want)
	}
	attributes, ok := document.AttributesAt(2)
	if !ok || !reflect.DeepEqual(attributes, Attributes{"bold": "true"}) {
		t.Fatalf("AttributesAt(2) = %#v, %t", attributes, ok)
	}
	attributes["bold"] = "changed"
	again, _ := document.AttributesAt(2)
	if again["bold"] != "true" {
		t.Fatalf("AttributesAt exposed internal map: %#v", again)
	}
	if _, ok := document.AttributesAt(5); ok {
		t.Fatal("out-of-range AttributesAt succeeded")
	}
}

func TestSpansCoalescesEqualValuesFromDifferentTags(t *testing.T) {
	document := mustDocument(t, "author")
	if _, err := document.Insert(0, "ab"); err != nil {
		t.Fatal(err)
	}
	if _, err := document.Format(0, 1, []AttributeChange{{Key: "bold", Value: "true"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := document.Format(1, 1, []AttributeChange{{Key: "bold", Value: "true"}}); err != nil {
		t.Fatal(err)
	}
	want := []Span{{Text: "ab", Attributes: Attributes{"bold": "true"}}}
	if got := document.Spans(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Spans() = %#v, want %#v", got, want)
	}
}

func TestSpansLargeUniformRunAndAttributeDifference(t *testing.T) {
	document := mustDocument(t, "author")
	value := strings.Repeat("a", 65)
	if _, err := document.Insert(0, value); err != nil {
		t.Fatal(err)
	}
	if got := document.Spans(); !reflect.DeepEqual(got, []Span{{Text: value}}) {
		t.Fatalf("Spans() = %#v", got)
	}
	if attributesEqual(Attributes{"bold": "true"}, Attributes{"bold": "false"}) {
		t.Fatal("different attributes compared equal")
	}
}

func TestApplyDeltaBoundsTargetAttributeProduct(t *testing.T) {
	options := DefaultOptions()
	options.MaxMarkEntries = 2
	document, err := NewWithOptions("author", options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := document.Insert(0, "ab"); err != nil {
		t.Fatal(err)
	}
	seed, err := document.Format(0, 2, []AttributeChange{{Key: "bold", Value: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(seed.operations) != 1 {
		t.Fatalf("seed operations = %d", len(seed.operations))
	}
	before := document.Spans()
	attack := Delta{operations: []formatOperation{
		{tag: crdt.Tag{ReplicaID: "peer", WallTime: 2}, targets: seed.operations[0].targets, changes: []AttributeChange{{Key: "bold", Value: "true"}}},
		{tag: crdt.Tag{ReplicaID: "peer", WallTime: 3}, targets: seed.operations[0].targets, changes: []AttributeChange{{Key: "bold", Value: "true"}}},
	}}
	if err := document.ApplyDelta(attack); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("oversized target/key work = %v", err)
	}
	if got := document.Spans(); !reflect.DeepEqual(got, before) {
		t.Fatalf("rejected delta changed spans: %#v, want %#v", got, before)
	}
}

func TestInsertWithAttributesAndFormatRemoval(t *testing.T) {
	document := mustDocument(t, "author")
	if _, err := document.InsertWithAttributes(0, "link", Attributes{"href": "https://example.test"}); err != nil {
		t.Fatal(err)
	}
	if got, want := document.Spans(), []Span{{Text: "link", Attributes: Attributes{"href": "https://example.test"}}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("insert spans = %#v, want %#v", got, want)
	}
	if _, err := document.Format(1, 2, []AttributeChange{{Key: "href", Remove: true}}); err != nil {
		t.Fatal(err)
	}
	want := []Span{
		{Text: "l", Attributes: Attributes{"href": "https://example.test"}},
		{Text: "in"},
		{Text: "k", Attributes: Attributes{"href": "https://example.test"}},
	}
	if got := document.Spans(); !reflect.DeepEqual(got, want) {
		t.Fatalf("removal spans = %#v, want %#v", got, want)
	}
	if _, err := document.InsertWithAttributes(4, "!", Attributes{"href": ""}); err != nil {
		t.Fatal(err)
	}
	if got := document.String(); got != "link!" {
		t.Fatalf("String() = %q", got)
	}
}

func TestThreeReplicaOutOfOrderFormattingConverges(t *testing.T) {
	alice := mustDocument(t, "alice")
	seed, err := alice.Insert(0, "draft")
	if err != nil {
		t.Fatal(err)
	}
	bob, carol := mustDocument(t, "bob"), mustDocument(t, "carol")
	for _, document := range []*Document{bob, carol} {
		if err := document.ApplyDelta(seed); err != nil {
			t.Fatal(err)
		}
	}
	aliceBold, err := alice.Format(0, 3, []AttributeChange{{Key: "bold", Value: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	bobItalic, err := bob.Format(2, 3, []AttributeChange{{Key: "italic", Value: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	carolLink, err := carol.InsertWithAttributes(5, "!", Attributes{"href": "https://example.test"})
	if err != nil {
		t.Fatal(err)
	}

	for _, change := range []Delta{carolLink, aliceBold, bobItalic, aliceBold} {
		if err := alice.ApplyDelta(change); err != nil {
			t.Fatal(err)
		}
	}
	for _, change := range []Delta{bobItalic, aliceBold, carolLink, bobItalic} {
		if err := bob.ApplyDelta(change); err != nil {
			t.Fatal(err)
		}
	}
	for _, change := range []Delta{aliceBold, carolLink, bobItalic, carolLink} {
		if err := carol.ApplyDelta(change); err != nil {
			t.Fatal(err)
		}
	}
	wantText, wantSpans := alice.String(), alice.Spans()
	for _, document := range []*Document{bob, carol} {
		if got := document.String(); got != wantText {
			t.Fatalf("String() = %q, want %q", got, wantText)
		}
		if got := document.Spans(); !reflect.DeepEqual(got, wantSpans) {
			t.Fatalf("Spans() = %#v, want %#v", got, wantSpans)
		}
	}
}

func TestFormatBeforeTextAndRemovalBeforeAssignmentConverge(t *testing.T) {
	source := mustDocument(t, "source")
	insert, err := source.Insert(0, "xy")
	if err != nil {
		t.Fatal(err)
	}
	set, err := source.Format(0, 2, []AttributeChange{{Key: "color", Value: "blue"}})
	if err != nil {
		t.Fatal(err)
	}
	remove, err := source.Format(0, 2, []AttributeChange{{Key: "color", Remove: true}})
	if err != nil {
		t.Fatal(err)
	}

	target := mustDocument(t, "target")
	for _, change := range []Delta{remove, set, insert, remove} {
		if err := target.ApplyDelta(change); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := target.String(), "xy"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if got, want := target.Spans(), []Span{{Text: "xy"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Spans() = %#v, want %#v", got, want)
	}
}

func TestSnapshotMergeAndExperimentalManifest(t *testing.T) {
	source := mustDocument(t, "source")
	if _, err := source.InsertWithAttributes(0, "stable", Attributes{"bold": "true"}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Format(2, 2, []AttributeChange{{Key: "comment", Value: "review"}}); err != nil {
		t.Fatal(err)
	}
	saved, err := source.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	if saved.TypeID != crdt.TypeIDRichTextState {
		t.Fatalf("snapshot type = %d", saved.TypeID)
	}
	restored, err := NewFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := restored.Spans(), source.Spans(); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored spans = %#v, want %#v", got, want)
	}

	other := mustDocument(t, "other")
	if _, err := other.Insert(0, "!"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Format(0, 1, []AttributeChange{{Key: "italic", Value: "true"}}); err != nil {
		t.Fatal(err)
	}
	if err := restored.Merge(other); err != nil {
		t.Fatal(err)
	}
	if got := restored.String(); len([]rune(got)) != 7 {
		t.Fatalf("merged String() = %q", got)
	}
	if (crdt.ProtocolPolicy{}).SupportsFrame(crdt.TypeIDRichTextDelta) {
		t.Fatal("zero policy accepted rich text")
	}
	if !(crdt.ProtocolPolicy{AllowExperimental: true}).SupportsFrame(crdt.TypeIDRichTextState) {
		t.Fatal("experimental policy rejected rich text")
	}
}

func TestRejectsInvalidInputAndPreservesContent(t *testing.T) {
	document := mustDocument(t, "author")
	if _, err := document.Insert(0, "safe"); err != nil {
		t.Fatal(err)
	}
	if _, err := document.Format(0, 1, []AttributeChange{{Key: "x", Value: "1"}, {Key: "x", Value: "2"}}); !errors.Is(err, ErrInvalidAttribute) {
		t.Fatalf("duplicate key error = %v", err)
	}
	if _, err := document.Format(0, 1, []AttributeChange{{Key: "x", Value: "not-empty", Remove: true}}); !errors.Is(err, ErrInvalidAttribute) {
		t.Fatalf("ambiguous remove error = %v", err)
	}
	limited := DefaultOptions()
	limited.MaxMarkEntries = 1
	bounded, err := NewWithOptions("bounded", limited)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bounded.Insert(0, "ab"); err != nil {
		t.Fatal(err)
	}
	if _, err := bounded.Format(0, 2, []AttributeChange{{Key: "bold", Value: "true"}}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("mark limit error = %v", err)
	}
	if got := bounded.Spans(); !reflect.DeepEqual(got, []Span{{Text: "ab"}}) {
		t.Fatalf("mark-limit failure mutated content: %#v", got)
	}

	encoded, err := document.Format(0, 1, []AttributeChange{{Key: "bold", Value: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	after := document.Spans()
	data, err := encoded.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0x80
	if _, err := UnmarshalDelta(data); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("corrupt delta error = %v", err)
	}
	if got := document.Spans(); !reflect.DeepEqual(got, after) {
		t.Fatalf("decode failure mutated document: %#v", got)
	}

	tight := frame.DefaultLimits()
	tight.MaxPayload = 1
	if _, err := document.FormatWithLimits(0, 1, []AttributeChange{{Key: "italic", Value: "true"}}, tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("tight output error = %v", err)
	}
	if got := document.String(); got != "safe" {
		t.Fatalf("tight failure changed text: %q", got)
	}
}

func mustDocument(t testing.TB, replicaID string) *Document {
	t.Helper()
	document, err := New(replicaID)
	if err != nil {
		t.Fatal(err)
	}
	return document
}
