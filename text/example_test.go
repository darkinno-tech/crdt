package text

import (
	"fmt"

	"github.com/darkinno-tech/crdt"
	frame "github.com/darkinno-tech/crdt/encoding"
)

// ExampleRGA_ApplyDelta shows a bounded RGA delivery. RGA frames are
// experimental: enabling local support is separate from authenticating peers
// and agreeing the manifest for a replication group.
func ExampleRGA_ApplyDelta() {
	policy := crdt.ProtocolPolicy{AllowExperimental: true}
	if !policy.SupportsFrame(crdt.TypeIDRGADelta) {
		panic("RGA must be enabled by the replication-group policy")
	}

	options := Options{
		MaxNodes:        64,
		MaxTombstones:   64,
		MaxPendingNodes: 16,
		MaxPendingBytes: 4 << 10,
	}
	writer, err := NewWithOptions("writer", options)
	if err != nil {
		panic(err)
	}
	reader, err := NewWithOptions("reader", options)
	if err != nil {
		panic(err)
	}

	delta, err := writer.Insert(0, "field note")
	if err != nil {
		panic(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		panic(err)
	}
	received, err := UnmarshalRGADeltaWithLimits(encoded, frame.DecoderLimits{
		MaxFrameBytes:  4 << 10,
		MaxPayload:     3 << 10,
		MaxCodecID:     128,
		MaxElements:    64,
		MaxTags:        64,
		MaxStringBytes: 256,
	})
	if err != nil {
		panic(err)
	}
	if err := reader.ApplyDelta(received); err != nil {
		panic(err)
	}

	fmt.Println(reader.String())
	// Output: field note
}
