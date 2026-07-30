// Command collaborative-board demonstrates how an application can use CRDT
// deltas for a field-maintenance workboard while replicas are disconnected.
package main

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/DarkInno/crdt/counter"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/set"
)

// receiveLimits are an example receive budget, not a production capacity
// recommendation. An application must derive these limits from its own
// authenticated transport and resource budget.
var receiveLimits = frame.DecoderLimits{
	MaxFrameBytes:  4 << 10,
	MaxPayload:     3 << 10,
	MaxCodecID:     128,
	MaxElements:    64,
	MaxTags:        128,
	MaxStringBytes: 256,
}

type taskCodec struct{}

func (taskCodec) ID() string                            { return "example.com/maintenance-task/v1" }
func (taskCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (taskCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(writer io.Writer) error {
	completed, err := completedInspections()
	if err != nil {
		return fmt.Errorf("calculate completed inspections: %w", err)
	}
	tasks, err := openTasksAfterRecovery()
	if err != nil {
		return fmt.Errorf("recover workboard: %w", err)
	}
	if _, err := fmt.Fprintf(writer, "completed-inspections=%d\nopen-tasks=%v\n", completed, tasks); err != nil {
		return fmt.Errorf("write collaborative board: %w", err)
	}
	return nil
}

func completedInspections() (uint64, error) {
	north, err := counter.NewGCounter("technician-north")
	if err != nil {
		return 0, err
	}
	south, err := counter.NewGCounter("technician-south")
	if err != nil {
		return 0, err
	}
	dashboard, err := counter.NewGCounter("operations-dashboard")
	if err != nil {
		return 0, err
	}
	northDelta, err := north.Increment(2)
	if err != nil {
		return 0, err
	}
	southDelta, err := south.Increment(3)
	if err != nil {
		return 0, err
	}
	// Delivery is intentionally out of order and the north delta is duplicated.
	for _, change := range []counter.GCounterDelta{southDelta, northDelta, northDelta} {
		if err := deliverCounter(dashboard, change); err != nil {
			return 0, err
		}
	}
	return dashboard.Value()
}

func openTasksAfterRecovery() ([]string, error) {
	codec := taskCodec{}
	dispatch, err := set.NewORSet("dispatch", codec)
	if err != nil {
		return nil, err
	}
	field, err := set.NewORSet("field-van-7", codec)
	if err != nil {
		return nil, err
	}

	inspection, err := dispatch.Add("inspect-pump")
	if err != nil {
		return nil, err
	}
	if err := deliverORSet(field, inspection, codec); err != nil {
		return nil, err
	}

	// The van is now partitioned. It adds one task and removes the observed
	// inspection while dispatch independently reopens that inspection.
	replacement, err := field.Add("replace-filter")
	if err != nil {
		return nil, err
	}
	completed, err := field.Remove("inspect-pump")
	if err != nil {
		return nil, err
	}
	reopened, err := dispatch.Add("inspect-pump")
	if err != nil {
		return nil, err
	}

	// The link returns. Applying the same encoded delta twice is harmless.
	for _, change := range []set.ORSetDelta[string]{completed, replacement} {
		if err := deliverORSet(dispatch, change, codec); err != nil {
			return nil, err
		}
	}
	if err := deliverORSet(field, reopened, codec); err != nil {
		return nil, err
	}
	if err := deliverORSet(field, reopened, codec); err != nil {
		return nil, err
	}

	// SnapshotCurrentState retains the local HLC state, so the recovered field
	// replica can safely retain its logical replica ID.
	saved, err := field.SnapshotCurrentState()
	if err != nil {
		return nil, err
	}
	recovered, err := set.NewORSetFromSnapshot(saved, codec)
	if err != nil {
		return nil, err
	}
	shiftClose, err := recovered.Add("close-shift")
	if err != nil {
		return nil, err
	}
	if err := deliverORSet(dispatch, shiftClose, codec); err != nil {
		return nil, err
	}

	tasks := dispatch.Elements()
	sort.Strings(tasks)
	return tasks, nil
}

func deliverCounter(target *counter.GCounter, delta counter.GCounterDelta) error {
	encoded, err := delta.MarshalBinary()
	if err != nil {
		return err
	}
	decoded, err := counter.UnmarshalGCounterDeltaWithLimits(encoded, receiveLimits)
	if err != nil {
		return err
	}
	return target.ApplyDelta(decoded)
}

func deliverORSet(target *set.ORSet[string], delta set.ORSetDelta[string], codec taskCodec) error {
	encoded, err := delta.MarshalBinary(codec)
	if err != nil {
		return err
	}
	decoded, err := set.UnmarshalORSetDeltaWithLimits(encoded, codec, receiveLimits)
	if err != nil {
		return err
	}
	return target.ApplyDelta(decoded)
}
