package tree

import (
	"errors"
	"reflect"
	"sync"
	"testing"
)

// TestORTreeConcurrentMutationReadAndRecovery exercises the public
// synchronization boundary while a live root remains available for local adds.
// A removal may race with another remover and legitimately return ErrUnknownNode.
func TestORTreeConcurrentMutationReadAndRecovery(t *testing.T) {
	value, err := New("local")
	if err != nil {
		t.Fatal(err)
	}
	root, rootDelta, err := value.Add(NodeID{}, []byte("root"))
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
				if _, _, err := value.Add(root, []byte{byte(worker), byte(index)}); err != nil {
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
			for _, node := range value.Nodes() {
				if node.ID == root {
					continue
				}
				if _, err := value.Remove(node.ID); err != nil && !errors.Is(err, ErrUnknownNode) {
					errs <- err
					return
				}
				break
			}
		}
	}()

	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		for index := 0; index < iterations; index++ {
			_ = value.Nodes()
			_ = value.State()
			if _, err := value.MarshalBinary(); err != nil {
				errs <- err
				return
			}
			if _, err := value.SnapshotCurrentState(); err != nil {
				errs <- err
				return
			}
			if err := value.ApplyDelta(rootDelta); err != nil {
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
			t.Fatalf("concurrent OR-Tree operation: %v", err)
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
	if got, want := recovered.Nodes(), value.Nodes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("recovered nodes = %#v, want %#v", got, want)
	}
}
