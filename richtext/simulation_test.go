package richtext

import (
	"math/rand"
	"reflect"
	"testing"
)

// TestEditorialReviewOverUnreliableNetwork models a realistic asynchronous
// review: independently authored inline formatting, an attributed insertion,
// a deletion, duplicate encoded frames, shuffled delivery, and snapshot
// recovery all converge to the same presentable document.
func TestEditorialReviewOverUnreliableNetwork(t *testing.T) {
	alice, bob, carol := mustDocument(t, "alice"), mustDocument(t, "bob"), mustDocument(t, "carol")
	base, err := alice.Insert(0, "Project plan")
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range []*Document{bob, carol} {
		if err := document.ApplyDelta(base); err != nil {
			t.Fatal(err)
		}
	}
	bold, err := alice.Format(0, len([]rune("Project")), []AttributeChange{{Key: "bold", Value: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	comment, err := bob.Format(len([]rune("Project ")), len([]rune("plan")), []AttributeChange{{Key: "comment", Value: "needs-owner"}})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := carol.InsertWithAttributes(carol.Len(), " v2", Attributes{"italic": "true"})
	if err != nil {
		t.Fatal(err)
	}
	cut, err := carol.Delete(len([]rune("Project ")), 1)
	if err != nil {
		t.Fatal(err)
	}
	changes := []Delta{base, bold, comment, revision, cut}
	for index, document := range []*Document{alice, bob, carol} {
		deliverRichTextChanges(t, document, changes, int64(20260729+index))
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
	saved, err := bob.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	if got := recovered.Spans(); !reflect.DeepEqual(got, wantSpans) {
		t.Fatalf("recovered spans = %#v, want %#v", got, wantSpans)
	}
}

func deliverRichTextChanges(t testing.TB, document *Document, changes []Delta, seed int64) {
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
		if err := document.ApplyDelta(change); err != nil {
			t.Fatal(err)
		}
	}
}
