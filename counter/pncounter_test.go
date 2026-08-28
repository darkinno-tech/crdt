package counter

import (
	"errors"
	"math"
	"math/big"
	"math/rand"
	"reflect"
	"sync"
	"testing"

	"github.com/darkinno-tech/crdt"
	frame "github.com/darkinno-tech/crdt/encoding"
)

func TestPNCounterDeltaDeliveryAndMerge(t *testing.T) {
	t.Parallel()

	left := mustNewPNCounter(t, "left")
	right := mustNewPNCounter(t, "right")
	observer := mustNewPNCounter(t, "observer")

	leftIncrement := mustPNIncrement(t, left, 7)
	leftDecrement := mustPNDecrement(t, left, 2)
	rightIncrement := mustPNIncrement(t, right, 3)
	rightDecrement := mustPNDecrement(t, right, 5)
	for _, delta := range []PNCounterDelta{rightDecrement, leftIncrement, leftDecrement, leftIncrement, rightIncrement} {
		if err := observer.ApplyDelta(delta); err != nil {
			t.Fatal(err)
		}
	}
	assertPNValue(t, observer, big.NewInt(3))
	if got, want := observer.PositiveCounts(), map[string]uint64{"left": 7, "right": 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("PositiveCounts() = %#v, want %#v", got, want)
	}
	if got, want := observer.NegativeCounts(), map[string]uint64{"left": 2, "right": 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("NegativeCounts() = %#v, want %#v", got, want)
	}

	merged := mustClonePNCounter(t, left)
	if err := merged.Merge(right); err != nil {
		t.Fatal(err)
	}
	assertSamePNCounts(t, merged, observer)
	beforePositive, beforeNegative := merged.PositiveCounts(), merged.NegativeCounts()
	if err := merged.Merge(merged); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(merged.PositiveCounts(), beforePositive) || !reflect.DeepEqual(merged.NegativeCounts(), beforeNegative) {
		t.Fatal("self merge changed PN-Counter")
	}
}

func TestPNCounterValueBoundsAndCopies(t *testing.T) {
	t.Parallel()

	counter := mustNewPNCounter(t, "a")
	if _, err := counter.Increment(math.MaxUint64); err != nil {
		t.Fatal(err)
	}
	if _, err := counter.Increment(1); !errors.Is(err, ErrCounterOverflow) {
		t.Fatalf("overflow increment error = %v, want %v", err, ErrCounterOverflow)
	}
	if _, err := counter.ValueInt64(); !errors.Is(err, ErrCounterOverflow) {
		t.Fatalf("ValueInt64() error = %v, want %v", err, ErrCounterOverflow)
	}
	assertPNValue(t, counter, new(big.Int).SetUint64(math.MaxUint64))
	if _, err := counter.Decrement(math.MaxUint64); err != nil {
		t.Fatal(err)
	}
	if got, err := counter.ValueInt64(); err != nil || got != 0 {
		t.Fatalf("ValueInt64() = %d, %v; want 0, nil", got, err)
	}
	value, err := counter.Value()
	if err != nil {
		t.Fatal(err)
	}
	value.SetInt64(42)
	assertPNValue(t, counter, big.NewInt(0))

	positive := counter.PositiveCounts()
	positive["a"] = 1
	negative := counter.NegativeCounts()
	negative["a"] = 1
	assertPNValue(t, counter, big.NewInt(0))
	if _, err := counter.Decrement(1); !errors.Is(err, ErrCounterOverflow) {
		t.Fatalf("overflow decrement error = %v, want %v", err, ErrCounterOverflow)
	}
}

func TestPNCounterMergePropertiesRandomized(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewSource(42))
	for iteration := 0; iteration < 128; iteration++ {
		a := randomPNCounter(t, rng, "a")
		b := randomPNCounter(t, rng, "b")
		c := randomPNCounter(t, rng, "c")

		left := mustClonePNCounter(t, a)
		for _, other := range []*PNCounter{b, c} {
			if err := left.Merge(other); err != nil {
				t.Fatal(err)
			}
		}
		right := mustClonePNCounter(t, c)
		for _, other := range []*PNCounter{a, b} {
			if err := right.Merge(other); err != nil {
				t.Fatal(err)
			}
		}
		assertSamePNCounts(t, left, right)
		beforePositive, beforeNegative := left.PositiveCounts(), left.NegativeCounts()
		if err := left.Merge(left); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(left.PositiveCounts(), beforePositive) || !reflect.DeepEqual(left.NegativeCounts(), beforeNegative) {
			t.Fatalf("iteration %d: self merge changed PN-Counter", iteration)
		}
	}
}

