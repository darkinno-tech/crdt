package delta

import (
	"errors"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/counter"
	frame "github.com/DarkInno/crdt/encoding"
)

func TestBatchRoundTripCopiesAndLimits(t *testing.T) {
	item := mustEncodedDelta(t, "a", 1)
	batch, err := NewBatch([][]byte{item}, len(item))
	if err != nil {
		t.Fatalf("NewBatch() error = %v", err)
	}
	item[0] ^= 1
	if string(batch.Items()[0]) == string(item) {
		t.Fatal("batch aliases input")
	}
	returned := batch.Items()
	returned[0][0] ^= 1
	if string(returned[0]) == string(batch.Items()[0]) {
		t.Fatal("batch aliases returned items")
	}
	encoded, err := batch.MarshalBinary(len(batch.Items()[0]))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalBatch(encoded, 1, len(batch.Items()[0]))
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Items()) != 1 || string(decoded.Items()[0]) != string(batch.Items()[0]) {
		t.Fatal("batch round trip changed item")
	}
	if _, err := NewBatch([][]byte{[]byte("not a frame")}, 32); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid frame error = %v", err)
	}
	if _, err := NewBatch([][]byte{batch.Items()[0]}, len(batch.Items()[0])-1); !errors.Is(err, ErrLimit) {
		t.Fatalf("limit error = %v", err)
	}
}

func TestBatchMarshalBinaryRetainsInternalValidation(t *testing.T) {
	batch := Batch{items: [][]byte{[]byte("not a frame")}}
	if _, err := batch.MarshalBinary(32); !errors.Is(err, ErrInvalid) {
		t.Fatalf("MarshalBinary() error = %v, want %v", err, ErrInvalid)
	}
	if _, err := (Batch{}).MarshalBinary(0); !errors.Is(err, ErrLimit) {
		t.Fatalf("MarshalBinary() limit error = %v, want %v", err, ErrLimit)
	}
}

func TestCoalescerJoinsWithoutGrowingQueue(t *testing.T) {
	source, err := counter.NewGCounter("source")
	if err != nil {
		t.Fatal(err)
	}
	firstDelta, err := source.Increment(1)
	if err != nil {
		t.Fatal(err)
	}
	secondDelta, err := source.Increment(2)
	if err != nil {
		t.Fatal(err)
	}
	first, err := firstDelta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	second, err := secondDelta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	coalescer, err := NewCoalescer(2, len(first)+len(second), mergeGCounter)
	if err != nil {
		t.Fatal(err)
	}
	if err := coalescer.Add(first); err != nil {
		t.Fatal(err)
	}
	if err := coalescer.Add(second); err != nil {
		t.Fatal(err)
	}
	if items, _ := coalescer.Len(); items != 1 {
		t.Fatalf("Len() items = %d, want 1", items)
	}
	batch := coalescer.Drain()
	decoded, err := counter.UnmarshalGCounterDelta(batch.Items()[0])
	if err != nil {
		t.Fatal(err)
	}
	target, err := counter.NewGCounter("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
	if got, err := target.Value(); err != nil || got != 3 {
		t.Fatalf("coalesced value = %d, %v; want 3, nil", got, err)
	}
}

func TestBatchAndCoalescerAcceptPNCounterDeltas(t *testing.T) {
	source, err := counter.NewPNCounter("source")
	if err != nil {
		t.Fatal(err)
	}
	firstDelta, err := source.Increment(5)
	if err != nil {
		t.Fatal(err)
	}
	secondDelta, err := source.Decrement(2)
	if err != nil {
		t.Fatal(err)
	}
	first, err := firstDelta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	second, err := secondDelta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	batch, err := NewBatch([][]byte{first, second}, len(first)+len(second))
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Items()) != 2 {
		t.Fatalf("batch item count = %d, want 2", len(batch.Items()))
	}
	coalescer, err := NewCoalescer(2, len(first)+len(second), mergePNCounter)
	if err != nil {
		t.Fatal(err)
	}
	if err := coalescer.Add(first); err != nil {
		t.Fatal(err)
	}
	if err := coalescer.Add(second); err != nil {
		t.Fatal(err)
	}
	items := coalescer.Drain().Items()
	if len(items) != 1 {
		t.Fatalf("coalesced item count = %d, want 1", len(items))
	}
	merged, err := counter.UnmarshalPNCounterDelta(items[0])
	if err != nil {
		t.Fatal(err)
	}
	target, err := counter.NewPNCounter("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(merged); err != nil {
		t.Fatal(err)
	}
	if got, err := target.ValueInt64(); err != nil || got != 3 {
		t.Fatalf("coalesced PN-Counter value = %d, %v; want 3, nil", got, err)
	}
}

