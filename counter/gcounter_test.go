package counter

import (
	"errors"
	"math"
	"math/rand"
	"reflect"
	"sync"
	"testing"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
)

func TestNewGCounterRejectsInvalidReplicaID(t *testing.T) {
	t.Parallel()

	if _, err := NewGCounter(" "); !errors.Is(err, ErrInvalidReplicaID) {
		t.Fatalf("NewGCounter() error = %v, want %v", err, ErrInvalidReplicaID)
	}
}

func TestGCounterIncrementAndApplyDelta(t *testing.T) {
	t.Parallel()

	source := mustNewGCounter(t, "source")
	target := mustNewGCounter(t, "target")

	delta, err := source.Increment(7)
	if err != nil {
		t.Fatalf("Increment() error = %v", err)
	}
	if err := target.ApplyDelta(delta); err != nil {
		t.Fatalf("ApplyDelta() error = %v", err)
	}
	if err := target.ApplyDelta(delta); err != nil {
		t.Fatalf("duplicate ApplyDelta() error = %v", err)
	}

	if got, err := target.Value(); err != nil || got != 7 {
		t.Fatalf("Value() = %d, %v; want 7, nil", got, err)
	}
	if got, want := target.Counts(), map[string]uint64{"source": 7}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Counts() = %#v, want %#v", got, want)
	}
}

func TestGCounterMergeIsCommutativeAssociativeAndIdempotent(t *testing.T) {
	t.Parallel()

	a := counterWithValue(t, "a", 2)
	b := counterWithValue(t, "b", 3)
	c := counterWithValue(t, "c", 5)

	left := cloneCounter(t, a)
	if err := left.Merge(b); err != nil {
		t.Fatalf("left.Merge(b) error = %v", err)
	}
	right := cloneCounter(t, b)
	if err := right.Merge(a); err != nil {
		t.Fatalf("right.Merge(a) error = %v", err)
	}
	assertSameCounts(t, left, right)

	associativeLeft := cloneCounter(t, a)
	if err := associativeLeft.Merge(b); err != nil {
		t.Fatalf("first associative Merge() error = %v", err)
	}
	if err := associativeLeft.Merge(c); err != nil {
		t.Fatalf("second associative Merge() error = %v", err)
	}

	mergedRight := cloneCounter(t, b)
	if err := mergedRight.Merge(c); err != nil {
		t.Fatalf("mergedRight.Merge(c) error = %v", err)
	}
	associativeRight := cloneCounter(t, a)
	if err := associativeRight.Merge(mergedRight); err != nil {
		t.Fatalf("associativeRight.Merge() error = %v", err)
	}
	assertSameCounts(t, associativeLeft, associativeRight)

	before := cloneCounter(t, associativeLeft)
	if err := associativeLeft.Merge(associativeLeft); err != nil {
		t.Fatalf("self Merge() error = %v", err)
	}
	assertSameCounts(t, associativeLeft, before)
}

func TestGCounterRejectsOverflowWithoutMutation(t *testing.T) {
	t.Parallel()

	counter := mustNewGCounter(t, "a")
	if _, err := counter.Increment(math.MaxUint64); err != nil {
		t.Fatalf("Increment(max) error = %v", err)
	}
	if _, err := counter.Increment(1); !errors.Is(err, ErrCounterOverflow) {
		t.Fatalf("Increment() error = %v, want %v", err, ErrCounterOverflow)
	}
	if got, err := counter.Value(); err != nil || got != math.MaxUint64 {
		t.Fatalf("Value() = %d, %v; want max uint64, nil", got, err)
	}
}

func TestGCounterValueRejectsAggregateOverflow(t *testing.T) {
	t.Parallel()

	counter := mustNewGCounter(t, "a")
	if err := counter.ApplyDelta(GCounterDelta{counts: map[string]uint64{"a": math.MaxUint64, "b": 1}}); err != nil {
		t.Fatalf("ApplyDelta() error = %v", err)
	}
	if _, err := counter.Value(); !errors.Is(err, ErrCounterOverflow) {
		t.Fatalf("Value() error = %v, want %v", err, ErrCounterOverflow)
	}
}