func TestPNCounterMergeTakesMaximumForEachComponent(t *testing.T) {
	t.Parallel()

	lower := mustNewPNCounter(t, "same-replica")
	upper := mustNewPNCounter(t, "same-replica")
	if _, err := lower.Increment(2); err != nil {
		t.Fatal(err)
	}
	if _, err := lower.Decrement(4); err != nil {
		t.Fatal(err)
	}
	if _, err := upper.Increment(5); err != nil {
		t.Fatal(err)
	}
	if _, err := upper.Decrement(1); err != nil {
		t.Fatal(err)
	}
	if err := lower.Merge(upper); err != nil {
		t.Fatal(err)
	}
	if got, want := lower.PositiveCounts(), map[string]uint64{"same-replica": 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("positive merge = %#v, want %#v", got, want)
	}
	if got, want := lower.NegativeCounts(), map[string]uint64{"same-replica": 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("negative merge = %#v, want %#v", got, want)
	}
	assertPNValue(t, lower, big.NewInt(1))
}

func TestPNCounterNilStateAndLimitPaths(t *testing.T) {
	t.Parallel()

	if _, err := NewPNCounter(" "); !errors.Is(err, ErrInvalidReplicaID) {
		t.Fatalf("NewPNCounter() error = %v, want %v", err, ErrInvalidReplicaID)
	}
	var nilCounter *PNCounter
	if _, err := nilCounter.Increment(1); !errors.Is(err, ErrNilPNCounter) {
		t.Fatalf("nil Increment() error = %v", err)
	}
	if _, err := nilCounter.Decrement(1); !errors.Is(err, ErrNilPNCounter) {
		t.Fatalf("nil Decrement() error = %v", err)
	}
	if _, err := nilCounter.Value(); !errors.Is(err, ErrNilPNCounter) {
		t.Fatalf("nil Value() error = %v", err)
	}
	if _, err := nilCounter.ValueInt64(); !errors.Is(err, ErrNilPNCounter) {
		t.Fatalf("nil ValueInt64() error = %v", err)
	}
	if nilCounter.PositiveCounts() != nil || nilCounter.NegativeCounts() != nil {
		t.Fatal("nil counter returned component maps")
	}
	if got := nilCounter.State(); got.Type != "pncounter" {
		t.Fatalf("nil State() = %#v", got)
	}
	if _, err := nilCounter.MarshalBinary(); !errors.Is(err, ErrNilPNCounter) {
		t.Fatalf("nil MarshalBinary() error = %v", err)
	}
	if err := nilCounter.UnmarshalBinary(nil); !errors.Is(err, ErrNilPNCounter) {
		t.Fatalf("nil UnmarshalBinary() error = %v", err)
	}
	if err := nilCounter.Merge(nil); !errors.Is(err, ErrNilPNCounter) {
		t.Fatalf("nil Merge() error = %v", err)
	}
	if err := nilCounter.ApplyDelta(PNCounterDelta{}); !errors.Is(err, ErrNilPNCounter) {
		t.Fatalf("nil ApplyDelta() error = %v", err)
	}

	counter := mustNewPNCounter(t, "a")
	if got := counter.State(); got.Type != "pncounter" || got.ReplicaID != "a" || got.ElementCount != 0 {
		t.Fatalf("State() = %#v", got)
	}
	delta := PNCounterDelta{positive: map[string]uint64{"a": 2}, negative: map[string]uint64{"a": 1}}
	if got, want := delta.PositiveCounts(), map[string]uint64{"a": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("delta PositiveCounts() = %#v, want %#v", got, want)
	}
	if got, want := delta.NegativeCounts(), map[string]uint64{"a": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("delta NegativeCounts() = %#v, want %#v", got, want)
	}
	if err := counter.ApplyDelta(delta); err != nil {
		t.Fatal(err)
	}
	if got := counter.State(); got.ElementCount != 2 {
		t.Fatalf("State().ElementCount = %d, want 2", got.ElementCount)
	}
	invalid := PNCounterDelta{positive: map[string]uint64{" ": 1}}
	if _, err := invalid.MarshalBinary(); !errors.Is(err, ErrInvalidReplicaID) {
		t.Fatalf("invalid delta marshal error = %v", err)
	}
	if _, err := invalid.Merge(delta); !errors.Is(err, ErrInvalidReplicaID) {
		t.Fatalf("invalid delta merge error = %v", err)
	}

	limits := frame.DefaultLimits()
	limits.MaxElements = 1
	if _, err := marshalPNCountsWithLimits(crdt.TypeIDPNCounterState, delta.positive, delta.negative, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("element limit error = %v, want %v", err, frame.ErrFrameLimit)
	}
	limits = frame.DefaultLimits()
	limits.MaxStringBytes = 0
	if _, err := marshalPNCountsWithLimits(crdt.TypeIDPNCounterState, delta.positive, nil, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("string limit error = %v, want %v", err, frame.ErrFrameLimit)
	}
	limits = frame.DefaultLimits()
	limits.MaxPayload = 1
	if _, err := marshalPNCountsWithLimits(crdt.TypeIDPNCounterState, delta.positive, nil, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("payload limit error = %v, want %v", err, frame.ErrFrameLimit)
	}
}

