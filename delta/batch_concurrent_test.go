package delta

import (
	"sync"
	"testing"

	"github.com/DarkInno/crdt/counter"
)

func TestCoalescerConcurrentAddAndDrain(t *testing.T) {
	source, err := counter.NewGCounter("source")
	if err != nil {
		t.Fatal(err)
	}
	frames := make([][]byte, 0, 64)
	for index := 0; index < cap(frames); index++ {
		delta, err := source.Increment(1)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := delta.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, encoded)
	}
	coalescer, err := NewCoalescer(1, 1<<20, mergeGCounter)
	if err != nil {
		t.Fatal(err)
	}

	var group sync.WaitGroup
	errs := make(chan error, len(frames))
	for _, frame := range frames {
		frame := frame
		group.Add(1)
		go func() {
			defer group.Done()
			if err := coalescer.Add(frame); err != nil {
				errs <- err
			}
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Add: %v", err)
		}
	}
	batch := coalescer.Drain()
	items := batch.Items()
	if len(items) != 1 {
		t.Fatalf("drained items = %d, want 1", len(items))
	}
	delta, err := counter.UnmarshalGCounterDelta(items[0])
	if err != nil {
		t.Fatal(err)
	}
	target, err := counter.NewGCounter("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(delta); err != nil {
		t.Fatal(err)
	}
	if value, err := target.Value(); err != nil || value != uint64(len(frames)) {
		t.Fatalf("coalesced value = %d, %v", value, err)
	}
}