func TestGCounterCountsReturnsCopy(t *testing.T) {
	t.Parallel()

	counter := counterWithValue(t, "a", 2)
	counts := counter.Counts()
	counts["a"] = 99

	if got, want := counter.Counts()["a"], uint64(2); got != want {
		t.Fatalf("Counts copy modified internal state: got %d, want %d", got, want)
	}
}

func TestGCounterBinaryRoundTripIsDeterministicAndAtomic(t *testing.T) {
	t.Parallel()
	source := mustNewGCounter(t, "a")
	if _, err := source.Increment(2); err != nil {
		t.Fatal(err)
	}
	if err := source.ApplyDelta(GCounterDelta{counts: map[string]uint64{"b": 3}}); err != nil {
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
	if !reflect.DeepEqual(first, second) {
		t.Fatal("MarshalBinary is non-deterministic")
	}
	target := mustNewGCounter(t, "target")
	if err := target.UnmarshalBinary(first); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(target.Counts(), source.Counts()) {
		t.Fatalf("round trip = %#v", target.Counts())
	}
	before := target.Counts()
	first[len(first)-1] ^= 1
	if err := target.UnmarshalBinary(first); err == nil {
		t.Fatal("corrupt frame accepted")
	}
	if !reflect.DeepEqual(target.Counts(), before) {
		t.Fatal("failed decode modified state")
	}
}

func TestGCounterDeltaBinaryRoundTripAndTypeIsolation(t *testing.T) {
	t.Parallel()
	source := mustNewGCounter(t, "source")
	delta, err := source.Increment(7)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalGCounterDelta(encoded)
	if err != nil {
		t.Fatal(err)
	}
	target := mustNewGCounter(t, "target")
	if err := target.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
	if got, err := target.Value(); err != nil || got != 7 {
		t.Fatalf("Value() = %d, %v; want 7, nil", got, err)
	}
	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalGCounterDelta(state); err == nil {
		t.Fatal("state frame accepted as delta")
	}
}

func TestGCounterMarshalRejectsMoreElementsThanItsDecoderAllows(t *testing.T) {
	t.Parallel()
	limits := frame.DefaultLimits()
	limits.MaxElements = 1
	if _, err := marshalCountsWithLimits(crdt.TypeIDGCounterState, map[string]uint64{"a": 1, "b": 2}, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("marshalCountsWithLimits() error = %v, want %v", err, frame.ErrFrameLimit)
	}
}

func TestGCounterMarshalRejectsPayloadBeyondConfiguredLimit(t *testing.T) {
	t.Parallel()
	limits := frame.DefaultLimits()
	limits.MaxPayload = 3
	if _, err := marshalCountsWithLimits(crdt.TypeIDGCounterState, map[string]uint64{"a": 1}, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("marshalCountsWithLimits() error = %v, want %v", err, frame.ErrFrameLimit)
	}
}

func TestGCounterBinaryHonorsCallerLimitsWithoutMutation(t *testing.T) {
	t.Parallel()
	source := mustNewGCounter(t, "a")
	if err := source.ApplyDelta(GCounterDelta{counts: map[string]uint64{"a": 1, "b": 2}}); err != nil {
		t.Fatal(err)
	}
	encoded, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	target := counterWithValue(t, "target", 9)
	before := target.Counts()
	limits := frame.DefaultLimits()
	limits.MaxElements = 1
	if err := target.UnmarshalBinaryWithLimits(encoded, limits); err == nil {
		t.Fatal("over-limit state accepted")
	}
	if got := target.Counts(); !reflect.DeepEqual(got, before) {
		t.Fatalf("over-limit decode modified receiver: got %#v, want %#v", got, before)
	}
}

func TestGCounterDecodeRejectsImpossibleCountBeforeAllocation(t *testing.T) {
	t.Parallel()
	payload := frame.AppendUvarint(nil, 1<<20)
	encoded, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDGCounterState, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	target := counterWithValue(t, "target", 9)
	before := target.Counts()
	if err := target.UnmarshalBinary(encoded); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("impossible count error = %v", err)
	}
	if got := target.Counts(); !reflect.DeepEqual(got, before) {
		t.Fatalf("impossible count changed receiver: got %#v, want %#v", got, before)
	}
	if _, _, err := unmarshalPNCountsSection(payload, 0, 1<<20, frame.DefaultLimits()); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("PN impossible count error = %v", err)
	}
}

