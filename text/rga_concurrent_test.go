package text

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestRGAConcurrentMutationReadAndRecovery exercises the synchronization
// promise on the public mutation, projection, state, and serialization paths.
// Deletes may race with another delete and legitimately observe ErrRange.
func TestRGAConcurrentMutationReadAndRecovery(t *testing.T) {
	value, err := New("local")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := value.Insert(0, "seed")
	if err != nil {
		t.Fatal(err)
	}

	const iterations = 64
	start := make(chan struct{})
	errs := make(chan error, 4*iterations)
	var group sync.WaitGroup

	for worker := 0; worker < 2; worker++ {
		worker := worker
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			for index := 0; index < iterations; index++ {
				delta, err := value.Insert(0, string(rune('a'+(worker+index)%26)))
				if err != nil {
					errs <- err
					return
				}
				if err := value.ApplyDelta(delta); err != nil {
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
			visible := value.Positions()
			if len(visible) == 0 {
				continue
			}
			if _, err := value.Delete(0, 1); err != nil && !errors.Is(err, ErrRange) {
				errs <- err
				return
			}
		}
	}()

	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		for index := 0; index < iterations; index++ {
			_ = value.String()
			_ = value.Positions()
			_ = value.State()
			if _, err := value.MarshalBinary(); err != nil {
				errs <- err
				return
			}
			if err := value.ApplyDelta(seed); err != nil {
				errs <- err
				return
			}
		}
	}()

	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent RGA operation: %v", err)
		}
	}

	state, err := value.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary after concurrent operations: %v", err)
	}
	recovered, err := New("recovered")
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.UnmarshalBinary(state); err != nil {
		t.Fatalf("UnmarshalBinary recovered state: %v", err)
	}
	if got, want := recovered.String(), value.String(); got != want {
		t.Fatalf("recovered text = %q, want %q", got, want)
	}
}

func TestRGAConcurrentEligibleCompactionAndReads(t *testing.T) {
	value, err := New("local")
	if err != nil {
		t.Fatal(err)
	}
	const count = 128
	if _, err := value.Insert(0, strings.Repeat("a", count)); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Delete(0, count); err != nil {
		t.Fatal(err)
	}
	tags := value.TombstoneTags()

	start := make(chan struct{})
	errs := make(chan error, 3)
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		removed, err := value.CompactEligibleTombstones(tags)
		if err != nil {
			errs <- err
			return
		}
		if removed != count {
			errs <- fmt.Errorf("CompactEligibleTombstones removed %d, want %d", removed, count)
		}
	}()
	for worker := 0; worker < 2; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			for index := 0; index < count; index++ {
				_ = value.String()
				_ = value.Positions()
				_ = value.State()
				if _, err := value.MarshalBinary(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent eligible compaction: %v", err)
		}
	}
	if state := value.State(); state.ElementCount != 0 || state.TombstoneCount != 0 {
		t.Fatalf("state after concurrent compaction = %#v", state)
	}
}
