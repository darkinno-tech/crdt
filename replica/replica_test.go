package replica

import (
	"errors"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	"github.com/DarkInno/crdt/counter"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/text"
	"github.com/DarkInno/crdt/tree"
)

func TestManifestRejectsDisabledAndMismatchedProtocols(t *testing.T) {
	stable, err := NewManifest("cart", "example.com/cart/v1", 1, Protocol{
		StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatalf("NewManifest stable: %v", err)
	}
	if err := stable.Compatible(stable); err != nil {
		t.Fatalf("stable manifest incompatibility: %v", err)
	}
	if _, err := NewManifest("text", "example.com/text/v1", 1, Protocol{
		StateID: crdt.TypeIDRGAState, DeltaID: crdt.TypeIDRGADelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{AllowExperimental: true}); err != nil {
		t.Fatalf("experimental RGA manifest error = %v", err)
	}
	if _, err := NewManifest("set", "example.com/set/v1", 1, Protocol{
		StateID: crdt.TypeIDLWWSetState, DeltaID: crdt.TypeIDLWWSetDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{AllowExperimental: true}); err != nil {
		t.Fatalf("experimental LWW-Set manifest error = %v", err)
	}
	remote := stable
	remote.Protocol.SemanticsVersion = 2
	if err := stable.Compatible(remote); !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("semantic mismatch error = %v", err)
	}
}

func TestExperimentalProtocolsRequireExplicitPolicyAtUseBoundaries(t *testing.T) {
	policy := crdt.ProtocolPolicy{AllowExperimental: true}
	manifest, err := NewManifest("text", "example.com/text/v1", 1, Protocol{
		StateID: crdt.TypeIDRGARunState, DeltaID: crdt.TypeIDRGARunDelta, SemanticsVersion: 2,
	}, policy)
	if err != nil {
		t.Fatalf("NewManifest experimental: %v", err)
	}
	frontier, err := NewFrontier(nil)
	if err != nil {
		t.Fatalf("NewFrontier: %v", err)
	}
	delta := mustFrame(t, crdt.TypeIDRGARunDelta, "")
	state := mustFrame(t, crdt.TypeIDRGARunState, "")
	clockState := clock.State{ReplicaID: "local", WallTime: 1}
	validator := func([]byte) error { return nil }

	if _, err := NewChange(manifest, Dot{Actor: "writer", Counter: 1}, delta); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("NewChange without explicit policy = %v, want ErrInvalidChange", err)
	}
	if _, err := NewChangeWithPolicy(manifest, Dot{Actor: "writer", Counter: 1}, delta, policy); err != nil {
		t.Fatalf("NewChange with explicit policy: %v", err)
	}
	if _, err := NewInbox(manifest, frontier, 1, 1024, func([]byte) error { return nil }); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("NewInbox without explicit policy = %v, want ErrInvalidChange", err)
	}
	if _, err := NewInboxWithPolicy(manifest, frontier, 1, 1024, func([]byte) error { return nil }, policy); err != nil {
		t.Fatalf("NewInbox with explicit policy: %v", err)
	}
	if _, err := NewCheckpoint(manifest, state, frontier, clockState, validator); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("NewCheckpoint without explicit policy = %v, want ErrInvalidCheckpoint", err)
	}
	if _, err := NewCheckpointWithPolicy(manifest, state, frontier, clockState, validator, policy); err != nil {
		t.Fatalf("NewCheckpoint with explicit policy: %v", err)
	}
	if _, err := NewSession(manifest); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("NewSession without explicit policy = %v, want ErrInvalidManifest", err)
	}
	if _, err := NewSessionWithPolicy(manifest, policy); err != nil {
		t.Fatalf("NewSession with explicit policy: %v", err)
	}
}

func TestFrontierAdvancesOnlyContiguousDots(t *testing.T) {
	frontier, err := NewFrontier(nil)
	if err != nil {
		t.Fatalf("NewFrontier: %v", err)
	}
	if _, err := frontier.Advance(Dot{Actor: "a", Counter: 2}); !errors.Is(err, ErrFrontierGap) {
		t.Fatalf("future dot error = %v", err)
	}
	frontier, err = frontier.Advance(Dot{Actor: "a", Counter: 1})
	if err != nil {
		t.Fatalf("first dot: %v", err)
	}
	duplicate, err := frontier.Advance(Dot{Actor: "a", Counter: 1})
	if err != nil || duplicate.Counter("a") != 1 {
		t.Fatalf("duplicate = %#v, %v", duplicate.Entries(), err)
	}
	frontier, err = frontier.Advance(Dot{Actor: "a", Counter: 2})
	if err != nil || !frontier.Covers(Dot{Actor: "a", Counter: 1}) || frontier.Counter("a") != 2 {
		t.Fatalf("second dot = %#v, %v", frontier.Entries(), err)
	}
	entries := frontier.Entries()
	entries["a"] = 99
	if frontier.Counter("a") != 2 {
		t.Fatal("Entries exposed mutable frontier state")
	}
}

