package set

import (
	"fmt"
	"sync"
	"testing"
)

func TestGSetConcurrentAddMergeReadAndEncode(t *testing.T) {
	codec := stringCodec{id: "example.com/gset-concurrent/v1"}
	left, err := NewGSet("left", codec)
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewGSet("right", codec)
	if err != nil {
		t.Fatal(err)
	}
	const iterations = 128
	start := make(chan struct{})
	errs := make(chan error, 6*iterations)
	var group sync.WaitGroup
	write := func(target *GSet[string], prefix string) {
		defer group.Done()
		<-start
		for index := 0; index < iterations; index++ {
			if _, err := target.Add(fmt.Sprintf("%s-%03d", prefix, index)); err != nil {
				errs <- err
				return
			}
		}
	}
	group.Add(2)
	go write(left, "left")
	go write(right, "right")
	for index := 0; index < 2; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			for iteration := 0; iteration < iterations; iteration++ {
				if err := left.Merge(right); err != nil {
					errs <- err
					return
				}
				if err := right.Merge(left); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		for index := 0; index < iterations; index++ {
			_, _ = left.MarshalBinary()
			_, _ = right.MarshalBinary()
			_ = left.Elements()
			_ = right.Elements()
			_ = left.State()
			_ = right.State()
		}
	}()
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent G-Set operation: %v", err)
		}
	}
	if err := left.Merge(right); err != nil {
		t.Fatal(err)
	}
	if left.State().ElementCount != 2*iterations {
		t.Fatalf("element count = %d, want %d", left.State().ElementCount, 2*iterations)
	}
}
