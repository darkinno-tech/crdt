package register

import (
	"fmt"
	"sync"
	"testing"
)

func TestMVRegisterConcurrentSetMergeReadAndEncode(t *testing.T) {
	left, err := NewMVRegister("left")
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewMVRegister("right")
	if err != nil {
		t.Fatal(err)
	}
	const iterations = 128
	start := make(chan struct{})
	errs := make(chan error, 6*iterations)
	var group sync.WaitGroup
	write := func(target *MVRegister, prefix string) {
		defer group.Done()
		<-start
		for index := 0; index < iterations; index++ {
			if _, err := target.Set([]byte(fmt.Sprintf("%s-%03d", prefix, index))); err != nil {
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
			_, _ = left.Value()
			_ = left.Values()
			_, _ = left.MarshalBinary()
			_ = left.State()
			_, _ = right.Value()
			_ = right.Values()
			_, _ = right.MarshalBinary()
			_ = right.State()
		}
	}()
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent MV-Register operation: %v", err)
		}
	}
	if err := left.Merge(right); err != nil {
		t.Fatal(err)
	}
	if count := left.State().ElementCount; count == 0 || count > 2 {
		t.Fatalf("visible concurrent values = %d", count)
	}
}