func TestCheckpointRequiresConcreteValidationAndCorrectClockBoundary(t *testing.T) {
	manifest, err := NewManifest("counter", "example.com/counter/v1", 1, Protocol{
		StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	state := mustFrame(t, crdt.TypeIDGCounterState, "")
	frontier, err := NewFrontier(map[string]uint64{"writer": 4})
	if err != nil {
		t.Fatalf("NewFrontier: %v", err)
	}
	if _, err := NewCheckpoint(manifest, state, frontier, clock.State{}, nil); !errors.Is(err, ErrNilValidator) {
		t.Fatalf("nil validator error = %v", err)
	}
	validatorErr := errors.New("invalid counter state")
	if _, err := NewCheckpoint(manifest, state, frontier, clock.State{}, func([]byte) error { return validatorErr }); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("invalid concrete state error = %v", err)
	}
	checkpoint, err := NewCheckpoint(manifest, state, frontier, clock.State{}, func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}
	if got := checkpoint.State(); string(got) != string(state) {
		t.Fatalf("checkpoint state = %x, want %x", got, state)
	}

	hlcManifest, err := NewManifest("set", "example.com/set/v1", 1, Protocol{
		StateID: crdt.TypeIDORSetState, DeltaID: crdt.TypeIDORSetDelta, CodecID: "example.com/string/v1", SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatalf("NewManifest HLC: %v", err)
	}
	hlcState := mustFrame(t, crdt.TypeIDORSetState, "example.com/string/v1")
	if _, err := NewCheckpoint(hlcManifest, hlcState, frontier, clock.State{}, func([]byte) error { return nil }); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("missing clock state error = %v", err)
	}
	if _, err := NewCheckpoint(hlcManifest, hlcState, frontier, clock.State{ReplicaID: "local", WallTime: 4, Logical: 2}, func([]byte) error { return nil }); err != nil {
		t.Fatalf("HLC checkpoint: %v", err)
	}
}

func TestSessionAcknowledgesOnlyAfterDurableInstall(t *testing.T) {
	manifest, checkpoint := testCheckpoint(t)
	session, err := NewSession(manifest)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	failed := recordingStore{err: errors.New("disk unavailable")}
	if err := session.Install(checkpoint, &failed); !errors.Is(err, failed.err) {
		t.Fatalf("failed Install error = %v", err)
	}
	if _, err := session.Acknowledge(); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("ack after failed persistence = %v", err)
	}
	stored := recordingStore{}
	if err := session.Install(checkpoint, &stored); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if stored.calls != 1 || stored.checkpoint.ID() != checkpoint.ID() {
		t.Fatalf("store = calls %d, checkpoint %x", stored.calls, stored.checkpoint.ID())
	}
	ack, err := session.Acknowledge()
	if err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if ack.GroupID != manifest.GroupID || ack.Epoch != manifest.Epoch || ack.CheckpointID != checkpoint.ID() || ack.Frontier.Counter("writer") != 1 {
		t.Fatalf("ack = %#v", ack)
	}
	differentManifest := manifest
	differentManifest.Epoch++
	different := checkpoint
	different.manifest = differentManifest
	different.id = different.digest()
	if err := session.Install(different, &stored); !errors.Is(err, ErrManifestMismatch) {
		t.Fatalf("epoch mismatch install = %v", err)
	}
}

func TestSessionSerializesDurableInstallBeforePublication(t *testing.T) {
	manifest, checkpoint := testCheckpoint(t)
	session, err := NewSession(manifest)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	other := checkpoint
	other.state = mustFramePayload(t, crdt.TypeIDGCounterState, "", []byte{2})
	other.id = other.digest()
	store := blockingStore{started: make(chan struct{}), release: make(chan struct{})}
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- session.Install(checkpoint, &store) }()
	<-store.started
	go func() { second <- session.Install(other, &store) }()
	close(store.release)
	if err := <-first; err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := <-second; !errors.Is(err, ErrCheckpointChanged) {
		t.Fatalf("second install: %v", err)
	}
	if store.calls != 1 {
		t.Fatalf("SaveCheckpoint calls = %d, want 1", store.calls)
	}
}

