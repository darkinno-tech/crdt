package delta

import (
	"bytes"
	"errors"
	"testing"
)

// TestCoalescerStopsAfterRepeatedConcurrentReplacements models a slow merge
// repeatedly losing its optimistic generation check to other writers. Before
// the retry budget, this loop could run indefinitely under sustained traffic.
func TestCoalescerStopsAfterRepeatedConcurrentReplacements(t *testing.T) {
	first := mustEncodedDelta(t, "source", 1)
	target := mustEncodedDelta(t, "target", 1)

	targetStarted := make(chan struct{}, maxMergeRetries)
	targetRelease := make(chan struct{}, maxMergeRetries)
	competitorStarted := make(chan struct{}, maxMergeRetries)
	competitorRelease := make(chan struct{}, maxMergeRetries)
	coalescer, err := NewCoalescer(1, 1<<20, func(left, right []byte) ([]byte, error) {
		if bytes.Equal(right, target) {
			targetStarted <- struct{}{}
			<-targetRelease
		} else {
			competitorStarted <- struct{}{}
			<-competitorRelease
		}
		return mergeGCounter(left, right)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coalescer.Add(first); err != nil {
		t.Fatal(err)
	}

	targetResult := make(chan error, 1)
	go func() { targetResult <- coalescer.Add(target) }()
	for attempt := 0; attempt < maxMergeRetries; attempt++ {
		<-targetStarted
		competitor := mustEncodedDelta(t, "competitor", uint64(attempt+1))
		competitorResult := make(chan error, 1)
		go func() { competitorResult <- coalescer.Add(competitor) }()
		<-competitorStarted
		competitorRelease <- struct{}{}
		if err := <-competitorResult; err != nil {
			t.Fatalf("competitor Add() error = %v", err)
		}
		targetRelease <- struct{}{}
	}
	if err := <-targetResult; !errors.Is(err, ErrMergeRetry) {
		t.Fatalf("Add() error = %v, want %v", err, ErrMergeRetry)
	}
	if items, bytes := coalescer.Len(); items != 1 || bytes == 0 {
		t.Fatalf("coalescer state after retry limit = %d items, %d bytes", items, bytes)
	}
}
