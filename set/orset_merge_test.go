package set

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/clock"
)

func TestORSetMergeRejectsInvalidSourceWithoutMutation(t *testing.T) {
	codec := stringCodec{id: "example.com/merge/v1"}
	destination := mustNewORSet(t, "destination", codec)
	if _, err := destination.Add("retained"); err != nil {
		t.Fatal(err)
	}
	before, err := destination.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}

	source := mustNewORSet(t, "source", codec)
	source.mu.Lock()
	source.elements["invalid"] = map[crdt.Tag]struct{}{{}: {}}
	source.mu.Unlock()

	if err := destination.Merge(source); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("Merge() error = %v, want %v", err, ErrInvalidDelta)
	}
	after, err := destination.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("invalid source modified destination")
	}
}

func TestORSetOppositeMergeDoesNotDeadlock(t *testing.T) {
	codec := stringCodec{id: "example.com/merge/v1"}
	left := mustNewORSet(t, "left", codec)
	right := mustNewORSet(t, "right", codec)
	if _, err := left.Add("left"); err != nil {
		t.Fatal(err)
	}
	if _, err := right.Add("right"); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	done := make(chan error, 2)
	var workers sync.WaitGroup
	for _, merge := range []func() error{
		func() error { return left.Merge(right) },
		func() error { return right.Merge(left) },
	} {
		workers.Add(1)
		go func(merge func() error) {
			defer workers.Done()
			<-start
			for iteration := 0; iteration < 256; iteration++ {
				if err := merge(); err != nil {
					done <- err
					return
				}
			}
		}(merge)
	}
	go func() {
		workers.Wait()
		close(done)
	}()
	close(start)

	select {
	case <-time.After(5 * time.Second):
		t.Fatal("opposite merges did not complete")
	case err, ok := <-done:
		if ok && err != nil {
			t.Fatal(err)
		}
		for err := range done {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if !left.Contains("right") || !right.Contains("left") {
		t.Fatal("opposite merges did not converge")
	}
}

func TestORSetMergeAdvancesLocalClock(t *testing.T) {
	codec := stringCodec{id: "example.com/merge/v1"}
	const wallTime = uint64(1 << 63)
	remote, err := NewORSetFromClock(clock.State{ReplicaID: "remote", WallTime: wallTime}, codec)
	if err != nil {
		t.Fatal(err)
	}
	remoteDelta, err := remote.Add("remote")
	if err != nil {
		t.Fatal(err)
	}
	local, err := NewORSetFromClock(clock.State{ReplicaID: "local", WallTime: wallTime}, codec)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Merge(remote); err != nil {
		t.Fatal(err)
	}
	localDelta, err := local.Add("local")
	if err != nil {
		t.Fatal(err)
	}

	remoteTag := onlyORSetTag(t, remoteDelta)
	localTag := onlyORSetTag(t, localDelta)
	if remoteTag.Compare(localTag) >= 0 {
		t.Fatalf("local tag %#v did not advance past merged tag %#v", localTag, remoteTag)
	}
}

func onlyORSetTag(t testing.TB, delta ORSetDelta[string]) crdt.Tag {
	t.Helper()
	for _, tags := range delta.adds {
		for tag := range tags {
			return tag
		}
	}
	t.Fatal("delta had no add tag")
	return crdt.Tag{}
}