func TestInboxBuffersOutOfOrderFramesWithoutFalsifyingFrontier(t *testing.T) {
	manifest, err := NewManifest("counter", "example.com/counter/v1", 1, Protocol{
		StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	frontier, err := NewFrontier(nil)
	if err != nil {
		t.Fatalf("NewFrontier: %v", err)
	}
	applied := make([]byte, 0, 2)
	inbox, err := NewInbox(manifest, frontier, 2, 1024, func(delta []byte) error {
		decoded, err := frame.UnmarshalFrame(delta, frame.DefaultLimits())
		if err != nil {
			return err
		}
		applied = append(applied, decoded.Payload[0])
		return nil
	})
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	first := mustChange(t, manifest, Dot{Actor: "writer", Counter: 1}, 1)
	second := mustChange(t, manifest, Dot{Actor: "writer", Counter: 2}, 2)
	delivery, err := inbox.Receive(second)
	if err != nil || !delivery.Buffered || len(delivery.Applied) != 0 {
		t.Fatalf("out-of-order delivery = %#v, %v", delivery, err)
	}
	if inbox.Frontier().Counter("writer") != 0 {
		t.Fatal("buffered future change advanced the frontier")
	}
	if changes, _ := inbox.Pending(); changes != 1 {
		t.Fatalf("pending changes = %d, want 1", changes)
	}
	delivery, err = inbox.Receive(first)
	if err != nil || delivery.Buffered || len(delivery.Applied) != 2 || delivery.Applied[0].Counter != 1 || delivery.Applied[1].Counter != 2 {
		t.Fatalf("unblocked delivery = %#v, %v", delivery, err)
	}
	if string(applied) != string([]byte{1, 2}) || inbox.Frontier().Counter("writer") != 2 {
		t.Fatalf("applied = %v, frontier = %#v", applied, inbox.Frontier().Entries())
	}
	if changes, bytes := inbox.Pending(); changes != 0 || bytes != 0 {
		t.Fatalf("pending after drain = %d changes, %d bytes", changes, bytes)
	}
	if delivery, err := inbox.Receive(first); err != nil || delivery.Buffered || len(delivery.Applied) != 0 || len(applied) != 2 {
		t.Fatalf("duplicate delivery = %#v, %v, applied %v", delivery, err, applied)
	}
}

func TestInboxRejectsConflictsAndDoesNotAdvanceAfterApplyFailure(t *testing.T) {
	manifest, err := NewManifest("counter", "example.com/counter/v1", 1, Protocol{
		StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	frontier, _ := NewFrontier(nil)
	inbox, err := NewInbox(manifest, frontier, 2, 1024, func([]byte) error { return errors.New("reject") })
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	first := mustChange(t, manifest, Dot{Actor: "writer", Counter: 1}, 1)
	if _, err := inbox.Receive(first); err == nil || inbox.Frontier().Counter("writer") != 0 {
		t.Fatalf("failed apply advanced frontier: %v, %#v", err, inbox.Frontier().Entries())
	}
	third := mustChange(t, manifest, Dot{Actor: "writer", Counter: 3}, 3)
	conflictingThird := mustChange(t, manifest, Dot{Actor: "writer", Counter: 3}, 9)
	if delivery, err := inbox.Receive(third); err != nil || !delivery.Buffered {
		t.Fatalf("buffer third = %#v, %v", delivery, err)
	}
	if _, err := inbox.Receive(conflictingThird); !errors.Is(err, ErrDotConflict) {
		t.Fatalf("conflicting dot error = %v", err)
	}
}

func TestReplicaInputAndNilBoundaries(t *testing.T) {
	if _, err := NewFrontier(map[string]uint64{"": 1}); !errors.Is(err, ErrInvalidDot) {
		t.Fatalf("blank frontier actor error = %v", err)
	}
	if _, err := NewFrontier(map[string]uint64{"writer": 0}); !errors.Is(err, ErrInvalidDot) {
		t.Fatalf("zero frontier counter error = %v", err)
	}
	frontier, _ := NewFrontier(nil)
	if _, err := frontier.Advance(Dot{}); !errors.Is(err, ErrInvalidDot) {
		t.Fatalf("invalid dot error = %v", err)
	}
	manifest, err := NewManifest("counter", "example.com/counter/v1", 1, Protocol{
		StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	if err := (Manifest{}).Compatible(manifest); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("invalid local manifest error = %v", err)
	}
	if err := manifest.Compatible(Manifest{}); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("invalid remote manifest error = %v", err)
	}
	if _, err := NewChange(manifest, Dot{}, mustFrame(t, crdt.TypeIDGCounterDelta, "")); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("invalid change dot error = %v", err)
	}
	if _, err := NewChange(manifest, Dot{Actor: "writer", Counter: 1}, mustFrame(t, crdt.TypeIDGCounterState, "")); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("wrong change frame error = %v", err)
	}
	if _, err := NewInbox(manifest, frontier, 1, 1, nil); !errors.Is(err, ErrNilApply) {
		t.Fatalf("nil apply error = %v", err)
	}
	if _, err := NewInbox(manifest, frontier, 0, 1, func([]byte) error { return nil }); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("invalid inbox limits error = %v", err)
	}
	var nilInbox *Inbox
	if _, err := nilInbox.Receive(Change{}); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("nil inbox receive error = %v", err)
	}
	if nilInbox.Frontier().Counter("writer") != 0 {
		t.Fatal("nil inbox had frontier")
	}
	if changes, bytes := nilInbox.Pending(); changes != 0 || bytes != 0 {
		t.Fatalf("nil inbox pending = %d, %d", changes, bytes)
	}
	if _, err := NewSession(Manifest{}); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("invalid session manifest error = %v", err)
	}
	var nilSession *Session
	if _, err := nilSession.Acknowledge(); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("nil session acknowledge error = %v", err)
	}
}

