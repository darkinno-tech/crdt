package replica

import (
	"errors"
	"math/rand"
	"strconv"
	"testing"

	"github.com/darkinno-tech/crdt"
	frame "github.com/darkinno-tech/crdt/encoding"
)

func TestInboxDeliveryClassifiesInstalledAndBufferedDuplicates(t *testing.T) {
	manifest, err := NewManifest("counter", "example.com/counter/v1", 1, Protocol{
		StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	frontier, err := NewFrontier(nil)
	if err != nil {
		t.Fatal(err)
	}
	applied := 0
	inbox, err := NewInbox(manifest, frontier, 4, 1024, func([]byte) error {
		applied++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	first := mustChange(t, manifest, Dot{Actor: "writer", Counter: 1}, 1)
	if delivery, err := inbox.Receive(first); err != nil || !delivery.Accepted() || delivery.Duplicate || len(delivery.Applied) != 1 {
		t.Fatalf("first delivery = %#v, %v", delivery, err)
	}
	if delivery, err := inbox.Receive(first); err != nil || !delivery.Duplicate || delivery.Accepted() || delivery.Buffered || len(delivery.Applied) != 0 {
		t.Fatalf("installed duplicate delivery = %#v, %v", delivery, err)
	}

	future := mustChange(t, manifest, Dot{Actor: "writer", Counter: 3}, 3)
	if delivery, err := inbox.Receive(future); err != nil || !delivery.Accepted() || delivery.Duplicate || !delivery.Buffered {
		t.Fatalf("future delivery = %#v, %v", delivery, err)
	}
	if delivery, err := inbox.Receive(future); err != nil || !delivery.Duplicate || delivery.Accepted() || !delivery.Buffered {
		t.Fatalf("buffered duplicate delivery = %#v, %v", delivery, err)
	}
	if applied != 1 {
		t.Fatalf("apply calls after duplicates = %d, want 1", applied)
	}
}

func TestInboxRejectsUnvalidatedInternallyConstructedChange(t *testing.T) {
	manifest, err := NewManifest("counter", "example.com/counter/v1", 1, Protocol{
		StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	frontier, err := NewFrontier(nil)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := NewInbox(manifest, frontier, 1, 1024, func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	forged := Change{
		Dot:      Dot{Actor: "writer", Counter: 1},
		manifest: manifest,
		delta:    []byte("not a frame"),
	}
	if _, err := inbox.Receive(forged); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("Receive(unvalidated forged change) = %v, want %v", err, ErrInvalidChange)
	}
	if got := inbox.Frontier().Counter("writer"); got != 0 {
		t.Fatalf("forged change advanced frontier to %d", got)
	}
}

func TestSimulatedInboxClassifiesDuplicatedOutOfOrderDelivery(t *testing.T) {
	manifest, err := NewManifest("counter", "example.com/counter/v1", 1, Protocol{
		StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatal(err)
	}

	for seed := int64(1); seed <= 32; seed++ {
		t.Run("seed_"+strconv.FormatInt(seed, 10), func(t *testing.T) {
			frontier, err := NewFrontier(nil)
			if err != nil {
				t.Fatal(err)
			}
			applied := make([]byte, 0, 24)
			inbox, err := NewInbox(manifest, frontier, 24, 4096, func(delta []byte) error {
				decoded, err := frame.UnmarshalFrame(delta, frame.DefaultLimits())
				if err != nil {
					return err
				}
				applied = append(applied, decoded.Payload[0])
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}

			changes := make([]Change, 0, 48)
			for counter := uint64(1); counter <= 24; counter++ {
				change := mustChange(t, manifest, Dot{Actor: "writer", Counter: counter}, byte(counter))
				changes = append(changes, change)
				for copies := counter % 3; copies > 0; copies-- {
					changes = append(changes, change)
				}
			}
			random := rand.New(rand.NewSource(seed))
			random.Shuffle(len(changes), func(left, right int) {
				changes[left], changes[right] = changes[right], changes[left]
			})

			seen := make(map[Dot]bool, 24)
			for _, change := range changes {
				delivery, err := inbox.Receive(change)
				if err != nil {
					t.Fatal(err)
				}
				if seen[change.Dot] {
					if !delivery.Duplicate || delivery.Accepted() {
						t.Fatalf("duplicate %v delivery = %#v", change.Dot, delivery)
					}
					continue
				}
				seen[change.Dot] = true
				if delivery.Duplicate || !delivery.Accepted() {
					t.Fatalf("first %v delivery = %#v", change.Dot, delivery)
				}
			}

			if got := inbox.Frontier().Counter("writer"); got != 24 {
				t.Fatalf("frontier = %d, want 24", got)
			}
			if changes, bytes := inbox.Pending(); changes != 0 || bytes != 0 {
				t.Fatalf("pending = %d changes, %d bytes", changes, bytes)
			}
			if len(applied) != 24 {
				t.Fatalf("applied count = %d, want 24", len(applied))
			}
			for index, payload := range applied {
				if want := byte(index + 1); payload != want {
					t.Fatalf("applied[%d] = %d, want %d", index, payload, want)
				}
			}
		})
	}
}
