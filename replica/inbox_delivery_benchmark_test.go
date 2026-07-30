package replica

import (
	"testing"

	"github.com/DarkInno/crdt"
)

// BenchmarkNewManifestRegistryValidation measures the pre-I/O admission check
// that binds one state/delta pair to its generated semantic version. It covers
// both scalar-v1 and run-v2 RGA contracts so a future registry growth cannot
// silently turn negotiated group creation into an allocation-heavy path.
func BenchmarkNewManifestRegistryValidation(b *testing.B) {
	protocols := []Protocol{
		{StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: crdt.SemanticsVersionGCounter},
		{StateID: crdt.TypeIDRGAState, DeltaID: crdt.TypeIDRGADelta, SemanticsVersion: crdt.SemanticsVersionRGA},
		{StateID: crdt.TypeIDRGARunState, DeltaID: crdt.TypeIDRGARunDelta, SemanticsVersion: crdt.SemanticsVersionRGARun},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := NewManifest("benchmark", "example.com/benchmark/v1", 1, protocols[index%len(protocols)], crdt.ProtocolPolicy{}); err != nil {
			b.Fatal(err)
		}
	}
}

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

// BenchmarkInboxReceiveBufferedDuplicate covers duplicate detection while a
// future change remains queued. This is the hot path that compares delta
// bytes to distinguish an idempotent resend from a conflicting dot.
func BenchmarkInboxReceiveBufferedDuplicate(b *testing.B) {
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
	inbox, err := NewInbox(manifest, frontier, 2, 1<<20, func([]byte) error { return nil })
	if err != nil {
		b.Fatal(err)
	}
	change, err := NewChange(manifest, Dot{Actor: "writer", Counter: 2}, mustFramePayload(b, crdt.TypeIDGCounterDelta, "", make([]byte, 4096)))
	if err != nil {
		b.Fatal(err)
	}
	if delivery, err := inbox.Receive(change); err != nil || !delivery.Buffered {
		b.Fatalf("initial buffering = %#v, %v", delivery, err)
	}
	b.SetBytes(int64(len(change.delta)))
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		delivery, err := inbox.Receive(change)
		if err != nil || !delivery.Buffered || !delivery.Duplicate {
			b.Fatalf("buffered duplicate = %#v, %v", delivery, err)
		}
	}
}