func TestInboxPendingLimitsAndDelayedApplyFailure(t *testing.T) {
	manifest, err := NewManifest("counter", "example.com/counter/v1", 1, Protocol{
		StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	frontier, _ := NewFrontier(nil)
	limited, err := NewInbox(manifest, frontier, 1, 1024, func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("NewInbox limited: %v", err)
	}
	second := mustChange(t, manifest, Dot{Actor: "writer", Counter: 2}, 2)
	third := mustChange(t, manifest, Dot{Actor: "writer", Counter: 3}, 3)
	if delivery, err := limited.Receive(second); err != nil || !delivery.Buffered {
		t.Fatalf("first pending change = %#v, %v", delivery, err)
	}
	if delivery, err := limited.Receive(second); err != nil || !delivery.Buffered {
		t.Fatalf("duplicate pending change = %#v, %v", delivery, err)
	}
	if _, err := limited.Receive(third); !errors.Is(err, ErrPendingLimit) {
		t.Fatalf("pending limit error = %v", err)
	}

	failOnce := true
	applied := make([]byte, 0, 2)
	inbox, err := NewInbox(manifest, frontier, 2, 1024, func(delta []byte) error {
		decoded, err := frame.UnmarshalFrame(delta, frame.DefaultLimits())
		if err != nil {
			return err
		}
		if decoded.Payload[0] == 2 && failOnce {
			failOnce = false
			return errors.New("transient apply failure")
		}
		applied = append(applied, decoded.Payload[0])
		return nil
	})
	if err != nil {
		t.Fatalf("NewInbox delayed failure: %v", err)
	}
	first := mustChange(t, manifest, Dot{Actor: "writer", Counter: 1}, 1)
	if _, err := inbox.Receive(second); err != nil {
		t.Fatalf("buffer second: %v", err)
	}
	if delivery, err := inbox.Receive(first); err == nil || len(delivery.Applied) != 1 || inbox.Frontier().Counter("writer") != 1 {
		t.Fatalf("delayed apply result = %#v, %v, frontier %#v", delivery, err, inbox.Frontier().Entries())
	}
	if delivery, err := inbox.Receive(second); err != nil || len(delivery.Applied) != 1 || inbox.Frontier().Counter("writer") != 2 {
		t.Fatalf("retry delayed change = %#v, %v, frontier %#v", delivery, err, inbox.Frontier().Entries())
	}
	if changes, bytes := inbox.Pending(); changes != 0 || bytes != 0 {
		t.Fatalf("retry left stale pending change = %d changes, %d bytes", changes, bytes)
	}
	if string(applied) != string([]byte{1, 2}) {
		t.Fatalf("applied = %v", applied)
	}
}

func TestCheckpointAccessorsValidationAndSessionGuards(t *testing.T) {
	manifest, checkpoint := testCheckpoint(t)
	if got := checkpoint.Manifest(); got != manifest {
		t.Fatalf("Manifest = %#v, want %#v", got, manifest)
	}
	checkpointFrontier := checkpoint.Frontier()
	if checkpointFrontier.Counter("writer") != 1 {
		t.Fatalf("checkpoint frontier = %#v", checkpointFrontier.Entries())
	}
	if state, ok := checkpoint.ClockState(); ok || state != (clock.State{}) {
		t.Fatalf("non-HLC clock state = %#v, %v", state, ok)
	}
	frontier, _ := NewFrontier(map[string]uint64{"writer": 1})
	if _, err := NewCheckpoint(manifest, mustFrame(t, crdt.TypeIDGCounterState, ""), frontier, clock.State{ReplicaID: "not-allowed"}, func([]byte) error { return nil }); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("non-HLC clock state error = %v", err)
	}
	if _, err := NewCheckpoint(manifest, mustFrame(t, crdt.TypeIDGCounterState, ""), frontier, clock.State{}, func([]byte) error { panic("validator panic") }); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("panicking validator error = %v", err)
	}
	hlcManifest, err := NewManifest("set", "example.com/set/v1", 1, Protocol{
		StateID: crdt.TypeIDORSetState, DeltaID: crdt.TypeIDORSetDelta, CodecID: "example.com/string/v1", SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatalf("NewManifest HLC: %v", err)
	}
	hlc, err := NewCheckpoint(hlcManifest, mustFrame(t, crdt.TypeIDORSetState, "example.com/string/v1"), frontier, clock.State{ReplicaID: "local"}, func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("NewCheckpoint HLC: %v", err)
	}
	if state, ok := hlc.ClockState(); !ok || state.ReplicaID != "local" {
		t.Fatalf("HLC clock state = %#v, %v", state, ok)
	}
	session, err := NewSession(manifest)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := session.Install(checkpoint, nil); !errors.Is(err, ErrNilStore) {
		t.Fatalf("nil store error = %v", err)
	}
	if err := session.Install(Checkpoint{}, &recordingStore{}); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("invalid checkpoint error = %v", err)
	}
	store := recordingStore{}
	if err := session.Install(checkpoint, &store); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := session.Install(checkpoint, &store); err != nil || store.calls != 1 {
		t.Fatalf("idempotent install = %v, calls %d", err, store.calls)
	}
}

