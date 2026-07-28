package crdt_test

import (
	"fmt"

	"github.com/DarkInno/crdt/counter"
	"github.com/DarkInno/crdt/set"
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

type exampleStringCodec struct{}

func (exampleStringCodec) ID() string                            { return "example.com/string/v1" }
func (exampleStringCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (exampleStringCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }
