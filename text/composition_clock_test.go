package text

import (
	"testing"

	"github.com/im10furry/crdt"
	frame "github.com/im10furry/crdt/encoding"
)

func TestPrepareRunDeltasDoNotMutateTextBeforeApply(t *testing.T) {
	value := mustRGA(t, "author")
	insert, encoded, err := value.PrepareInsertRunBinaryWithLimits(0, "ab", frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "" {
		t.Fatalf("prepared insert changed text: %q", got)
	}
	if len(insert.NodePositions()) != 2 || len(encoded) == 0 {
		t.Fatalf("prepared insert = %d positions, %d bytes", len(insert.NodePositions()), len(encoded))
	}
	if err := value.ApplyDelta(insert); err != nil {
		t.Fatal(err)
	}
	deleteDelta, _, err := value.PrepareDeleteRunBinaryWithLimits(0, 1, frame.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "ab" {
		t.Fatalf("prepared delete changed text: %q", got)
	}
	if err := value.ApplyDelta(deleteDelta); err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "b" {
		t.Fatalf("applied prepared delete text: %q", got)
	}
}

func TestCompositionClockTagsAdvanceSafely(t *testing.T) {
	value := mustRGA(t, "author")
	first, err := value.NextTag()
	if err != nil {
		t.Fatal(err)
	}
	if !first.Valid() {
		t.Fatal("NextTag returned invalid tag")
	}
	remote := crdt.Tag{ReplicaID: "remote", WallTime: first.WallTime + 1}
	if err := value.WitnessTag(remote); err != nil {
		t.Fatal(err)
	}
	next, err := value.NextTag()
	if err != nil {
		t.Fatal(err)
	}
	if next.Compare(remote) <= 0 {
		t.Fatalf("next tag %#v did not follow witnessed remote %#v", next, remote)
	}
}