func TestPNCounterBinaryRoundTripAndTypeIsolation(t *testing.T) {
	t.Parallel()

	source := mustNewPNCounter(t, "a")
	if _, err := source.Increment(5); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Decrement(2); err != nil {
		t.Fatal(err)
	}
	other := mustNewPNCounter(t, "b")
	increment := mustPNIncrement(t, other, 3)
	decrement := mustPNDecrement(t, other, 7)
	if err := source.ApplyDelta(increment); err != nil {
		t.Fatal(err)
	}
	if err := source.ApplyDelta(decrement); err != nil {
		t.Fatal(err)
	}

	first, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("MarshalBinary is non-deterministic")
	}
	target := mustNewPNCounter(t, "target")
	if err := target.UnmarshalBinary(first); err != nil {
		t.Fatal(err)
	}
	assertSamePNCounts(t, target, source)
	if _, err := UnmarshalPNCounterDelta(first); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("state accepted as delta: %v", err)
	}

	encodedDelta, err := increment.Merge(decrement)
	if err != nil {
		t.Fatal(err)
	}
	deltaBytes, err := encodedDelta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decodedDelta, err := UnmarshalPNCounterDelta(deltaBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.UnmarshalBinary(deltaBytes); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("delta accepted as state: %v", err)
	}
	if err := target.ApplyDelta(decodedDelta); err != nil {
		t.Fatal(err)
	}
	assertSamePNCounts(t, target, source)
}

func TestPNCounterDecoderBoundsCanonicalOrderAndAtomicity(t *testing.T) {
	t.Parallel()

	source := mustNewPNCounter(t, "source")
	if _, err := source.Increment(1); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Decrement(1); err != nil {
		t.Fatal(err)
	}
	encoded, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	target := mustNewPNCounter(t, "target")
	if _, err := target.Increment(9); err != nil {
		t.Fatal(err)
	}
	beforePositive, beforeNegative := target.PositiveCounts(), target.NegativeCounts()
	limits := frame.DefaultLimits()
	limits.MaxElements = 1
	if err := target.UnmarshalBinaryWithLimits(encoded, limits); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("over-limit state error = %v, want %v", err, frame.ErrInvalidFrame)
	}
	if !reflect.DeepEqual(target.PositiveCounts(), beforePositive) || !reflect.DeepEqual(target.NegativeCounts(), beforeNegative) {
		t.Fatal("failed decode modified receiver")
	}

	payload := make([]byte, 0)
	payload = frame.AppendUvarint(payload, 2)
	for _, replicaID := range []string{"b", "a"} {
		payload = frame.AppendUvarint(payload, uint64(len(replicaID)))
		payload = append(payload, replicaID...)
		payload = frame.AppendUvarint(payload, 1)
	}
	payload = frame.AppendUvarint(payload, 0)
	nonCanonical, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDPNCounterState, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.UnmarshalBinary(nonCanonical); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("non-canonical state error = %v, want %v", err, frame.ErrInvalidFrame)
	}
}

