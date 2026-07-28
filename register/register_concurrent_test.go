package register

import (
	"sync"
	"testing"
)

func TestRegistersConcurrentReadWriteAndMerge(t *testing.T) {
	left, err := NewLWW("left")
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewLWW("right")
	if err != nil {
		t.Fatal(err)
	}
	leftMax, rightMax := NewMax(), NewMax()

	const iterations = 128
	start := make(chan struct{})
	errs := make(chan error, 4*iterations)
	var group sync.WaitGroup
	write := func(target *LWW, maximum *Max, offset byte) {
		defer group.Done()
		<-start
		for index := 0; index < iterations; index++ {
			if err := target.Set([]byte{offset, byte(index)}); err != nil {
				errs <- err
				return
			}
			if err := maximum.Set(uint64(index)); err != nil {
				errs <- err
				return
			}
		}
	}
	group.Add(2)
	go write(left, leftMax, 'l')
	go write(right, rightMax, 'r')

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
				if err := leftMax.Merge(rightMax); err != nil {
					errs <- err
					return
				}
				if err := rightMax.Merge(leftMax); err != nil {
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
			_, _ = left.Get()
			_, _ = right.Get()
			_, _ = leftMax.Get()
			_, _ = rightMax.Get()
			_ = left.ClockState()
			_ = right.ClockState()
			_ = left.State()
			_ = right.State()
			_ = leftMax.State()
			_ = rightMax.State()
		}
	}()

	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent register operation: %v", err)
		}
	}
	if err := left.Merge(right); err != nil {
		t.Fatal(err)
	}
	if err := right.Merge(left); err != nil {
		t.Fatal(err)
	}
	if leftValue, leftOK := left.Get(); !leftOK || len(leftValue) == 0 {
		t.Fatal("left LWW lost its final value")
	}
	if value, ok := leftMax.Get(); !ok || value != iterations-1 {
		t.Fatalf("left max = %d, %v", value, ok)
	}
}
