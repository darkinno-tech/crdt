package richtext

import (
	"math/rand"
	"reflect"
	"strconv"
	"strings"
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

// TestMultiEditorFormattingWorkloadConvergesAfterReconnect models a longer
// document-review session. Each editor makes local insert, format, and delete
// decisions from its own stale projection; reconnect replays every canonical
// frame twice in a different order. This verifies CRDT convergence rather than
// a presentation order that a concurrent RGA sibling insertion does not promise.
func TestMultiEditorFormattingWorkloadConvergesAfterReconnect(t *testing.T) {
	const editors = 4
	baseText := strings.Repeat("review paragraph ", 48)
	documents := make([]*Document, editors)
	for index := range documents {
		documents[index] = mustDocument(t, "editor-"+strconv.Itoa(index))
	}
	base, err := documents[0].Insert(0, baseText)
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range documents[1:] {
		if err := document.ApplyDelta(base); err != nil {
			t.Fatal(err)
		}
	}
	changes := []Delta{base}
	for editor, document := range documents {
		for step := 0; step < 72; step++ {
			length := document.Len()
			switch step % 4 {
			case 0:
				offset := (editor*31 + step*17) % length
				count := 1 + (editor+step)%7
				if count > length-offset {
					count = length - offset
				}
				delta, err := document.Format(offset, count, []AttributeChange{{Key: "bold", Value: "true"}})
				if err != nil {
					t.Fatal(err)
				}
				changes = append(changes, delta)
			case 1:
				offset := (editor*19 + step*13) % (length + 1)
				delta, err := document.InsertWithAttributes(offset, " +", Attributes{"author": strconv.Itoa(editor), "italic": "true"})
				if err != nil {
					t.Fatal(err)
				}
				changes = append(changes, delta)
			case 2:
				offset := (editor*23 + step*11) % length
				count := 1 + (editor*step)%5
				if count > length-offset {
					count = length - offset
				}
				delta, err := document.Format(offset, count, []AttributeChange{{Key: "review", Value: strconv.Itoa(step)}, {Key: "color", Value: "accent"}})
				if err != nil {
					t.Fatal(err)
				}
				changes = append(changes, delta)
			case 3:
				offset := (editor*29 + step*7) % length
				delta, err := document.Delete(offset, 1)
				if err != nil {
					t.Fatal(err)
				}
				changes = append(changes, delta)
			}
		}
	}
	for index, document := range documents {
		deliverRichTextChanges(t, document, changes, int64(2026073000+index))
	}
	wantText, wantSpans := documents[0].String(), documents[0].Spans()
	wantState, err := documents[0].MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range documents[1:] {
		if got := document.String(); got != wantText {
			t.Fatalf("String() = %q, want %q", got, wantText)
		}
		if got := document.Spans(); !reflect.DeepEqual(got, wantSpans) {
			t.Fatalf("Spans() = %#v, want %#v", got, wantSpans)
		}
		state, err := document.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if string(state) != string(wantState) {
			t.Fatal("converged documents produced different canonical state frames")
		}
	}
	saved, err := documents[editors-1].SnapshotCurrentState()
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
