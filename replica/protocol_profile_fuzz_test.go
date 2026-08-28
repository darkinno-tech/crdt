package replica

import (
	"testing"

	"github.com/im10furry/crdt"
)

func FuzzProtocolFromFrameType(f *testing.F) {
	for _, registration := range crdt.RegisteredFrameTypes() {
		f.Add(registration.StateID, registration.DeltaID, registration.SemanticsVersion, registration.UsesHLC, "")
	}
	f.Add(uint64(0), uint64(0), uint64(0), false, "")
	f.Add(crdt.TypeIDGCounterState, crdt.TypeIDGCounterDelta, crdt.SemanticsVersionGCounter, true, "")

	f.Fuzz(func(t *testing.T, stateID, deltaID, semanticsVersion uint64, usesHLC bool, codecID string) {
		frameType := crdt.FrameType{
			StateID:          stateID,
			DeltaID:          deltaID,
			SemanticsVersion: semanticsVersion,
			UsesHLC:          usesHLC,
		}
		protocol, err := ProtocolFromFrameType(frameType, codecID)
		if err != nil {
			return
		}
		registered, ok := crdt.FrameTypeForState(stateID)
		if !ok || registered != frameType || protocol.StateID != stateID || protocol.DeltaID != deltaID || protocol.SemanticsVersion != semanticsVersion || protocol.CodecID != codecID {
			t.Fatalf("accepted non-canonical protocol: frame=%#v protocol=%#v registered=%#v ok=%v", frameType, protocol, registered, ok)
		}
	})
}
