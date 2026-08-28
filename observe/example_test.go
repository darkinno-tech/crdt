package observe_test

import (
	"fmt"

	"github.com/im10furry/crdt/counter"
	"github.com/im10furry/crdt/observe"
)

func ExampleStore() {
	value, err := counter.NewGCounter("browser-tab")
	if err != nil {
		panic(err)
	}
	store, err := observe.New(value, func(current *counter.GCounter) uint64 {
		total, err := current.Value()
		if err != nil {
			panic(err)
		}
		return total
	})
	if err != nil {
		panic(err)
	}

	rendered := make(chan uint64, 2)
	subscription, err := store.Subscribe(func(event observe.Event[uint64]) {
		rendered <- event.Value
	})
	if err != nil {
		panic(err)
	}
	<-rendered // Initial view; a UI can render it immediately.
	if err := store.Mutate(observe.Local, func(current *counter.GCounter) error {
		_, err := current.Increment(4)
		return err
	}); err != nil {
		panic(err)
	}

	fmt.Println(<-rendered)
	subscription.Unsubscribe()
	<-subscription.Done()
	// Output: 4
}