func TestPNCounterConcurrentMutation(t *testing.T) {
	t.Parallel()

	counter := mustNewPNCounter(t, "a")
	const operations = 100
	var group sync.WaitGroup
	errs := make(chan error, operations*2)
	for index := 0; index < operations; index++ {
		group.Add(2)
		go func() {
			defer group.Done()
			_, err := counter.Increment(1)
			errs <- err
		}()
		go func() {
			defer group.Done()
			_, err := counter.Decrement(1)
			errs <- err
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent mutation error = %v", err)
		}
	}
	assertPNValue(t, counter, big.NewInt(0))
}

func FuzzPNCounterUnmarshalBinary(f *testing.F) {
	source, err := NewPNCounter("seed")
	if err != nil {
		f.Fatal(err)
	}
	if _, err := source.Increment(2); err != nil {
		f.Fatal(err)
	}
	if _, err := source.Decrement(1); err != nil {
		f.Fatal(err)
	}
	seed, err := source.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte("CRDT"))
	f.Fuzz(func(t *testing.T, data []byte) {
		target := mustNewPNCounter(t, "target")
		if err := target.UnmarshalBinary(data); err == nil {
			if _, err := target.Value(); err != nil {
				t.Fatalf("decoded PN-Counter Value() error = %v", err)
			}
		}
		if delta, err := UnmarshalPNCounterDelta(data); err == nil {
			if err := target.ApplyDelta(delta); err != nil {
				t.Fatalf("valid decoded delta rejected: %v", err)
			}
		}
	})
}

func mustNewPNCounter(t testing.TB, replicaID string) *PNCounter {
	t.Helper()
	counter, err := NewPNCounter(replicaID)
	if err != nil {
		t.Fatalf("NewPNCounter(%q) error = %v", replicaID, err)
	}
	return counter
}

func mustPNIncrement(t testing.TB, counter *PNCounter, amount uint64) PNCounterDelta {
	t.Helper()
	delta, err := counter.Increment(amount)
	if err != nil {
		t.Fatalf("Increment(%d) error = %v", amount, err)
	}
	return delta
}

func mustPNDecrement(t testing.TB, counter *PNCounter, amount uint64) PNCounterDelta {
	t.Helper()
	delta, err := counter.Decrement(amount)
	if err != nil {
		t.Fatalf("Decrement(%d) error = %v", amount, err)
	}
	return delta
}

func mustClonePNCounter(t testing.TB, source *PNCounter) *PNCounter {
	t.Helper()
	encoded, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	clone := mustNewPNCounter(t, source.replicaID)
	if err := clone.UnmarshalBinary(encoded); err != nil {
		t.Fatal(err)
	}
	return clone
}

func assertPNValue(t testing.TB, counter *PNCounter, want *big.Int) {
	t.Helper()
	got, err := counter.Value()
	if err != nil || got.Cmp(want) != 0 {
		t.Fatalf("Value() = %v, %v; want %v, nil", got, err, want)
	}
}

func assertSamePNCounts(t testing.TB, left, right *PNCounter) {
	t.Helper()
	if got, want := left.PositiveCounts(), right.PositiveCounts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("PositiveCounts() = %#v, want %#v", got, want)
	}
	if got, want := left.NegativeCounts(), right.NegativeCounts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("NegativeCounts() = %#v, want %#v", got, want)
	}
}

func randomPNCounter(t testing.TB, rng *rand.Rand, replicaID string) *PNCounter {
	t.Helper()
	counter := mustNewPNCounter(t, replicaID)
	for operation := 0; operation < 8; operation++ {
		amount := uint64(rng.Intn(1_000))
		var err error
		if rng.Intn(2) == 0 {
			_, err = counter.Increment(amount)
		} else {
			_, err = counter.Decrement(amount)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return counter
}
