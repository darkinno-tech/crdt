package replica

import (
	"testing"

	"github.com/DarkInno/crdt"
)

func BenchmarkInboxReceiveInstalledDuplicate(b *testing.B) {
	manifest, err := NewManifest("counter", "example.com/counter/v1", 1, Protocol{
		StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		b.Fatal(err)
	}
	frontier, err := NewFrontier(nil)
	if err != nil {
		b.Fatal(err)
	}
	inbox, err := NewInbox(manifest, frontier, 1, 1024, func([]byte) error { return nil })
	if err != nil {
		b.Fatal(err)
	}
	change, err := NewChange(manifest, Dot{Actor: "writer", Counter: 1}, mustFramePayload(b, crdt.TypeIDGCounterDelta, "", []byte{1}))
	if err != nil {
		b.Fatal(err)
	}
	if _, err := inbox.Receive(change); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		delivery, err := inbox.Receive(change)
		if err != nil || !delivery.Duplicate || delivery.Accepted() {
			b.Fatalf("duplicate delivery = %#v, %v", delivery, err)
		}
	}
}
