package documenttree

import (
	"github.com/im10furry/crdt"
	frame "github.com/im10furry/crdt/encoding"
)

// The v1 identifiers remain permanently reserved. They are intentionally not
// registered in crdt.ProtocolPolicy: v2 peers must not join a v1 group.
const (
	legacyDocumentTreeStateTypeID uint64 = 27
	legacyDocumentTreeDeltaTypeID uint64 = 28
)

// MigrateV1State converts one complete v1 state frame into a complete v2
// frame without changing its object graph or HLC tags. It is an offline,
// one-time migration tool, not a replication decoder. A v1 frame containing
// its former lazy-reference value is rejected: the referenced content was not
// in the old frame, so treating it as a fully nested value would lose data.
func MigrateV1State(data []byte, options Options, limits frame.DecoderLimits) ([]byte, error) {
	state, err := unmarshalState(data, legacyDocumentTreeStateTypeID, options, limits, false)
	if err != nil {
		return nil, err
	}
	return marshalState(crdt.TypeIDDocumentTreeState, state, options, limits, false)
}

// MigrateV1Delta converts one bounded v1 delta into a v2 delta. It is safe
// only for a controlled cutover where every participant has stopped sending
// v1 frames; hosts must deliver the resulting v2 frame to a v2 replication
// group with the same authenticated schema and compatible limits.
func MigrateV1Delta(data []byte, options Options, limits frame.DecoderLimits) ([]byte, error) {
	state, err := unmarshalState(data, legacyDocumentTreeDeltaTypeID, options, limits, true)
	if err != nil {
		return nil, err
	}
	return marshalState(crdt.TypeIDDocumentTreeDelta, state, options, limits, true)
}
