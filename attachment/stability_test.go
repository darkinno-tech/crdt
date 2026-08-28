package attachment

import (
	"errors"
	"reflect"
	"testing"

	"github.com/darkinno-tech/crdt/tombstonegc"
)

func TestRegisterExactAcknowledgementTombstoneLifecycle(t *testing.T) {
	options := DefaultOptions()
	options.MaxEntries = 1
	source, err := NewWithOptions("source", options)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := NewWithOptions("remote", options)
	if err != nil {
		t.Fatal(err)
	}

	put, err := source.Put("cover", testReference("cover", "image/png", 2_048))
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.ApplyDelta(put); err != nil {
		t.Fatal(err)
	}
	deleted, err := source.Delete("cover")
	if err != nil {
		t.Fatal(err)
	}
	tags := source.TombstoneTags()
	if len(tags) != 1 {
		t.Fatalf("source tombstones = %#v", tags)
	}

	coordinator, err := tombstonegc.NewCoordinator[string]("media-session", []string{"source", "remote"})
	if err != nil {
		t.Fatal(err)
	}
	membership := coordinator.Membership()
	removed, err := coordinator.AcknowledgeAndCompactTarget(membership.GroupID, "source", membership.Epoch, tags, source)
	if err != nil || removed != 0 {
		t.Fatalf("source-only acknowledgement compacted %d, %v", removed, err)
	}
	removed, err = coordinator.AcknowledgeAndCompactTarget(membership.GroupID, "remote", membership.Epoch, remote.TombstoneTags(), source)
	if err != nil || removed != 0 {
		t.Fatalf("frontier-like empty acknowledgement compacted %d, %v", removed, err)
	}
	if state := source.State(); state.TombstoneCount != 1 {
		t.Fatalf("source compacted before remote observed delete: %#v", state)
	}

	if err := remote.ApplyDelta(deleted); err != nil {
		t.Fatal(err)
	}
	remoteTags := remote.TombstoneTags()
	if !reflect.DeepEqual(remoteTags, tags) {
		t.Fatalf("remote exact tombstones = %#v, want %#v", remoteTags, tags)
	}
	removed, err = coordinator.AcknowledgeAndCompactTarget(membership.GroupID, "remote", membership.Epoch, remoteTags, source)
	if err != nil || removed != 1 {
		t.Fatalf("exact acknowledgement compacted %d, %v", removed, err)
	}
	if state := source.State(); state.ElementCount != 0 || state.TombstoneCount != 0 {
		t.Fatalf("source post-compaction state = %#v", state)
	}
	if _, err := source.SnapshotCurrentState(); err != nil {
		t.Fatalf("post-compaction snapshot = %v", err)
	}
	removed, err = coordinator.AcknowledgeAndCompactTarget(membership.GroupID, "remote", membership.Epoch, remoteTags, remote)
	if err != nil || removed != 1 {
		t.Fatalf("remote compacted %d, %v", removed, err)
	}
	if state := remote.State(); state.ElementCount != 0 || state.TombstoneCount != 0 {
		t.Fatalf("remote post-compaction state = %#v", state)
	}
	if _, err := source.Put("next", testReference("next", "image/webp", 512)); err != nil {
		t.Fatalf("entry budget was not reclaimed after compaction: %v", err)
	}
}

func TestRegisterForwardsUnderlyingDecodeBudgetsAndNilTombstoneMethods(t *testing.T) {
	options := Options{MaxEntries: 1, MaxKeyBytes: 4, MaxObjectIDBytes: 8, MaxObjectBytes: 1 << 10}
	value, err := NewWithOptions("target", options)
	if err != nil {
		t.Fatal(err)
	}
	before, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	beforeClock := value.ClockState()

	writer, err := New("writer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Put("oversized", testReference("ok", "image/png", 1)); err != nil {
		t.Fatal(err)
	}
	state, err := writer.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := value.UnmarshalBinary(state); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("over-budget state = %v", err)
	}
	after, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) || value.ClockState() != beforeClock {
		t.Fatalf("over-budget state changed register: state=%x clock=%#v", after, value.ClockState())
	}

	var nilRegister *Register
	if tags := nilRegister.TombstoneTags(); tags != nil {
		t.Fatalf("nil tombstones = %#v", tags)
	}
	if removed, err := nilRegister.CompactTombstones(nil); !errors.Is(err, ErrNilRegister) || removed != 0 {
		t.Fatalf("nil CompactTombstones() = %d, %v", removed, err)
	}
}
