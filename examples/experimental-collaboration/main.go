// Command experimental-collaboration demonstrates bounded framed replication
// for LWW-Map, RGA, and OR-Tree after an application has authenticated and
// negotiated the experimental protocol policy for its replication group.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/lww"
	"github.com/DarkInno/crdt/text"
	"github.com/DarkInno/crdt/tree"
)

var receiveLimits = frame.DecoderLimits{
	MaxFrameBytes:  4 << 10,
	MaxPayload:     3 << 10,
	MaxCodecID:     128,
	MaxElements:    128,
	MaxTags:        256,
	MaxStringBytes: 512,
}

var textLimits = text.Options{
	MaxNodes:        64,
	MaxTombstones:   64,
	MaxPendingNodes: 16,
	MaxPendingBytes: 1024,
}

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(writer io.Writer) error {
	// In a real application, only construct this policy after the peer has
	// authenticated an identical replication manifest and protocol version.
	policy := crdt.ProtocolPolicy{AllowExperimental: true}
	assignee, err := replicateAssignee(policy)
	if err != nil {
		return fmt.Errorf("replicate assignee: %w", err)
	}
	note, err := replicateText(policy)
	if err != nil {
		return fmt.Errorf("replicate text: %w", err)
	}
	nodeCount, err := replicateAssetTree(policy)
	if err != nil {
		return fmt.Errorf("replicate asset tree: %w", err)
	}
	fmt.Fprintf(writer, "assignee=%s\nnote=%s\nasset-tree-nodes=%d\n", assignee, note, nodeCount)
	return nil
}

func replicateAssignee(policy crdt.ProtocolPolicy) (string, error) {
	if !policy.SupportsFrame(crdt.TypeIDLWWMapDelta) {
		return "", fmt.Errorf("LWW-Map is not enabled by the replication policy")
	}
	writer, err := lww.NewMap("coordinator")
	if err != nil {
		return "", err
	}
	reader, err := lww.NewMap("dashboard")
	if err != nil {
		return "", err
	}
	delta, err := writer.SetWithDelta("pump-42", []byte("west"))
	if err != nil {
		return "", err
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		return "", err
	}
	received, err := lww.UnmarshalMapDeltaWithLimits(encoded, receiveLimits)
	if err != nil {
		return "", err
	}
	if err := reader.ApplyDelta(received); err != nil {
		return "", err
	}
	value, ok := reader.Get("pump-42")
	if !ok {
		return "", fmt.Errorf("assignee missing after accepted delta")
	}
	return string(value), nil
}

func replicateText(policy crdt.ProtocolPolicy) (string, error) {
	if !policy.SupportsFrame(crdt.TypeIDRGADelta) {
		return "", fmt.Errorf("RGA is not enabled by the replication policy")
	}
	writer, err := text.NewWithOptions("editor-a", textLimits)
	if err != nil {
		return "", err
	}
	reader, err := text.NewWithOptions("editor-b", textLimits)
	if err != nil {
		return "", err
	}
	delta, err := writer.Insert(0, "inspect pump")
	if err != nil {
		return "", err
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		return "", err
	}
	received, err := text.UnmarshalRGADeltaWithLimits(encoded, receiveLimits)
	if err != nil {
		return "", err
	}
	if err := reader.ApplyDelta(received); err != nil {
		return "", err
	}
	if reader.PendingCount() != 0 {
		return "", fmt.Errorf("complete delta unexpectedly has pending dependencies")
	}
	return reader.String(), nil
}

func replicateAssetTree(policy crdt.ProtocolPolicy) (int, error) {
	if !policy.SupportsFrame(crdt.TypeIDORTreeDelta) {
		return 0, fmt.Errorf("OR-Tree is not enabled by the replication policy")
	}
	writer, err := tree.New("asset-service")
	if err != nil {
		return 0, err
	}
	reader, err := tree.New("dashboard")
	if err != nil {
		return 0, err
	}
	_, delta, err := writer.Add(tree.NodeID{}, []byte("pump-42"))
	if err != nil {
		return 0, err
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		return 0, err
	}
	received, err := tree.UnmarshalDeltaWithLimits(encoded, receiveLimits)
	if err != nil {
		return 0, err
	}
	if err := reader.ApplyDelta(received); err != nil {
		return 0, err
	}
	return len(reader.Nodes()), nil
}
