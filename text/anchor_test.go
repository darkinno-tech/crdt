package text

import (
	"errors"
	"math/rand"
	"sync"
	"testing"
)

func TestRGAAnchorAtAndResolve(t *testing.T) {
	document := mustRGA(t, "anchor")
	if _, err := document.Insert(0, "abcd"); err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset <= 4; offset++ {
		anchor, err := document.AnchorAt(offset)
		if err != nil {
			t.Fatalf("AnchorAt(%d) = %v", offset, err)
		}
		if !anchor.Valid() {
			t.Fatalf("AnchorAt(%d) returned invalid anchor %#v", offset, anchor)
		}
		resolved, err := document.ResolveAnchor(anchor)
		if err != nil || resolved != offset {
			t.Fatalf("ResolveAnchor(AnchorAt(%d)) = %d, %v", offset, resolved, err)
		}
	}

	end, err := document.AnchorAt(4)
	if err != nil {
		t.Fatal(err)
	}
	if end.Position.Valid() || end.Association != AnchorAfter {
		t.Fatalf("end anchor = %#v, want root AnchorAfter", end)
	}
	if _, err := document.AnchorAt(5); !errors.Is(err, ErrRange) {
		t.Fatalf("AnchorAt(out of range) = %v, want %v", err, ErrRange)
	}
}

