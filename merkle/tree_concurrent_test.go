package merkle

import (
	"strconv"
	"sync"
	"testing"
)

func TestTreeConcurrentInsertDeleteRootAndDiff(t *testing.T) {
	value := NewTree()
	const iterations = 256
	start := make(chan struct{})
	var group sync.WaitGroup
	for worker := 0; worker < 2; worker++ {
		worker := worker
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			for index := 0; index < iterations; index++ {
				key := "key-" + strconv.Itoa((worker+index)%32)
				entry := []byte(strconv.Itoa(worker*iterations + index))
				value.Insert(key, entry)
				if index%3 == 0 {
					value.Delete(key)
				}
			}
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		for index := 0; index < iterations; index++ {
			_ = value.Root()
			_, _, _ = Diff(value, value)
		}
	}()
	close(start)
	group.Wait()
	if first, second := value.Root(), value.Root(); first != second {
		t.Fatal("root changed without a write")
	}
	leftOnly, rightOnly, different := Diff(value, value)
	if len(leftOnly) != 0 || len(rightOnly) != 0 || len(different) != 0 {
		t.Fatalf("equal trees differ: %#v %#v %#v", leftOnly, rightOnly, different)
	}
}
