package observe_test

import (
	"testing"

	"github.com/DarkInno/crdt/counter"
	"github.com/DarkInno/crdt/observe"
)

func BenchmarkGCounterBinding(b *testing.B) {
	b.Run("direct", func(b *testing.B) {
		value := benchmarkCounter(b, "direct")
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			if _, err := value.Increment(1); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("store_no_subscriber", func(b *testing.B) {
		value := benchmarkCounter(b, "no-subscriber")
		store := benchmarkStore(b, value)
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			if err := store.Mutate(observe.Local, func(current *counter.GCounter) error {
				_, err := current.Increment(1)
				return err
			}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("store_one_subscriber", func(b *testing.B) {
		value := benchmarkCounter(b, "one-subscriber")
		store := benchmarkStore(b, value)
		subscription, err := store.Subscribe(func(observe.Event[uint64]) {})
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() {
			subscription.Unsubscribe()
			<-subscription.Done()
		})
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			if err := store.Mutate(observe.Local, func(current *counter.GCounter) error {
				_, err := current.Increment(1)
				return err
			}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkGCounterBindingParallel(b *testing.B) {
	value := benchmarkCounter(b, "parallel")
	store := benchmarkStore(b, value)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(worker *testing.PB) {
		for worker.Next() {
			if err := store.Mutate(observe.Local, func(current *counter.GCounter) error {
				_, err := current.Increment(1)
				return err
			}); err != nil {
				b.Error(err)
			}
		}
	})
}

func benchmarkCounter(b *testing.B, replicaID string) *counter.GCounter {
	b.Helper()
	value, err := counter.NewGCounter(replicaID)
	if err != nil {
		b.Fatal(err)
	}
	return value
}

func benchmarkStore(b *testing.B, value *counter.GCounter) *observe.Store[*counter.GCounter, uint64] {
	b.Helper()
	store, err := observe.New(value, func(current *counter.GCounter) uint64 {
		total, err := current.Value()
		if err != nil {
			panic(err)
		}
		return total
	})
	if err != nil {
		b.Fatal(err)
	}
	return store
}