func TestRGAAnchorTracksRetainedTombstoneAcrossRunV2Delivery(t *testing.T) {
	alice := mustRGA(t, "anchor-alice")
	base := mustInsertRGA(t, alice, 0, "abcd")

	bob := mustRGA(t, "anchor-bob")
	carol := mustRGA(t, "anchor-carol")
	applyAnchorRunDelta(t, bob, base)
	applyAnchorRunDelta(t, carol, base)

	anchor, err := bob.AnchorAt(2) // The boundary directly before c.
	if err != nil {
		t.Fatal(err)
	}
	insert := mustInsertRGA(t, carol, 2, "X")
	deleted, err := bob.Delete(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	changes := []Delta{base, insert, deleted}
	for index, replica := range []*RGA{alice, bob, carol} {
		deliverAnchorRunChanges(t, replica, changes, int64(20260730+index))
		if got := replica.String(); got != "abXd" {
			t.Fatalf("replica text = %q, want abXd", got)
		}
		resolved, err := replica.ResolveAnchor(anchor)
		if err != nil || resolved != 3 {
			t.Fatalf("ResolveAnchor after shuffled run-v2 delivery = %d, %v; want 3, nil", resolved, err)
		}
	}
}

func applyAnchorRunDelta(t testing.TB, target *RGA, delta Delta) {
	t.Helper()
	encoded, err := delta.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalRGARunDelta(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
}

func deliverAnchorRunChanges(t testing.TB, target *RGA, changes []Delta, seed int64) {
	t.Helper()
	frames := make([][]byte, 0, len(changes)*2)
	for _, change := range changes {
		encoded, err := change.MarshalRunBinary()
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, encoded, encoded)
	}
	random := rand.New(rand.NewSource(seed))
	random.Shuffle(len(frames), func(left, right int) { frames[left], frames[right] = frames[right], frames[left] })
	for _, encoded := range frames {
		decoded, err := UnmarshalRGARunDelta(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if err := target.ApplyDelta(decoded); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRGAAnchorAfterPositionPrecedesDescendants(t *testing.T) {
	document := mustRGA(t, "anchor-after")
	if _, err := document.Insert(0, "ab"); err != nil {
		t.Fatal(err)
	}
	positions := document.Positions()
	anchor := Anchor{Position: positions[0], Association: AnchorAfter}
	if _, err := document.Insert(1, "X"); err != nil {
		t.Fatal(err)
	}
	if got := document.String(); got != "aXb" {
		t.Fatalf("text = %q, want aXb", got)
	}
	resolved, err := document.ResolveAnchor(anchor)
	if err != nil || resolved != 1 {
		t.Fatalf("ResolveAnchor(after a) = %d, %v; want 1, nil", resolved, err)
	}
}

func TestRGAAnchorResolvesAfterSnapshotRecovery(t *testing.T) {
	document := mustRGA(t, "anchor-recovery")
	if _, err := document.Insert(0, "abcd"); err != nil {
		t.Fatal(err)
	}
	anchor, err := document.AnchorAt(2)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := document.SnapshotRunCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := recovered.ResolveAnchor(anchor)
	if err != nil || resolved != 2 {
		t.Fatalf("ResolveAnchor after recovery = %d, %v; want 2, nil", resolved, err)
	}
}

func TestRGAAnchorFailsClosedAfterCompaction(t *testing.T) {
	document := mustRGA(t, "anchor-gc")
	if _, err := document.Insert(0, "a"); err != nil {
		t.Fatal(err)
	}
	anchor, err := document.AnchorAt(0)
	if err != nil {
		t.Fatal(err)
	}
	position := document.Positions()[0]
	if _, err := document.Delete(0, 1); err != nil {
		t.Fatal(err)
	}
	removed, err := document.CompactTombstones([]Position{position})
	if err != nil || removed != 1 {
		t.Fatalf("CompactTombstones = %d, %v; want 1, nil", removed, err)
	}
	if _, err := document.ResolveAnchor(anchor); !errors.Is(err, ErrAnchorGone) {
		t.Fatalf("ResolveAnchor(compacted) = %v, want %v", err, ErrAnchorGone)
	}
}

func TestRGAAnchorRejectsInvalidMetadata(t *testing.T) {
	document := mustRGA(t, "anchor-invalid")
	if _, err := document.ResolveAnchor(Anchor{}); !errors.Is(err, ErrInvalidAnchor) {
		t.Fatalf("ResolveAnchor(zero) = %v, want %v", err, ErrInvalidAnchor)
	}
	badPosition := Anchor{Position: Position{WallTime: 1}, Association: AnchorBefore}
	if _, err := document.ResolveAnchor(badPosition); !errors.Is(err, ErrInvalidAnchor) {
		t.Fatalf("ResolveAnchor(bad position) = %v, want %v", err, ErrInvalidAnchor)
	}
	unknownPosition := Anchor{Position: Position{ReplicaID: "missing"}, Association: AnchorBefore}
	if _, err := document.ResolveAnchor(unknownPosition); !errors.Is(err, ErrAnchorGone) {
		t.Fatalf("ResolveAnchor(unknown position) = %v, want %v", err, ErrAnchorGone)
	}
	var nilDocument *RGA
	if _, err := nilDocument.AnchorAt(0); !errors.Is(err, ErrNilText) {
		t.Fatalf("nil AnchorAt = %v, want %v", err, ErrNilText)
	}
}

func TestRGAAnchorResolveConcurrentMutation(t *testing.T) {
	document := mustRGA(t, "anchor-concurrent")
	if _, err := document.Insert(0, "seed"); err != nil {
		t.Fatal(err)
	}
	anchor, err := document.AnchorAt(0)
	if err != nil {
		t.Fatal(err)
	}

	const readers = 4
	const iterations = 200
	start := make(chan struct{})
	var group sync.WaitGroup
	group.Add(readers + 1)
	for reader := 0; reader < readers; reader++ {
		go func() {
			defer group.Done()
			<-start
			for iteration := 0; iteration < iterations; iteration++ {
				if _, err := document.ResolveAnchor(anchor); err != nil {
					t.Errorf("ResolveAnchor during mutation = %v", err)
					return
				}
			}
		}()
	}
	go func() {
		defer group.Done()
		<-start
		for iteration := 0; iteration < iterations; iteration++ {
			if _, err := document.Insert(0, "x"); err != nil {
				t.Errorf("Insert during anchor reads = %v", err)
				return
			}
			if _, err := document.Delete(0, 1); err != nil {
				t.Errorf("Delete during anchor reads = %v", err)
				return
			}
		}
	}()
	close(start)
	group.Wait()
}
