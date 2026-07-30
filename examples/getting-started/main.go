// Command getting-started is the smallest complete replication flow for a
// stable CRDT: mutate locally, encode for an outbox, decode untrusted bytes
// within a budget, then apply idempotently at the receiver.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/DarkInno/crdt/counter"
	frame "github.com/DarkInno/crdt/encoding"
)

// receiveLimits are intentionally small so the example makes the resource
// boundary visible. Select production values from the authenticated transport
// body limit and the replication group's expected cardinality.
var receiveLimits = frame.DecoderLimits{
	MaxFrameBytes:  4 << 10,
	MaxPayload:     3 << 10,
	MaxCodecID:     128,
	MaxElements:    64,
	MaxTags:        64,
	MaxStringBytes: 256,
}

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(writer io.Writer) error {
	left, err := counter.NewGCounter("warehouse-a")
	if err != nil {
		return fmt.Errorf("create left replica: %w", err)
	}
	right, err := counter.NewGCounter("warehouse-b")
	if err != nil {
		return fmt.Errorf("create right replica: %w", err)
	}

	leftDelta, err := left.Increment(2)
	if err != nil {
		return fmt.Errorf("increment left replica: %w", err)
	}
	rightDelta, err := right.Increment(3)
	if err != nil {
		return fmt.Errorf("increment right replica: %w", err)
	}

	// A network may repeat or reorder an outbox record. The duplicate left
	// delivery is intentional: the receiver still reaches the same join.
	for _, delivery := range []struct {
		target *counter.GCounter
		delta  counter.GCounterDelta
	}{
		{target: right, delta: leftDelta},
		{target: left, delta: rightDelta},
		{target: right, delta: leftDelta},
	} {
		if err := deliverCounter(delivery.target, delivery.delta); err != nil {
			return err
		}
	}

	leftValue, err := left.Value()
	if err != nil {
		return fmt.Errorf("read left replica: %w", err)
	}
	rightValue, err := right.Value()
	if err != nil {
		return fmt.Errorf("read right replica: %w", err)
	}
	if _, err := fmt.Fprintf(writer, "left=%d\nright=%d\nconverged=%t\n", leftValue, rightValue, leftValue == rightValue); err != nil {
		return fmt.Errorf("write getting-started result: %w", err)
	}
	return nil
}

func deliverCounter(target *counter.GCounter, delta counter.GCounterDelta) error {
	encoded, err := delta.MarshalBinary()
	if err != nil {
		return fmt.Errorf("encode G-Counter delta: %w", err)
	}
	return receiveCounter(target, encoded, receiveLimits)
}

// receiveCounter is the application-facing receive boundary. Authentication,
// authorization, and the transport body limit must happen before this point.
// The delta is fully validated against limits before ApplyDelta can mutate
// target state.
func receiveCounter(target *counter.GCounter, encoded []byte, limits frame.DecoderLimits) error {
	delta, err := counter.UnmarshalGCounterDeltaWithLimits(encoded, limits)
	if err != nil {
		return fmt.Errorf("decode G-Counter delta: %w", err)
	}
	if err := target.ApplyDelta(delta); err != nil {
		return fmt.Errorf("apply G-Counter delta: %w", err)
	}
	return nil
}