func TestCheckpointUsesConcreteGCounterValidator(t *testing.T) {
	manifest, err := NewManifest("counter", "example.com/counter/v1", 1, Protocol{
		StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	source, err := counter.NewGCounter("source")
	if err != nil {
		t.Fatalf("NewGCounter source: %v", err)
	}
	if _, err := source.Increment(7); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	frontier, _ := NewFrontier(map[string]uint64{"source": 1})
	validator := func(data []byte) error {
		decoded, err := counter.NewGCounter("validator")
		if err != nil {
			return err
		}
		return decoded.UnmarshalBinary(data)
	}
	checkpoint, err := NewCheckpoint(manifest, state, frontier, clock.State{}, validator)
	if err != nil {
		t.Fatalf("NewCheckpoint concrete G-Counter: %v", err)
	}
	restored, err := counter.NewGCounter("restored")
	if err != nil {
		t.Fatalf("NewGCounter restored: %v", err)
	}
	if err := restored.UnmarshalBinary(checkpoint.State()); err != nil {
		t.Fatalf("UnmarshalBinary checkpoint state: %v", err)
	}
	if value, err := restored.Value(); err != nil || value != 7 {
		t.Fatalf("restored value = %d, %v", value, err)
	}
}

func TestInboxConvergesWithCanonicalGCounterDeltasAcrossDeliveryOrders(t *testing.T) {
	manifest, err := NewManifest("counter", "example.com/counter/v1", 1, Protocol{
		StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	source, err := counter.NewGCounter("source")
	if err != nil {
		t.Fatalf("NewGCounter source: %v", err)
	}
	changes := make([]Change, 0, 3)
	for index, increment := range []uint64{2, 3, 5} {
		delta, err := source.Increment(increment)
		if err != nil {
			t.Fatalf("Increment %d: %v", increment, err)
		}
		encoded, err := delta.MarshalBinary()
		if err != nil {
			t.Fatalf("MarshalBinary delta %d: %v", index, err)
		}
		change, err := NewChange(manifest, Dot{Actor: "source", Counter: uint64(index + 1)}, encoded)
		if err != nil {
			t.Fatalf("NewChange %d: %v", index, err)
		}
		changes = append(changes, change)
	}
	orders := [][]int{{0, 1, 2}, {2, 1, 0}, {1, 2, 0}, {1, 0, 2}, {2, 0, 1}, {0, 2, 1}}
	for _, order := range orders {
		name := string(rune('a'+order[0])) + string(rune('a'+order[1])) + string(rune('a'+order[2]))
		t.Run(name, func(t *testing.T) {
			target, err := counter.NewGCounter("target")
			if err != nil {
				t.Fatalf("NewGCounter target: %v", err)
			}
			frontier, _ := NewFrontier(nil)
			inbox, err := NewInbox(manifest, frontier, 3, 1<<20, func(encoded []byte) error {
				delta, err := counter.UnmarshalGCounterDelta(encoded)
				if err != nil {
					return err
				}
				return target.ApplyDelta(delta)
			})
			if err != nil {
				t.Fatalf("NewInbox: %v", err)
			}
			for _, index := range order {
				if _, err := inbox.Receive(changes[index]); err != nil {
					t.Fatalf("Receive dot %d: %v", index+1, err)
				}
			}
			if value, err := target.Value(); err != nil || value != 10 {
				t.Fatalf("target value = %d, %v", value, err)
			}
			if inbox.Frontier().Counter("source") != 3 {
				t.Fatalf("frontier = %#v", inbox.Frontier().Entries())
			}
			if changes, bytes := inbox.Pending(); changes != 0 || bytes != 0 {
				t.Fatalf("pending = %d changes, %d bytes", changes, bytes)
			}
			if _, err := inbox.Receive(changes[0]); err != nil {
				t.Fatalf("duplicate Receive: %v", err)
			}
			if value, err := target.Value(); err != nil || value != 10 {
				t.Fatalf("duplicate changed value = %d, %v", value, err)
			}
		})
	}
}

func TestInboxRejectsChangesFromAnotherEpoch(t *testing.T) {
	protocol := Protocol{StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1}
	oldManifest, err := NewManifest("counter", "example.com/counter/v1", 1, protocol, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatalf("NewManifest old epoch: %v", err)
	}
	newManifest, err := NewManifest("counter", "example.com/counter/v1", 2, protocol, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatalf("NewManifest new epoch: %v", err)
	}
	oldChange := mustChange(t, oldManifest, Dot{Actor: "source", Counter: 1}, 1)
	frontier, _ := NewFrontier(nil)
	inbox, err := NewInbox(newManifest, frontier, 1, 1024, func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	if _, err := inbox.Receive(oldChange); !errors.Is(err, ErrManifestMismatch) {
		t.Fatalf("old epoch change error = %v", err)
	}
	if inbox.Frontier().Counter("source") != 0 {
		t.Fatal("old epoch change advanced new epoch frontier")
	}
	newChange := mustChange(t, newManifest, Dot{Actor: "source", Counter: 1}, 1)
	if delivery, err := inbox.Receive(newChange); err != nil || len(delivery.Applied) != 1 {
		t.Fatalf("new epoch change = %#v, %v", delivery, err)
	}
}

func TestCheckpointRebaseRejectsOldEpochRGAAnchorsAndParents(t *testing.T) {
	policy := crdt.ProtocolPolicy{AllowExperimental: true}
	protocol := Protocol{StateID: crdt.TypeIDRGAState, DeltaID: crdt.TypeIDRGADelta, SemanticsVersion: 1}
	oldManifest := mustExperimentalManifest(t, "text", "example.com/text/v1", 1, protocol, policy)
	newManifest := mustExperimentalManifest(t, "text", "example.com/text/v1", 2, protocol, policy)

	oldAnchor, err := text.New("old-anchor")
	if err != nil {
		t.Fatal(err)
	}
	anchorDelta, err := oldAnchor.Insert(0, "a")
	if err != nil {
		t.Fatal(err)
	}
	anchor := oldAnchor.Positions()[0]
	tombstoneDelta, err := oldAnchor.Delete(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := oldAnchor.CompactTombstones([]text.Position{anchor}); err != nil || removed != 1 {
		t.Fatalf("CompactTombstones = %d, %v", removed, err)
	}

	oldParent, err := text.New("old-parent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oldParent.Insert(0, "p"); err != nil {
		t.Fatal(err)
	}
	childDelta, err := oldParent.Insert(1, "c")
	if err != nil {
		t.Fatal(err)
	}

	rebased, err := text.New("rebased")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rebased.Insert(0, "fresh"); err != nil {
		t.Fatal(err)
	}
	checkpoint := mustRGARebaseCheckpoint(t, newManifest, rebased, policy)
	recovered := mustRestoreRGA(t, checkpoint)
	inbox := mustRGAInbox(t, newManifest, checkpoint.Frontier(), recovered, policy)

	for _, test := range []struct {
		name   string
		change Change
	}{
		{"compacted old anchor", mustExperimentalChange(t, oldManifest, Dot{Actor: "old-anchor", Counter: 1}, mustMarshalRGADelta(t, anchorDelta), policy)},
		{"old anchor tombstone", mustExperimentalChange(t, oldManifest, Dot{Actor: "old-anchor", Counter: 2}, mustMarshalRGADelta(t, tombstoneDelta), policy)},
		{"old parent reference", mustExperimentalChange(t, oldManifest, Dot{Actor: "old-parent", Counter: 2}, mustMarshalRGADelta(t, childDelta), policy)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := inbox.Receive(test.change); !errors.Is(err, ErrManifestMismatch) {
				t.Fatalf("Receive(old epoch) = %v, want %v", err, ErrManifestMismatch)
			}
		})
	}
	if got := recovered.String(); got != "fresh" {
		t.Fatalf("old epoch revived RGA state = %q", got)
	}
	if pending := recovered.PendingCount(); pending != 0 {
		t.Fatalf("old parent entered RGA pending queue: %d", pending)
	}
	if pending, _ := inbox.Pending(); pending != 0 {
		t.Fatalf("old changes entered Inbox queue: %d", pending)
	}

	future, err := rebased.Insert(5, "!")
	if err != nil {
		t.Fatal(err)
	}
	change := mustExperimentalChange(t, newManifest, Dot{Actor: "rebased", Counter: 1}, mustMarshalRGADelta(t, future), policy)
	if delivery, err := inbox.Receive(change); err != nil || len(delivery.Applied) != 1 {
		t.Fatalf("Receive(new epoch) = %#v, %v", delivery, err)
	}
	if got := recovered.String(); got != "fresh!" {
		t.Fatalf("new epoch RGA text = %q", got)
	}
}

func TestCheckpointRebaseRejectsOldEpochORTreeAnchorsAndParents(t *testing.T) {
	policy := crdt.ProtocolPolicy{AllowExperimental: true}
	protocol := Protocol{StateID: crdt.TypeIDORTreeState, DeltaID: crdt.TypeIDORTreeDelta, SemanticsVersion: 1}
	oldManifest := mustExperimentalManifest(t, "tree", "example.com/tree/v1", 1, protocol, policy)
	newManifest := mustExperimentalManifest(t, "tree", "example.com/tree/v1", 2, protocol, policy)

	old, err := tree.New("old")
	if err != nil {
		t.Fatal(err)
	}
	oldRoot, oldRootDelta, err := old.Add(tree.NodeID{}, []byte("old-root"))
	if err != nil {
		t.Fatal(err)
	}
	_, oldChildDelta, err := old.Add(oldRoot, []byte("old-child"))
	if err != nil {
		t.Fatal(err)
	}
	oldRemoveDelta, err := old.Remove(oldRoot)
	if err != nil {
		t.Fatal(err)
	}

	rebased, err := tree.New("rebased")
	if err != nil {
		t.Fatal(err)
	}
	newRoot, _, err := rebased.Add(tree.NodeID{}, []byte("fresh-root"))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := mustORTreeRebaseCheckpoint(t, newManifest, rebased, policy)
	recovered := mustRestoreORTree(t, checkpoint)
	inbox := mustORTreeInbox(t, newManifest, checkpoint.Frontier(), recovered, policy)

	for _, test := range []struct {
		name   string
		change Change
	}{
		{"old anchor", mustExperimentalChange(t, oldManifest, Dot{Actor: "old", Counter: 1}, mustMarshalORTreeDelta(t, oldRootDelta), policy)},
		{"old parent reference", mustExperimentalChange(t, oldManifest, Dot{Actor: "old", Counter: 2}, mustMarshalORTreeDelta(t, oldChildDelta), policy)},
		{"old anchor tombstone", mustExperimentalChange(t, oldManifest, Dot{Actor: "old", Counter: 3}, mustMarshalORTreeDelta(t, oldRemoveDelta), policy)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := inbox.Receive(test.change); !errors.Is(err, ErrManifestMismatch) {
				t.Fatalf("Receive(old epoch) = %v, want %v", err, ErrManifestMismatch)
			}
		})
	}
	if nodes := recovered.Nodes(); len(nodes) != 1 || string(nodes[0].Value) != "fresh-root" {
		t.Fatalf("old epoch revived OR-Tree nodes = %#v", nodes)
	}
	if pending, _ := inbox.Pending(); pending != 0 {
		t.Fatalf("old changes entered Inbox queue: %d", pending)
	}

	_, future, err := rebased.Add(newRoot, []byte("fresh-child"))
	if err != nil {
		t.Fatal(err)
	}
	change := mustExperimentalChange(t, newManifest, Dot{Actor: "rebased", Counter: 1}, mustMarshalORTreeDelta(t, future), policy)
	if delivery, err := inbox.Receive(change); err != nil || len(delivery.Applied) != 1 {
		t.Fatalf("Receive(new epoch) = %#v, %v", delivery, err)
	}
	if nodes := recovered.Nodes(); len(nodes) != 2 || string(nodes[1].Value) != "fresh-child" {
		t.Fatalf("new epoch OR-Tree nodes = %#v", nodes)
	}
}

func TestCheckpointRecoveryIgnoresPreCheckpointDeltasAndAcceptsFutureDelta(t *testing.T) {
	manifest, err := NewManifest("counter", "example.com/counter/v1", 1, Protocol{
		StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	source, err := counter.NewGCounter("source")
	if err != nil {
		t.Fatalf("NewGCounter source: %v", err)
	}
	changes := make([]Change, 0, 3)
	for index, increment := range []uint64{2, 3, 5} {
		delta, err := source.Increment(increment)
		if err != nil {
			t.Fatalf("Increment %d: %v", increment, err)
		}
		encoded, err := delta.MarshalBinary()
		if err != nil {
			t.Fatalf("MarshalBinary delta %d: %v", index, err)
		}
		changes = append(changes, mustEncodedChange(t, manifest, Dot{Actor: "source", Counter: uint64(index + 1)}, encoded))
	}
	state, err := source.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary state: %v", err)
	}
	frontier, _ := NewFrontier(map[string]uint64{"source": 3})
	validator := func(data []byte) error {
		candidate, err := counter.NewGCounter("validator")
		if err != nil {
			return err
		}
		return candidate.UnmarshalBinary(data)
	}
	checkpoint, err := NewCheckpoint(manifest, state, frontier, clock.State{}, validator)
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}
	recovered, err := counter.NewGCounter("recovered")
	if err != nil {
		t.Fatalf("NewGCounter recovered: %v", err)
	}
	if err := recovered.UnmarshalBinary(checkpoint.State()); err != nil {
		t.Fatalf("UnmarshalBinary checkpoint: %v", err)
	}
	inbox, err := NewInbox(manifest, checkpoint.Frontier(), 2, 1024, func(encoded []byte) error {
		delta, err := counter.UnmarshalGCounterDelta(encoded)
		if err != nil {
			return err
		}
		return recovered.ApplyDelta(delta)
	})
	if err != nil {
		t.Fatalf("NewInbox recovered: %v", err)
	}
	for _, change := range changes {
		if delivery, err := inbox.Receive(change); err != nil || len(delivery.Applied) != 0 {
			t.Fatalf("pre-checkpoint delivery = %#v, %v", delivery, err)
		}
	}
	future, err := source.Increment(7)
	if err != nil {
		t.Fatalf("future Increment: %v", err)
	}
	futureEncoded, err := future.MarshalBinary()
	if err != nil {
		t.Fatalf("future MarshalBinary: %v", err)
	}
	if delivery, err := inbox.Receive(mustEncodedChange(t, manifest, Dot{Actor: "source", Counter: 4}, futureEncoded)); err != nil || len(delivery.Applied) != 1 {
		t.Fatalf("future delivery = %#v, %v", delivery, err)
	}
	if value, err := recovered.Value(); err != nil || value != 17 {
		t.Fatalf("recovered value = %d, %v", value, err)
	}
}

func FuzzInboxHandlesUntrustedChangesWithoutPanic(f *testing.F) {
	manifest, err := NewManifest("counter", "example.com/counter/v1", 1, Protocol{
		StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		f.Fatal(err)
	}
	f.Add([]byte("not a frame"))
	f.Add(mustFrameForFuzz(f, crdt.TypeIDGCounterDelta, "", []byte{0}))
	f.Fuzz(func(t *testing.T, data []byte) {
		frontier, err := NewFrontier(nil)
		if err != nil {
			t.Fatal(err)
		}
		target, err := counter.NewGCounter("target")
		if err != nil {
			t.Fatal(err)
		}
		inbox, err := NewInbox(manifest, frontier, 2, frame.DefaultLimits().MaxFrameBytes, func(encoded []byte) error {
			delta, err := counter.UnmarshalGCounterDelta(encoded)
			if err != nil {
				return err
			}
			return target.ApplyDelta(delta)
		})
		if err != nil {
			t.Fatal(err)
		}
		change := Change{Dot: Dot{Actor: "source", Counter: 1}, manifest: manifest, delta: append([]byte(nil), data...)}
		_, _ = inbox.Receive(change)
		if pending, bytes := inbox.Pending(); pending < 0 || bytes < 0 {
			t.Fatalf("negative pending accounting: %d changes, %d bytes", pending, bytes)
		}
		if counter := inbox.Frontier().Counter("source"); counter > 1 {
			t.Fatalf("single input advanced frontier to %d", counter)
		}
	})
}

type recordingStore struct {
	calls      int
	checkpoint Checkpoint
	err        error
}

type blockingStore struct {
	calls   int
	started chan struct{}
	release chan struct{}
}

func (s *blockingStore) SaveCheckpoint(Checkpoint) error {
	s.calls++
	close(s.started)
	<-s.release
	return nil
}

func (s *recordingStore) SaveCheckpoint(checkpoint Checkpoint) error {
	s.calls++
	if s.err != nil {
		return s.err
	}
	s.checkpoint = checkpoint
	return nil
}

func testCheckpoint(t *testing.T) (Manifest, Checkpoint) {
	t.Helper()
	manifest, err := NewManifest("counter", "example.com/counter/v1", 3, Protocol{
		StateID: crdt.TypeIDGCounterState, DeltaID: crdt.TypeIDGCounterDelta, SemanticsVersion: 1,
	}, crdt.ProtocolPolicy{})
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	frontier, err := NewFrontier(map[string]uint64{"writer": 1})
	if err != nil {
		t.Fatalf("NewFrontier: %v", err)
	}
	checkpoint, err := NewCheckpoint(manifest, mustFrame(t, crdt.TypeIDGCounterState, ""), frontier, clock.State{}, func([]byte) error { return nil })
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}
	return manifest, checkpoint
}

func mustExperimentalManifest(t testing.TB, groupID, schemaID string, epoch uint64, protocol Protocol, policy crdt.ProtocolPolicy) Manifest {
	t.Helper()
	manifest, err := NewManifest(groupID, schemaID, epoch, protocol, policy)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func mustExperimentalChange(t testing.TB, manifest Manifest, dot Dot, delta []byte, policy crdt.ProtocolPolicy) Change {
	t.Helper()
	change, err := NewChangeWithPolicy(manifest, dot, delta, policy)
	if err != nil {
		t.Fatal(err)
	}
	return change
}

func mustMarshalRGADelta(t testing.TB, delta text.Delta) []byte {
	t.Helper()
	encoded, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func mustRGARebaseCheckpoint(t testing.TB, manifest Manifest, source *text.RGA, policy crdt.ProtocolPolicy) Checkpoint {
	t.Helper()
	state, clockState, err := source.MarshalBinaryWithClockState()
	if err != nil {
		t.Fatal(err)
	}
	frontier, err := NewFrontier(nil)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := NewCheckpointWithPolicy(manifest, state, frontier, clockState, func(data []byte) error {
		candidate, err := text.New("validator")
		if err != nil {
			return err
		}
		return candidate.UnmarshalBinary(data)
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	return checkpoint
}

func mustRestoreRGA(t testing.TB, checkpoint Checkpoint) *text.RGA {
	t.Helper()
	clockState, ok := checkpoint.ClockState()
	if !ok {
		t.Fatal("RGA checkpoint omitted HLC state")
	}
	restored, err := text.NewFromClock(clockState)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.UnmarshalBinary(checkpoint.State()); err != nil {
		t.Fatal(err)
	}
	return restored
}

func mustRGAInbox(t testing.TB, manifest Manifest, frontier Frontier, target *text.RGA, policy crdt.ProtocolPolicy) *Inbox {
	t.Helper()
	inbox, err := NewInboxWithPolicy(manifest, frontier, 8, frame.DefaultLimits().MaxFrameBytes, func(encoded []byte) error {
		delta, err := text.UnmarshalRGADelta(encoded)
		if err != nil {
			return err
		}
		return target.ApplyDelta(delta)
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	return inbox
}

func mustMarshalORTreeDelta(t testing.TB, delta tree.Delta) []byte {
	t.Helper()
	encoded, err := delta.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func mustORTreeRebaseCheckpoint(t testing.TB, manifest Manifest, source *tree.ORTree, policy crdt.ProtocolPolicy) Checkpoint {
	t.Helper()
	state, clockState, err := source.MarshalBinaryWithClockState()
	if err != nil {
		t.Fatal(err)
	}
	frontier, err := NewFrontier(nil)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := NewCheckpointWithPolicy(manifest, state, frontier, clockState, func(data []byte) error {
		candidate, err := tree.New("validator")
		if err != nil {
			return err
		}
		return candidate.UnmarshalBinary(data)
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	return checkpoint
}

func mustRestoreORTree(t testing.TB, checkpoint Checkpoint) *tree.ORTree {
	t.Helper()
	clockState, ok := checkpoint.ClockState()
	if !ok {
		t.Fatal("OR-Tree checkpoint omitted HLC state")
	}
	restored, err := tree.NewFromClock(clockState)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.UnmarshalBinary(checkpoint.State()); err != nil {
		t.Fatal(err)
	}
	return restored
}

func mustORTreeInbox(t testing.TB, manifest Manifest, frontier Frontier, target *tree.ORTree, policy crdt.ProtocolPolicy) *Inbox {
	t.Helper()
	inbox, err := NewInboxWithPolicy(manifest, frontier, 8, frame.DefaultLimits().MaxFrameBytes, func(encoded []byte) error {
		delta, err := tree.UnmarshalDelta(encoded)
		if err != nil {
			return err
		}
		return target.ApplyDelta(delta)
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	return inbox
}

func mustFrame(t testing.TB, typeID uint64, codecID string) []byte {
	return mustFramePayload(t, typeID, codecID, []byte{1})
}

func mustFramePayload(t testing.TB, typeID uint64, codecID string, payload []byte) []byte {
	t.Helper()
	encoded, err := frame.MarshalFrame(frame.Frame{TypeID: typeID, CodecID: codecID, Payload: payload})
	if err != nil {
		t.Fatalf("MarshalFrame: %v", err)
	}
	return encoded
}

func mustChange(t testing.TB, manifest Manifest, dot Dot, payload byte) Change {
	t.Helper()
	return mustEncodedChange(t, manifest, dot, mustFramePayload(t, manifest.Protocol.DeltaID, manifest.Protocol.CodecID, []byte{payload}))
}

func mustEncodedChange(t testing.TB, manifest Manifest, dot Dot, encoded []byte) Change {
	t.Helper()
	change, err := NewChange(manifest, dot, encoded)
	if err != nil {
		t.Fatalf("NewChange: %v", err)
	}
	return change
}

func mustFrameForFuzz(f *testing.F, typeID uint64, codecID string, payload []byte) []byte {
	f.Helper()
	encoded, err := frame.MarshalFrame(frame.Frame{TypeID: typeID, CodecID: codecID, Payload: payload})
	if err != nil {
		f.Fatal(err)
	}
	return encoded
}
