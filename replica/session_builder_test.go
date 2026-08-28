package replica

import (
	"testing"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/text"
)

type builderCheckpointStore struct{ checkpoint Checkpoint }

func (s *builderCheckpointStore) SaveCheckpoint(checkpoint Checkpoint) error {
	s.checkpoint = checkpoint
	return nil
}

func TestSessionBuilderBindsStableScalarRGAAcrossLifecycle(t *testing.T) {
	policy := crdt.ProtocolPolicy{}
	builder, err := NewSessionBuilder("notes", "example.com/notes/v1", 1, Protocol{
		StateID:          crdt.TypeIDRGAState,
		DeltaID:          crdt.TypeIDRGADelta,
		SemanticsVersion: 1,
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	manifest := builder.Manifest()
	if _, err := NewSessionBuilderFromManifest(manifest, crdt.ProtocolPolicy{}); err != nil {
		t.Fatalf("zero-policy builder = %v", err)
	}

	writer, err := text.New("writer")
	if err != nil {
		t.Fatal(err)
	}
	delta, err := writer.Insert(0, "inspection note")
	if err != nil {
		t.Fatal(err)
	}
	encodedDelta, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	change, err := builder.NewChange(Dot{Actor: "writer", Counter: 1}, encodedDelta)
	if err != nil {
		t.Fatal(err)
	}

	frontier, err := NewFrontier(nil)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := text.New("reader")
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := builder.NewInbox(frontier, 4, 4<<10, func(data []byte) error {
		decoded, err := text.UnmarshalRGADelta(data)
		if err != nil {
			return err
		}
		return reader.ApplyDelta(decoded)
	})
	if err != nil {
		t.Fatal(err)
	}
	if delivery, err := inbox.Receive(change); err != nil || len(delivery.Applied) != 1 || reader.String() != "inspection note" {
		t.Fatalf("builder inbox delivery = %#v, %v, text=%q", delivery, err, reader.String())
	}

	state, clockState, err := writer.MarshalBinaryWithClockState()
	if err != nil {
		t.Fatal(err)
	}
	checkpointFrontier, err := NewFrontier(map[string]uint64{"writer": 1})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := builder.NewCheckpoint(state, checkpointFrontier, clockState, func(data []byte) error {
		candidate, err := text.New("validator")
		if err != nil {
			return err
		}
		return candidate.UnmarshalBinary(data)
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := builder.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	store := &builderCheckpointStore{}
	if err := session.Install(checkpoint, store); err != nil {
		t.Fatal(err)
	}
	if store.checkpoint.ID() != checkpoint.ID() {
		t.Fatalf("stored checkpoint ID = %x, want %x", store.checkpoint.ID(), checkpoint.ID())
	}
	if acknowledgement, err := session.Acknowledge(); err != nil || acknowledgement.GroupID != manifest.GroupID || acknowledgement.Epoch != manifest.Epoch || acknowledgement.Frontier.Counter("writer") != 1 {
		t.Fatalf("builder session acknowledgement = %#v, %v", acknowledgement, err)
	}
}

func TestSessionBuilderRetainsStableDefault(t *testing.T) {
	builder, err := NewSessionBuilder("counter", "example.com/counter/v1", 1, Protocol{
		StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := builder.NewSession(); err != nil {
		t.Fatalf("stable builder session = %v", err)
	}
}
