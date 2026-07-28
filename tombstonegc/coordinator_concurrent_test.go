package tombstonegc

import (
	"errors"
	"sync"
	"testing"
)

func TestCoordinatorConcurrentAcknowledgementAndMembershipReplacement(t *testing.T) {
	codec := stringCodec{}
	value := mustSet(t, "source", codec)
	if _, err := value.Add("item"); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Remove("item"); err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCoordinator[string]("orders/v1", []string{"source", "remote"})
	if err != nil {
		t.Fatal(err)
	}

	const iterations = 128
	start := make(chan struct{})
	errs := make(chan error, iterations)
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		for index := 0; index < iterations; index++ {
			membership := coordinator.Membership()
			member := "source"
			if index%2 == 0 {
				member = "remote"
			}
			err := coordinator.Acknowledge(membership.GroupID, member, membership.Epoch, value.TombstoneTags())
			if err != nil && !errors.Is(err, ErrStaleMembership) && !errors.Is(err, ErrUnknownMember) {
				errs <- err
				return
			}
		}
	}()
	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		if _, err := coordinator.ReplaceMembership([]string{"source", "replacement"}); err != nil {
			errs <- err
		}
	}()
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent coordinator operation: %v", err)
		}
	}

	membership := coordinator.Membership()
	if err := coordinator.Acknowledge(membership.GroupID, "source", membership.Epoch, value.TombstoneTags()); err != nil {
		t.Fatal(err)
	}
	if removed, err := coordinator.AcknowledgeAndCompact(membership.GroupID, "replacement", membership.Epoch, value.TombstoneTags(), value); err != nil || removed != 1 {
		t.Fatalf("new membership compaction = %d, %v", removed, err)
	}
}