func TestGCounterIncrementIsConcurrentSafe(t *testing.T) {
	t.Parallel()

	counter := mustNewGCounter(t, "a")
	const increments = 100
	var group sync.WaitGroup
	errs := make(chan error, increments)
	for i := 0; i < increments; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := counter.Increment(1)
			if err != nil {
				errs <- err
			}
		}()
	}
	group.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("Increment() error = %v", err)
	}
	if got, err := counter.Value(); err != nil || got != increments {
		t.Fatalf("Value() = %d, %v; want %d, nil", got, err, increments)
	}
}

func TestGCounterMergePropertiesRandomized(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(42))
	for iteration := 0; iteration < 128; iteration++ {
		a := counterWithValue(t, "a", uint64(rng.Intn(1_000)))
		b := counterWithValue(t, "b", uint64(rng.Intn(1_000)))
		c := counterWithValue(t, "c", uint64(rng.Intn(1_000)))

		left := cloneCounter(t, a)
		if err := left.Merge(b); err != nil {
			t.Fatal(err)
		}
		if err := left.Merge(c); err != nil {
			t.Fatal(err)
		}

		right := cloneCounter(t, c)
		if err := right.Merge(a); err != nil {
			t.Fatal(err)
		}
		if err := right.Merge(b); err != nil {
			t.Fatal(err)
		}
		assertSameCounts(t, left, right)
		before := left.Counts()
		if err := left.Merge(left); err != nil {
			t.Fatal(err)
		}
		if got := left.Counts(); !reflect.DeepEqual(got, before) {
			t.Fatalf("iteration %d: self merge = %#v, want %#v", iteration, got, before)
		}
	}
}

func FuzzGCounterUnmarshalBinary(f *testing.F) {
	source, err := NewGCounter("seed")
	if err != nil {
		f.Fatal(err)
	}
	if _, err := source.Increment(1); err != nil {
		f.Fatal(err)
	}
	seed, err := source.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte("CRDT"))
	f.Fuzz(func(t *testing.T, data []byte) {
		target := mustNewGCounter(t, "target")
		if err := target.UnmarshalBinary(data); err == nil {
			if _, err := target.Value(); err != nil && !errors.Is(err, ErrCounterOverflow) {
				t.Fatalf("decoded counter returned unexpected value error: %v", err)
			}
		}
		if delta, err := UnmarshalGCounterDelta(data); err == nil {
			if err := target.ApplyDelta(delta); err != nil {
				t.Fatalf("valid decoded delta rejected: %v", err)
			}
		}
	})
}

func mustNewGCounter(t *testing.T, replicaID string) *GCounter {
	t.Helper()

	counter, err := NewGCounter(replicaID)
	if err != nil {
		t.Fatalf("NewGCounter(%q) error = %v", replicaID, err)
	}
	return counter
}

func counterWithValue(t *testing.T, replicaID string, value uint64) *GCounter {
	t.Helper()

	counter := mustNewGCounter(t, replicaID)
	if _, err := counter.Increment(value); err != nil {
		t.Fatalf("Increment(%d) error = %v", value, err)
	}
	return counter
}

func cloneCounter(t *testing.T, source *GCounter) *GCounter {
	t.Helper()

	clone := mustNewGCounter(t, source.replicaID)
	if err := clone.ApplyDelta(GCounterDelta{counts: source.Counts()}); err != nil {
		t.Fatalf("clone ApplyDelta() error = %v", err)
	}
	return clone
}

func assertSameCounts(t *testing.T, left, right *GCounter) {
	t.Helper()

	if got, want := left.Counts(), right.Counts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Counts() = %#v, want %#v", got, want)
	}
}
