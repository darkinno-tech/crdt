package observe_test

import (
	"testing"

	"github.com/darkinno-tech/crdt/counter"
	"github.com/darkinno-tech/crdt/observe"
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

// BenchmarkGCounterObserverRemoteApply measures one decoded remote delta at a
// reactive target. It is a controlled in-process baseline, not transport,
// browser paint, persistence, or production-capacity evidence.
func BenchmarkGCounterObserverRemoteApply(b *testing.B) {
	source, err := observe.NewGCounterObserver("benchmark-source")
	if err != nil {
		b.Fatal(err)
	}
	target, err := observe.NewGCounterObserver("benchmark-target")
	if err != nil {
		b.Fatal(err)
	}
	subscription, err := target.Subscribe(func(observe.Event[observe.GCounterView]) {})
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
		delta, err := source.Increment(1)
		if err != nil {
			b.Fatal(err)
		}
		if changed, err := target.ApplyDelta(delta); err != nil || !changed {
			b.Fatalf("ApplyDelta() = %t, %v", changed, err)
		}
	}
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
