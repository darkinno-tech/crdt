package text

import (
	"testing"

	"github.com/darkinno-tech/crdt"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/replica"
)

func TestRGARunV2DefaultManifestDeliversRealDelta(t *testing.T) {
	manifest, err := replica.NewManifest("document", "example.com/text/run-v2", 1, replica.Protocol{
		StateID: crdt.TypeIDRGARunState, DeltaID: crdt.TypeIDRGARunDelta, SemanticsVersion: 2,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	target := mustRGA(t, "target")
	frontier, err := replica.NewFrontier(nil)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := replica.NewInbox(manifest, frontier, 8, frame.DefaultLimits().MaxFrameBytes, func(encoded []byte) error {
		delta, err := UnmarshalRGARunDelta(encoded)
		if err != nil {
			return err
		}
		return target.ApplyDelta(delta)
	})
	if err != nil {
		t.Fatal(err)
	}

	source := mustRGA(t, "source")
	delta := mustInsertRGA(t, source, 0, "default run protocol")
	encoded, err := delta.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	change, err := replica.NewChange(manifest, replica.Dot{Actor: "source", Counter: 1}, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if delivery, err := inbox.Receive(change); err != nil || len(delivery.Applied) != 1 {
		t.Fatalf("Receive() = %#v, %v", delivery, err)
	}
	if _, err := inbox.Receive(change); err != nil {
		t.Fatalf("duplicate Receive() = %v", err)
	}
	if got, want := target.String(), source.String(); got != want {
		t.Fatalf("target text = %q, want %q", got, want)
	}
}
