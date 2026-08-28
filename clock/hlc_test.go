package clock

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/im10furry/crdt"
)

func TestNewHLCRejectsInvalidReplicaID(t *testing.T) {
	t.Parallel()

	if _, err := NewHLC(" \t"); !errors.Is(err, ErrInvalidReplicaID) {
		t.Fatalf("NewHLC() error = %v, want %v", err, ErrInvalidReplicaID)
	}
}

func TestHLCNowRemainsMonotonicWhenPhysicalTimeMovesBackward(t *testing.T) {
	t.Parallel()

	h, err := NewHLC("replica-a")
	if err != nil {
		t.Fatalf("NewHLC() error = %v", err)
	}

	values := []time.Time{
		time.UnixMilli(100),
		time.UnixMilli(100),
		time.UnixMilli(99),
	}
	var index int
	h.now = func() time.Time {
		value := values[index]
		index++
		return value
	}

	first, err := h.Now()
	if err != nil {
		t.Fatalf("first Now() error = %v", err)
	}
	second, err := h.Now()
	if err != nil {
		t.Fatalf("second Now() error = %v", err)
	}
	third, err := h.Now()
	if err != nil {
		t.Fatalf("third Now() error = %v", err)
	}

	if first != (crdt.Tag{ReplicaID: "replica-a", WallTime: 100}) {
		t.Fatalf("first Now() = %#v", first)
	}
	if second != (crdt.Tag{ReplicaID: "replica-a", WallTime: 100, Logical: 1}) {
		t.Fatalf("second Now() = %#v", second)
	}
	if third != (crdt.Tag{ReplicaID: "replica-a", WallTime: 100, Logical: 2}) {
		t.Fatalf("third Now() = %#v", third)
	}
}

func TestHLCWitnessAdvancesPastRemoteTag(t *testing.T) {
	t.Parallel()

	h, err := NewHLC("replica-a")
	if err != nil {
		t.Fatalf("NewHLC() error = %v", err)
	}
	h.now = func() time.Time { return time.UnixMilli(100) }

	remote := crdt.Tag{ReplicaID: "replica-b", WallTime: 102, Logical: 7}
	if err := h.Witness(remote); err != nil {
		t.Fatalf("Witness() error = %v", err)
	}

	local, err := h.Now()
	if err != nil {
		t.Fatalf("Now() error = %v", err)
	}
	if local != (crdt.Tag{ReplicaID: "replica-a", WallTime: 102, Logical: 9}) {
		t.Fatalf("Now() = %#v, want wall time 102 and logical time 9", local)
	}
}

func TestHLCWitnessRejectsInvalidTagWithoutMutation(t *testing.T) {
	t.Parallel()

	h, err := NewHLC("replica-a")
	if err != nil {
		t.Fatalf("NewHLC() error = %v", err)
	}
	h.now = func() time.Time { return time.UnixMilli(100) }

	if _, err := h.Now(); err != nil {
		t.Fatalf("Now() error = %v", err)
	}
	if err := h.Witness(crdt.Tag{}); !errors.Is(err, ErrInvalidRemoteTag) {
		t.Fatalf("Witness() error = %v, want %v", err, ErrInvalidRemoteTag)
	}

	next, err := h.Now()
	if err != nil {
		t.Fatalf("Now() error = %v", err)
	}
	if next != (crdt.Tag{ReplicaID: "replica-a", WallTime: 100, Logical: 1}) {
		t.Fatalf("Now() after invalid Witness = %#v", next)
	}
}

func TestHLCAdvancesWallTimeWhenLogicalCounterOverflows(t *testing.T) {
	t.Parallel()

	h, err := NewHLC("replica-a")
	if err != nil {
		t.Fatalf("NewHLC() error = %v", err)
	}
	h.now = func() time.Time { return time.UnixMilli(1) }
	h.wallTime = 5
	h.logical = math.MaxUint64

	tag, err := h.Now()
	if err != nil {
		t.Fatalf("Now() error = %v", err)
	}
	if tag != (crdt.Tag{ReplicaID: "replica-a", WallTime: 6}) {
		t.Fatalf("Now() = %#v", tag)
	}
}

func TestHLCNowIsConcurrentSafe(t *testing.T) {
	t.Parallel()

	h, err := NewHLC("replica-a")
	if err != nil {
		t.Fatalf("NewHLC() error = %v", err)
	}
	h.now = func() time.Time { return time.UnixMilli(100) }

	const calls = 100
	tags := make(chan crdt.Tag, calls)
	errs := make(chan error, calls)
	var group sync.WaitGroup
	for i := 0; i < calls; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			tag, err := h.Now()
			if err != nil {
				errs <- err
				return
			}
			tags <- tag
		}()
	}
	group.Wait()
	close(tags)
	close(errs)

	for err := range errs {
		t.Fatalf("Now() error = %v", err)
	}

	seen := make(map[crdt.Tag]struct{}, calls)
	for tag := range tags {
		seen[tag] = struct{}{}
	}
	if len(seen) != calls {
		t.Fatalf("unique tag count = %d, want %d", len(seen), calls)
	}
}

func TestHLCStateRestoresMonotonicityAcrossRestart(t *testing.T) {
	t.Parallel()
	original, err := NewHLC("replica-a")
	if err != nil {
		t.Fatal(err)
	}
	original.now = func() time.Time { return time.UnixMilli(100) }
	first, err := original.Now()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := NewHLCFromState(original.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	restored.now = func() time.Time { return time.UnixMilli(100) }
	next, err := restored.Now()
	if err != nil {
		t.Fatal(err)
	}
	if first.Compare(next) >= 0 {
		t.Fatalf("restored tag %#v does not advance past %#v", next, first)
	}
}
