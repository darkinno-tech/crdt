package main

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/counter"
	"github.com/DarkInno/crdt/replica"
)

func TestRun(t *testing.T) {
	var output bytes.Buffer
	if err := run(&output); err != nil {
		t.Fatal(err)
	}
	const want = "profile=counter/grow-only\nstate_type=1\ndelta_type=3\nvalue=3\n"
	if got := output.String(); got != want {
		t.Fatalf("run output = %q, want %q", got, want)
	}
}

func TestGrowOnlyProfileThreeReplicaDuplicateAndReorderedDeliverySimulation(t *testing.T) {
	profile, ok := crdt.ReplicationProfileFor("counter/grow-only")
	if !ok {
		t.Fatal("missing grow-only profile")
	}
	builder, err := replica.NewSessionBuilderForFrameType("simulation", "example.com/simulation/v1", 1, profile.FrameType, "", crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	replicas := map[string]*counter.GCounter{}
	for _, id := range []string{"north", "south", "west"} {
		value, err := counter.NewGCounter(id)
		if err != nil {
			t.Fatal(err)
		}
		replicas[id] = value
	}

	deliveries := make([][]byte, 0, 6)
	actorCounters := make(map[string]uint64, len(replicas))
	for _, mutation := range []struct {
		actor  string
		amount uint64
	}{
		{actor: "north", amount: 1},
		{actor: "south", amount: 2},
		{actor: "north", amount: 4},
		{actor: "west", amount: 8},
		{actor: "south", amount: 16},
		{actor: "west", amount: 32},
	} {
		delta, err := replicas[mutation.actor].Increment(mutation.amount)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := delta.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		actorCounters[mutation.actor]++
		change, err := builder.NewChange(replica.Dot{Actor: mutation.actor, Counter: actorCounters[mutation.actor]}, encoded)
		if err != nil {
			t.Fatal(err)
		}
		deliveries = append(deliveries, change.Delta())
	}

	deliveries = append(deliveries, deliveries...)
	for seed := int64(1); seed <= 16; seed++ {
		for _, target := range replicas {
			order := rand.New(rand.NewSource(seed)).Perm(len(deliveries))
			for _, index := range order {
				if err := receive(target, deliveries[index]); err != nil {
					t.Fatalf("seed %d delivery %d: %v", seed, index, err)
				}
			}
			if got, err := target.Value(); err != nil || got != 63 {
				t.Fatalf("seed %d value = %d, %v; want 63, nil", seed, got, err)
			}
		}
	}
}

func BenchmarkGrowOnlyProfileBoundedDuplicateDelivery(b *testing.B) {
	profile, ok := crdt.ReplicationProfileFor("counter/grow-only")
	if !ok {
		b.Fatal("missing grow-only profile")
	}
	builder, err := replica.NewSessionBuilderForFrameType("benchmark", "example.com/benchmark/v1", 1, profile.FrameType, "", crdt.ProtocolPolicy{})
	if err != nil {
		b.Fatal(err)
	}
	writer, err := counter.NewGCounter("writer")
	if err != nil {
		b.Fatal(err)
	}
	target, err := counter.NewGCounter("target")
	if err != nil {
		b.Fatal(err)
	}
	delta, err := writer.Increment(1)
	if err != nil {
		b.Fatal(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		b.Fatal(err)
	}
	change, err := builder.NewChange(replica.Dot{Actor: "writer", Counter: 1}, encoded)
	if err != nil {
		b.Fatal(err)
	}
	if err := receive(target, change.Delta()); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := receive(target, change.Delta()); err != nil {
			b.Fatal(err)
		}
	}
}
