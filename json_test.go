package crdt_test

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/darkinno-tech/crdt"
	"github.com/darkinno-tech/crdt/counter"
	"github.com/darkinno-tech/crdt/lww"
	"github.com/darkinno-tech/crdt/register"
	"github.com/darkinno-tech/crdt/set"
	"github.com/darkinno-tech/crdt/text"
	"github.com/darkinno-tech/crdt/tree"
)

var (
	_ json.Marshaler = (*counter.GCounter)(nil)
	_ json.Marshaler = (*counter.PNCounter)(nil)
	_ json.Marshaler = counter.GCounterDelta{}
	_ json.Marshaler = counter.PNCounterDelta{}
	_ json.Marshaler = (*set.GSet[string])(nil)
	_ json.Marshaler = (*set.ORSet[string])(nil)
	_ json.Marshaler = set.GSetDelta[string]{}
	_ json.Marshaler = set.ORSetDelta[string]{}
	_ json.Marshaler = (*lww.Set[string])(nil)
	_ json.Marshaler = (*lww.Map)(nil)
	_ json.Marshaler = lww.MapDelta{}
	_ json.Marshaler = (*register.LWW)(nil)
	_ json.Marshaler = (*register.Max)(nil)
	_ json.Marshaler = (*register.MVRegister)(nil)
	_ json.Marshaler = register.MVRegisterDelta{}
	_ json.Marshaler = (*text.RGA)(nil)
	_ json.Marshaler = text.Delta{}
	_ json.Marshaler = (*tree.ORTree)(nil)
	_ json.Marshaler = tree.Delta{}
)

func TestMarshalStateJSONUsesStableDiagnosticSchema(t *testing.T) {
	value, err := counter.NewGCounter("replica-a")
	if err != nil {
		t.Fatalf("NewGCounter() error = %v", err)
	}
	if _, err := value.Increment(7); err != nil {
		t.Fatalf("Increment() error = %v", err)
	}

	got, err := crdt.MarshalStateJSON(value)
	if err != nil {
		t.Fatalf("MarshalStateJSON() error = %v", err)
	}
	const want = `{"type":"gcounter","replica_id":"replica-a","element_count":1,"tombstone_count":0}`
	if string(got) != want {
		t.Fatalf("MarshalStateJSON() = %s, want %s", got, want)
	}
}

func TestStateJSONMarshalerOmitsCounterComponents(t *testing.T) {
	value, err := counter.NewGCounter("replica-a")
	if err != nil {
		t.Fatalf("NewGCounter() error = %v", err)
	}
	if _, err := value.Increment(7); err != nil {
		t.Fatalf("Increment() error = %v", err)
	}

	got, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if string(got) != `{"type":"gcounter","replica_id":"replica-a","element_count":1,"tombstone_count":0}` {
		t.Fatalf("json.Marshal() = %s", got)
	}
	if string(got) == `{"replica-a":7}` {
		t.Fatal("json.Marshal() exposed per-replica components")
	}
}

func TestDeltaJSONMarshalerOmitsCounterComponents(t *testing.T) {
	value, err := counter.NewGCounter("replica-a")
	if err != nil {
		t.Fatalf("NewGCounter() error = %v", err)
	}
	delta, err := value.Increment(7)
	if err != nil {
		t.Fatalf("Increment() error = %v", err)
	}

	got, err := json.Marshal(delta)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	const want = `{"type":"gcounter-delta","replica_id":"","element_count":1,"tombstone_count":0}`
	if string(got) != want {
		t.Fatalf("json.Marshal() = %s, want %s", got, want)
	}
	if string(got) == `{"replica-a":7}` {
		t.Fatal("json.Marshal() exposed per-replica components")
	}
}

func TestStateJSONMarshalerConcurrentMutation(t *testing.T) {
	value, err := counter.NewGCounter("replica-a")
	if err != nil {
		t.Fatalf("NewGCounter() error = %v", err)
	}

	const (
		writers             = 16
		serializers         = 8
		operationsPerWorker = 1024
	)
	errCh := make(chan error, writers+serializers)
	var group sync.WaitGroup
	for worker := 0; worker < writers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for operation := 0; operation < operationsPerWorker; operation++ {
				if _, err := value.Increment(1); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	for worker := 0; worker < serializers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for operation := 0; operation < operationsPerWorker; operation++ {
				encoded, err := json.Marshal(value)
				if err != nil {
					errCh <- err
					return
				}
				if !json.Valid(encoded) {
					errCh <- fmt.Errorf("invalid JSON: %q", encoded)
					return
				}
			}
		}()
	}
	group.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := value.Value()
	if err != nil {
		t.Fatalf("Value() error = %v", err)
	}
	if want := uint64(writers * operationsPerWorker); got != want {
		t.Fatalf("Value() = %d, want %d", got, want)
	}
}
