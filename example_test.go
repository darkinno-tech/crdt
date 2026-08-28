package crdt_test

import (
	"fmt"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/counter"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/lww"
	"github.com/darkinno-tech/crdt/register"
	"github.com/darkinno-tech/crdt/set"
	"github.com/darkinno-tech/crdt/text"
	"github.com/darkinno-tech/crdt/tree"
)

func ExampleGCounter() {
	left, err := counter.NewGCounter("left")
	if err != nil {
		panic(err)
	}
	right, err := counter.NewGCounter("right")
	if err != nil {
		panic(err)
	}
	if _, err := left.Increment(2); err != nil {
		panic(err)
	}
	if _, err := right.Increment(3); err != nil {
		panic(err)
	}
	if err := left.Merge(right); err != nil {
		panic(err)
	}

	value, err := left.Value()
	if err != nil {
		panic(err)
	}
	fmt.Println(value)
	// Output: 5
}

func ExamplePNCounter() {
	left, err := counter.NewPNCounter("left")
	if err != nil {
		panic(err)
	}
	right, err := counter.NewPNCounter("right")
	if err != nil {
		panic(err)
	}
	if _, err := left.Increment(7); err != nil {
		panic(err)
	}
	if _, err := right.Decrement(2); err != nil {
		panic(err)
	}
	if err := left.Merge(right); err != nil {
		panic(err)
	}

	value, err := left.Value()
	if err != nil {
		panic(err)
	}
	fmt.Println(value)
	// Output: 5
}

func ExampleORSet() {
	codec := exampleStringCodec{}
	left, err := set.NewORSet("left", codec)
	if err != nil {
		panic(err)
	}
	right, err := set.NewORSet("right", codec)
	if err != nil {
		panic(err)
	}
	delta, err := left.Add("item")
	if err != nil {
		panic(err)
	}
	if err := right.ApplyDelta(delta); err != nil {
		panic(err)
	}

	fmt.Println(right.Contains("item"))
	// Output: true
}

func ExampleGSet() {
	codec := exampleStringCodec{}
	writer, err := set.NewGSet("warehouse-a", codec)
	if err != nil {
		panic(err)
	}
	reader, err := set.NewGSet("warehouse-b", codec)
	if err != nil {
		panic(err)
	}

	delta, err := writer.Add("filter-17")
	if err != nil {
		panic(err)
	}
	encoded, err := delta.MarshalBinary(codec)
	if err != nil {
		panic(err)
	}
	received, err := set.UnmarshalGSetDelta(encoded, codec)
	if err != nil {
		panic(err)
	}
	if err := reader.ApplyDelta(received); err != nil {
		panic(err)
	}
	if err := reader.ApplyDelta(received); err != nil { // Duplicate delivery is harmless.
		panic(err)
	}

	fmt.Println(reader.Contains("filter-17"))
	// Output: true
}

func ExampleMVRegister() {
	west, err := register.NewMVRegister("west")
	if err != nil {
		panic(err)
	}
	east, err := register.NewMVRegister("east")
	if err != nil {
		panic(err)
	}
	dashboard, err := register.NewMVRegister("dashboard")
	if err != nil {
		panic(err)
	}

	westDelta, err := west.Set([]byte("maintenance"))
	if err != nil {
		panic(err)
	}
	eastDelta, err := east.Set([]byte("inspection-required"))
	if err != nil {
		panic(err)
	}
	for _, delta := range []register.MVRegisterDelta{westDelta, eastDelta} {
		encoded, err := delta.MarshalBinary()
		if err != nil {
			panic(err)
		}
		received, err := register.UnmarshalMVRegisterDelta(encoded)
		if err != nil {
			panic(err)
		}
		if err := dashboard.ApplyDelta(received); err != nil {
			panic(err)
		}
	}

	for _, value := range dashboard.Values() {
		fmt.Println(string(value.Value))
	}
	// Output:
	// inspection-required
	// maintenance
}

