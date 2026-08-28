// Command intent-first-setup shows how to select a CRDT by business intent,
// derive its canonical manifest protocol, then perform bounded delivery. It
// remains a local teaching example: authentication, authorization, durable
// outboxes, and production limits belong to the host application.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/counter"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/replica"
)

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
	profile, ok := crdt.ReplicationProfileFor("counter/grow-only")
	if !ok {
		return fmt.Errorf("load grow-only-counter profile")
	}
	builder, err := replica.NewSessionBuilderForFrameType(
		"warehouse-completions",
		"example.com/warehouse-completions/v1",
		1,
		profile.FrameType,
		"",
		crdt.ProtocolPolicy{},
	)
	if err != nil {
		return fmt.Errorf("build manifest: %w", err)
	}

	writerReplica, err := counter.NewGCounter("warehouse-a")
	if err != nil {
		return fmt.Errorf("create writer: %w", err)
	}
	receiverReplica, err := counter.NewGCounter("warehouse-b")
	if err != nil {
		return fmt.Errorf("create receiver: %w", err)
	}
	delta, err := writerReplica.Increment(3)
	if err != nil {
		return fmt.Errorf("increment writer: %w", err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		return fmt.Errorf("encode delta: %w", err)
	}
	change, err := builder.NewChange(replica.Dot{Actor: "warehouse-a", Counter: 1}, encoded)
	if err != nil {
		return fmt.Errorf("validate manifest-bound change: %w", err)
	}
	if err := receive(receiverReplica, change.Delta()); err != nil {
		return err
	}
	if err := receive(receiverReplica, change.Delta()); err != nil { // A retry is idempotent.
		return err
	}
	value, err := receiverReplica.Value()
	if err != nil {
		return fmt.Errorf("read receiver: %w", err)
	}
	manifest := builder.Manifest()
	if _, err := fmt.Fprintf(writer, "profile=%s\nstate_type=%d\ndelta_type=%d\nvalue=%d\n", profile.ID, manifest.Protocol.StateID, manifest.Protocol.DeltaID, value); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return nil
}

func receive(target *counter.GCounter, encoded []byte) error {
	delta, err := counter.UnmarshalGCounterDeltaWithLimits(encoded, receiveLimits)
	if err != nil {
		return fmt.Errorf("decode bounded G-Counter delta: %w", err)
	}
	if err := target.ApplyDelta(delta); err != nil {
		return fmt.Errorf("apply G-Counter delta: %w", err)
	}
	return nil
}
