package main

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/replica"
	"github.com/DarkInno/crdt/text"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestRun(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatal(err)
	}
	const want = "assignee=west\nnote=inspect pump\nasset-tree-nodes=1\n"
	if output.String() != want {
		t.Fatalf("run() output = %q, want %q", output.String(), want)
	}
}

func TestRunReturnsWriteFailure(t *testing.T) {
	if err := run(failingWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("run() error = %v, want wrapped %v", err, io.ErrClosedPipe)
	}
}

// TestThreeEditorReplicationSimulation exercises the example's complete RGA
// delivery boundary. The transport duplicates frames and chooses a repeatable
// arbitrary order, so the test covers both per-actor Inbox buffering and RGA
// parent-dependency buffering before every replica converges.
func TestThreeEditorReplicationSimulation(t *testing.T) {
	policy := crdt.ProtocolPolicy{AllowExperimental: true}
	builder := newTextBuilder(t, policy)

	alice := newTestRGA(t, "alice")
	base, err := alice.Insert(0, "Draft")
	if err != nil {
		t.Fatal(err)
	}
	bob := newTestRGA(t, "bob")
	carol := newTestRGA(t, "carol")
	for _, editor := range []*text.RGA{bob, carol} {
		if err := editor.ApplyDelta(base); err != nil {
			t.Fatal(err)
		}
	}
	bobEdit, err := bob.Insert(5, " B")
	if err != nil {
		t.Fatal(err)
	}
	carolEdit, err := carol.Insert(5, " C")
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []text.Delta{bobEdit, carolEdit} {
		if err := alice.ApplyDelta(change); err != nil {
			t.Fatal(err)
		}
	}
	deleteEdit, err := alice.Delete(1, 2)
	if err != nil {
		t.Fatal(err)
	}

	changes := []replica.Change{
		mustTextChange(t, builder, replica.Dot{Actor: "alice", Counter: 1}, base),
		mustTextChange(t, builder, replica.Dot{Actor: "bob", Counter: 1}, bobEdit),
		mustTextChange(t, builder, replica.Dot{Actor: "carol", Counter: 1}, carolEdit),
		mustTextChange(t, builder, replica.Dot{Actor: "alice", Counter: 2}, deleteEdit),
	}
	want := alice.String()
	for seed := int64(1); seed <= 24; seed++ {
		for index, replicaID := range []string{"alice-receiver", "bob-receiver", "carol-receiver"} {
			receiver := newTestRGA(t, replicaID)
			inbox := newTextInbox(t, builder, receiver)
			deliverTextSimulation(t, inbox, changes, seed*100+int64(index))
			if got := receiver.String(); got != want {
				t.Fatalf("seed %d %s text = %q, want %q", seed, replicaID, got, want)
			}
			if pending := receiver.PendingCount(); pending != 0 {
				t.Fatalf("seed %d %s retained %d RGA dependencies", seed, replicaID, pending)
			}
			if pending, _ := inbox.Pending(); pending != 0 {
				t.Fatalf("seed %d %s retained %d transport changes", seed, replicaID, pending)
			}
			frontier := inbox.Frontier()
			if frontier.Counter("alice") != 2 || frontier.Counter("bob") != 1 || frontier.Counter("carol") != 1 {
				t.Fatalf("seed %d %s frontier = %#v", seed, replicaID, frontier.Entries())
			}
		}
	}
}

func newTextBuilder(t testing.TB, policy crdt.ProtocolPolicy) replica.SessionBuilder {
	t.Helper()
	builder, err := replica.NewSessionBuilder("field-note", "example.com/field-note/v1", 1, replica.Protocol{
		StateID:          crdt.TypeIDRGAState,
		DeltaID:          crdt.TypeIDRGADelta,
		SemanticsVersion: 1,
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	return builder
}

func newTestRGA(t testing.TB, replicaID string) *text.RGA {
	t.Helper()
	value, err := text.NewWithOptions(replicaID, textLimits)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustTextChange(t testing.TB, builder replica.SessionBuilder, dot replica.Dot, delta text.Delta) replica.Change {
	t.Helper()
	change, err := newTextChange(builder, dot, delta)
	if err != nil {
		t.Fatal(err)
	}
	return change
}

func newTextInbox(t testing.TB, builder replica.SessionBuilder, target *text.RGA) *replica.Inbox {
	t.Helper()
	frontier, err := replica.NewFrontier(nil)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := builder.NewInbox(frontier, 8, 8*receiveLimits.MaxFrameBytes, func(encoded []byte) error {
		delta, err := text.UnmarshalRGADeltaWithLimits(encoded, receiveLimits)
		if err != nil {
			return err
		}
		return target.ApplyDelta(delta)
	})
	if err != nil {
		t.Fatal(err)
	}
	return inbox
}

func deliverTextSimulation(t testing.TB, inbox *replica.Inbox, changes []replica.Change, seed int64) {
	t.Helper()
	frames := make([]replica.Change, 0, len(changes)*2)
	for _, change := range changes {
		frames = append(frames, change, change)
	}
	random := rand.New(rand.NewSource(seed))
	random.Shuffle(len(frames), func(left, right int) { frames[left], frames[right] = frames[right], frames[left] })
	for _, change := range frames {
		if _, err := inbox.Receive(change); err != nil {
			t.Fatal(err)
		}
	}
}
