package set

import (
	"fmt"
	"sync"
	"testing"
)

func TestORSetConcurrentMutationSnapshotAndCrossMerge(t *testing.T) {
	codec := stringCodec{id: "example.com/stress/v1"}
	left, err := NewORSet("stress-left", codec)
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewORSet("stress-right", codec)
	if err != nil {
		t.Fatal(err)
	}

	const writersPerReplica = 6
	const elementsPerWriter = 512
	const maintenanceIterations = 192

	errCh := make(chan error, 2*writersPerReplica+4)
	var writers sync.WaitGroup
	for replica, value := range []*ORSet[string]{left, right} {
		for writer := 0; writer < writersPerReplica; writer++ {
			writers.Add(1)
			go func(replica int, value *ORSet[string], writer int) {
				defer writers.Done()
				for element := 0; element < elementsPerWriter; element++ {
					if _, err := value.Add(fmt.Sprintf("%d/%d/%d", replica, writer, element)); err != nil {
						errCh <- err
						return
					}
				}
			}(replica, value, writer)
		}
	}

	var maintenance sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		maintenance.Add(1)
		go func(worker int) {
			defer maintenance.Done()
			for iteration := 0; iteration < maintenanceIterations; iteration++ {
				if worker%2 == 0 {
					if err := left.Merge(right); err != nil {
						errCh <- err
						return
					}
					if _, _, err := left.MarshalBinaryWithClockState(); err != nil {
						errCh <- err
						return
					}
				} else {
					if err := right.Merge(left); err != nil {
						errCh <- err
						return
					}
					if _, err := right.SnapshotCurrentState(); err != nil {
						errCh <- err
						return
					}
				}
			}
		}(worker)
	}

	writers.Wait()
	maintenance.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := left.Merge(right); err != nil {
		t.Fatal(err)
	}
	if err := right.Merge(left); err != nil {
		t.Fatal(err)
	}
	want := 2 * writersPerReplica * elementsPerWriter
	for name, value := range map[string]*ORSet[string]{"left": left, "right": right} {
		if got := len(value.Elements()); got != want {
			t.Fatalf("%s element count = %d, want %d", name, got, want)
		}
	}
}
