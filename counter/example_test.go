package counter

import (
	"fmt"

	frame "github.com/DarkInno/crdt/encoding"
)

// ExampleGCounter_ApplyDelta shows the local-mutation and bounded-receive
// split. Authenticate and apply the transport body limit before handing bytes
// to the decoder.
func ExampleGCounter_ApplyDelta() {
	writer, err := NewGCounter("warehouse-a")
	if err != nil {
		panic(err)
	}
	reader, err := NewGCounter("warehouse-b")
	if err != nil {
		panic(err)
	}

	delta, err := writer.Increment(2)
	if err != nil {
		panic(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		panic(err)
	}

	limits := frame.DecoderLimits{
		MaxFrameBytes:  4 << 10,
		MaxPayload:     3 << 10,
		MaxCodecID:     128,
		MaxElements:    64,
		MaxTags:        64,
		MaxStringBytes: 256,
	}
	received, err := UnmarshalGCounterDeltaWithLimits(encoded, limits)
	if err != nil {
		panic(err)
	}
	if err := reader.ApplyDelta(received); err != nil {
		panic(err)
	}
	if err := reader.ApplyDelta(received); err != nil { // A retry is idempotent.
		panic(err)
	}

	value, err := reader.Value()
	if err != nil {
		panic(err)
	}
	fmt.Println(value)
	// Output: 2
}