func TestBatchAdmitsImplementedLWWSetDeltaType(t *testing.T) {
	item, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDLWWSetDelta})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewBatch([][]byte{item}, len(item)); err != nil {
		t.Fatalf("NewBatch() error = %v", err)
	}
}

func FuzzUnmarshalBatch(f *testing.F) {
	seed := mustEncodedDelta(f, "seed", 1)
	batch, err := NewBatch([][]byte{seed}, len(seed))
	if err != nil {
		f.Fatal(err)
	}
	encoded, err := batch.MarshalBinary(len(seed))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(encoded)
	f.Add([]byte(batchMagic))
	f.Fuzz(func(t *testing.T, data []byte) {
		batch, err := UnmarshalBatch(data, 64, 1<<20)
		if err != nil {
			return
		}
		items := batch.Items()
		if _, err := NewBatch(items, 1<<20); err != nil {
			t.Fatalf("successful batch decode returned invalid items: %v", err)
		}
	})
}

var benchmarkBatchBytes []byte

func BenchmarkBatchMarshalBinary(b *testing.B) {
	items := make([][]byte, 24)
	for index := range items {
		items[index] = mustEncodedDelta(b, "source", uint64(index+1))
	}
	batch, err := NewBatch(items, 1<<20)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(items[0]) * len(items)))
	b.ReportAllocs()
	for _, benchmark := range []struct {
		name string
		fn   func(Batch, int) ([]byte, error)
	}{
		{name: "optimized", fn: Batch.MarshalBinary},
		{name: "pre_optimization", fn: marshalBinaryWithCopy},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				encoded, err := benchmark.fn(batch, 1<<20)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkBatchBytes = encoded
			}
		})
	}
}

func marshalBinaryWithCopy(b Batch, maxBytes int) ([]byte, error) {
	validated, err := NewBatch(b.items, maxBytes)
	if err != nil {
		return nil, err
	}
	encoded := make([]byte, 0, 4+len(validated.items)*frameByteOverhead(validated.items))
	encoded = append(encoded, batchMagic...)
	encoded = frame.AppendUvarint(encoded, uint64(len(validated.items)))
	for _, item := range validated.items {
		encoded = frame.AppendUvarint(encoded, uint64(len(item)))
		encoded = append(encoded, item...)
	}
	return encoded, nil
}

func mustEncodedDelta(t testing.TB, replicaID string, amount uint64) []byte {
	t.Helper()
	counter, err := counter.NewGCounter(replicaID)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := counter.Increment(amount)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func mergeGCounter(left, right []byte) ([]byte, error) {
	leftDelta, err := counter.UnmarshalGCounterDelta(left)
	if err != nil {
		return nil, err
	}
	rightDelta, err := counter.UnmarshalGCounterDelta(right)
	if err != nil {
		return nil, err
	}
	merged, err := leftDelta.Merge(rightDelta)
	if err != nil {
		return nil, err
	}
	return merged.MarshalBinary()
}

func mergePNCounter(left, right []byte) ([]byte, error) {
	leftDelta, err := counter.UnmarshalPNCounterDelta(left)
	if err != nil {
		return nil, err
	}
	rightDelta, err := counter.UnmarshalPNCounterDelta(right)
	if err != nil {
		return nil, err
	}
	merged, err := leftDelta.Merge(rightDelta)
	if err != nil {
		return nil, err
	}
	return merged.MarshalBinary()
}
