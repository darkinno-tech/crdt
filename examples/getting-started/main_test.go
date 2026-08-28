package main

import (
	"bytes"
	"errors"
	"math/rand"
	"reflect"
	"testing"

	"github.com/darkinno-tech/crdt/counter"
	frame "github.com/darkinno-tech/crdt/encoding"
)

func TestRun(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatal(err)
	}
	const want = "left=5\nright=5\nconverged=true\n"
	if got := output.String(); got != want {
		t.Fatalf("run output = %q, want %q", got, want)
	}
}

func TestReceiveCounterRejectsOverBudgetFrameWithoutMutation(t *testing.T) {
	source := mustNewGCounter(t, "source")
	target := mustNewGCounter(t, "target")
	if _, err := target.Increment(4); err != nil {
		t.Fatal(err)
	}
	delta, err := source.Increment(3)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	limits := receiveLimits
	limits.MaxFrameBytes = len(encoded) - 1

	if err := receiveCounter(target, encoded, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("receiveCounter() error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if got, err := target.Value(); err != nil || got != 4 {
		t.Fatalf("target Value() = %d, %v; want 4, nil", got, err)
	}
}

func TestThreeReplicaDuplicateAndReorderedDeliverySimulation(t *testing.T) {
	for seed := int64(1); seed <= 16; seed++ {
		replicas, deliveries := simulationFixture(t)
		deliveries = append(deliveries, deliveries...)
		random := rand.New(rand.NewSource(seed))
		for _, target := range replicas {
			for _, index := range random.Perm(len(deliveries)) {
				if err := receiveCounter(target, deliveries[index], receiveLimits); err != nil {
					t.Fatalf("seed %d delivery %d: %v", seed, index, err)
				}
			}
		}

		for _, replica := range replicas {
			if got, err := replica.Value(); err != nil || got != 16 {
				t.Fatalf("seed %d Value() = %d, %v; want 16, nil", seed, got, err)
			}
			if got, want := replica.Counts(), map[string]uint64{"north": 3, "south": 5, "west": 8}; !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d Counts() = %#v, want %#v", seed, got, want)
			}
		}
	}
}

func BenchmarkReceiveCounterDuplicate(b *testing.B) {
	source := mustNewGCounter(b, "source")
	target := mustNewGCounter(b, "target")
	delta, err := source.Increment(1)
	if err != nil {
		b.Fatal(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		b.Fatal(err)
	}
	if err := receiveCounter(target, encoded, receiveLimits); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := receiveCounter(target, encoded, receiveLimits); err != nil {
			b.Fatal(err)
		}
	}
}

func simulationFixture(tb testing.TB) ([]*counter.GCounter, [][]byte) {
	tb.Helper()
	north := mustNewGCounter(tb, "north")
	south := mustNewGCounter(tb, "south")
	west := mustNewGCounter(tb, "west")

	mutations := []struct {
		counter *counter.GCounter
		amount  uint64
	}{
		{counter: north, amount: 1},
		{counter: south, amount: 2},
		{counter: north, amount: 2},
		{counter: west, amount: 5},
		{counter: south, amount: 3},
		{counter: west, amount: 3},
	}
	deliveries := make([][]byte, 0, len(mutations))
	for _, mutation := range mutations {
		delta, err := mutation.counter.Increment(mutation.amount)
		if err != nil {
			tb.Fatal(err)
		}
		encoded, err := delta.MarshalBinary()
		if err != nil {
			tb.Fatal(err)
		}
		deliveries = append(deliveries, encoded)
	}
	return []*counter.GCounter{north, south, west}, deliveries
}

func mustNewGCounter(tb testing.TB, replicaID string) *counter.GCounter {
	tb.Helper()
	value, err := counter.NewGCounter(replicaID)
	if err != nil {
		tb.Fatal(err)
	}
	return value
}