func Example_lwwSet() {
	writer, err := lww.NewSet[string]("writer")
	if err != nil {
		panic(err)
	}
	reader, err := lww.NewSet[string]("reader")
	if err != nil {
		panic(err)
	}
	if err := writer.Add("on-call"); err != nil {
		panic(err)
	}
	if err := reader.Merge(writer); err != nil {
		panic(err)
	}

	fmt.Println(reader.Contains("on-call"))
	// Output: true
}

func Example_lwwRegister() {
	writer, err := register.NewLWW("writer")
	if err != nil {
		panic(err)
	}
	reader, err := register.NewLWW("reader")
	if err != nil {
		panic(err)
	}
	if err := writer.Set([]byte("healthy")); err != nil {
		panic(err)
	}
	if err := reader.Merge(writer); err != nil {
		panic(err)
	}

	value, ok := reader.Get()
	fmt.Println(ok, string(value))
	// Output: true healthy
}

func Example_maxRegister() {
	local := register.NewMax()
	remote := register.NewMax()
	if err := local.Set(8); err != nil {
		panic(err)
	}
	if err := remote.Set(13); err != nil {
		panic(err)
	}
	if err := local.Merge(remote); err != nil {
		panic(err)
	}

	value, ok := local.Get()
	fmt.Println(ok, value)
	// Output: true 13
}

func ExampleProtocolPolicy() {
	stable := crdt.ProtocolPolicy{}
	compatibility := crdt.ProtocolPolicy{AllowExperimental: true}

	fmt.Println(stable.SupportsFrame(crdt.TypeIDRGAState))
	fmt.Println(compatibility.SupportsFrame(crdt.TypeIDRGAState))
	// Output:
	// true
	// true
}

func ExampleRGA() {
	policy := crdt.ProtocolPolicy{}
	if !policy.SupportsFrame(crdt.TypeIDRGADelta) {
		panic("RGA frame type must be implemented")
	}
	options := text.Options{MaxNodes: 64, MaxTombstones: 64, MaxPendingNodes: 16, MaxPendingBytes: 1024}
	writer, err := text.NewWithOptions("writer", options)
	if err != nil {
		panic(err)
	}
	reader, err := text.NewWithOptions("reader", options)
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
	received, err := text.UnmarshalRGADeltaWithLimits(encoded, boundedExampleLimits())
	if err != nil {
		panic(err)
	}
	if err := reader.ApplyDelta(received); err != nil {
		panic(err)
	}

	fmt.Println(reader.String())
	// Output: field note
}

func ExampleORTree() {
	policy := crdt.ProtocolPolicy{}
	if !policy.SupportsFrame(crdt.TypeIDORTreeDelta) {
		panic("OR-Tree must be enabled by the authenticated replication-group policy")
	}
	writer, err := tree.New("writer")
	if err != nil {
		panic(err)
	}
	reader, err := tree.New("reader")
	if err != nil {
		panic(err)
	}

	_, delta, err := writer.Add(tree.NodeID{}, []byte("pump-42"))
	if err != nil {
		panic(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		panic(err)
	}
	received, err := tree.UnmarshalDeltaWithLimits(encoded, boundedExampleLimits())
	if err != nil {
		panic(err)
	}
	if err := reader.ApplyDelta(received); err != nil {
		panic(err)
	}

	nodes := reader.Nodes()
	fmt.Println(string(nodes[0].Value))
	// Output: pump-42
}

func boundedExampleLimits() frame.DecoderLimits {
	return frame.DecoderLimits{
		MaxFrameBytes:  4 << 10,
		MaxPayload:     3 << 10,
		MaxCodecID:     128,
		MaxElements:    128,
		MaxTags:        256,
		MaxStringBytes: 512,
	}
}

type exampleStringCodec struct{}

func (exampleStringCodec) ID() string                            { return "example.com/string/v1" }
func (exampleStringCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (exampleStringCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }
