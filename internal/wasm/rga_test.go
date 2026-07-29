package wasm

import (
	"errors"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
	"github.com/DarkInno/crdt/text"
)

func TestRuntimeThreeReplicaUnreliableDeliveryAndRecovery(t *testing.T) {
	testRuntimeThreeReplicaUnreliableDeliveryAndRecovery(t, DefaultRGAOptions(), RGAProtocol{
		StateTypeID: RGAStateTypeID, DeltaTypeID: RGADeltaTypeID, SemanticsVersion: RGASemanticsVersion,
	})
}

func TestRuntimeRunV2ThreeReplicaUnreliableDeliveryAndRecovery(t *testing.T) {
	testRuntimeThreeReplicaUnreliableDeliveryAndRecovery(t, DefaultRunRGAOptions(), RGAProtocol{
		StateTypeID: RGARunStateTypeID, DeltaTypeID: RGARunDeltaTypeID, SemanticsVersion: RGARunSemanticsVersion,
	})
}

// TestRuntimeRunV2InteroperatesWithNativeRGA proves the negotiated frame and
// atomic snapshot contracts at the Go-to-browser-runtime boundary. The Node
// Wasm artifact test exercises the same runtime from JavaScript separately.
func TestRuntimeRunV2InteroperatesWithNativeRGA(t *testing.T) {
	options := DefaultRunRGAOptions()
	runtime, err := NewRuntime(options)
	if err != nil {
		t.Fatal(err)
	}
	wasmHandle := mustCreate(t, runtime, "wasm")
	native, err := text.NewWithOptions("native", options.Text)
	if err != nil {
		t.Fatal(err)
	}

	nativeDelta, err := native.InsertRunBinaryWithLimits(0, "native", options.Decoder)
	if err != nil {
		t.Fatal(err)
	}
	mustApply(t, runtime, wasmHandle, nativeDelta)
	wasmDelta := mustInsert(t, runtime, wasmHandle, len([]rune(mustText(t, runtime, wasmHandle))), " + wasm")
	decoded, err := text.UnmarshalRGARunDeltaWithLimits(wasmDelta, options.Decoder)
	if err != nil {
		t.Fatal(err)
	}
	if err := native.ApplyDelta(decoded); err != nil {
		t.Fatal(err)
	}
	if got, want := mustText(t, runtime, wasmHandle), native.String(); got != want {
		t.Fatalf("native/runtime text = %q, want %q", got, want)
	}

	nativeSnapshot, err := native.SnapshotRunCurrentStateWithLimits(options.Decoder)
	if err != nil {
		t.Fatal(err)
	}
	clockState, ok := nativeSnapshot.ClockState()
	if !ok {
		t.Fatal("native run snapshot is missing clock state")
	}
	recovered, err := runtime.Restore(RGASnapshot{
		State: nativeSnapshot.Bytes(), Frontier: nativeSnapshot.Frontier(), Clock: clockState,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := mustText(t, runtime, recovered), native.String(); got != want {
		t.Fatalf("runtime restored native snapshot = %q, want %q", got, want)
	}

	runtimeSnapshot, err := runtime.Snapshot(wasmHandle)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := snapshot.NewWithClockState(runtimeSnapshot.State, runtimeSnapshot.Frontier, runtimeSnapshot.Clock)
	if err != nil {
		t.Fatal(err)
	}
	nativeRecovered, err := text.NewFromSnapshotWithOptions(saved, options.Text, options.Decoder)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := nativeRecovered.String(), native.String(); got != want {
		t.Fatalf("native restored runtime snapshot = %q, want %q", got, want)
	}
}

func testRuntimeThreeReplicaUnreliableDeliveryAndRecovery(t *testing.T, options RGAOptions, wantProtocol RGAProtocol) {
	t.Helper()
	runtime, err := NewRuntime(options)
	if err != nil {
		t.Fatal(err)
	}
	if got := runtime.Protocol(); got != wantProtocol {
		t.Fatalf("runtime protocol = %#v, want %#v", got, wantProtocol)
	}
	alice := mustCreate(t, runtime, "alice")
	bob := mustCreate(t, runtime, "bob")
	carol := mustCreate(t, runtime, "carol")

	base := mustInsert(t, runtime, alice, 0, "Draft")
	decoded, err := frame.UnmarshalFrame(base, options.Decoder)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.TypeID != wantProtocol.DeltaTypeID {
		t.Fatalf("local delta type = %d, want %d", decoded.TypeID, wantProtocol.DeltaTypeID)
	}
	for _, handle := range []uint64{bob, carol} {
		mustApply(t, runtime, handle, base)
	}
	bobEdit := mustInsert(t, runtime, bob, 5, " for review")
	carolEdit := mustInsert(t, runtime, carol, 5, " collaboratively")
	mustApply(t, runtime, alice, carolEdit)
	mustApply(t, runtime, alice, bobEdit)
	deleteEdit := mustDelete(t, runtime, alice, 1, 2)

	changes := [][]byte{base, bobEdit, carolEdit, deleteEdit}
	for index, handle := range []uint64{alice, bob, carol} {
		deliverDuplicatedAndShuffled(t, runtime, handle, changes, int64(20260729+index))
	}
	want := mustText(t, runtime, alice)
	for _, handle := range []uint64{bob, carol} {
		if got := mustText(t, runtime, handle); got != want {
			t.Fatalf("replica text = %q, want %q", got, want)
		}
		if pending, err := runtime.PendingCount(handle); err != nil || pending != 0 {
			t.Fatalf("replica pending = %d, %v; want 0", pending, err)
		}
	}

	saved, err := runtime.Snapshot(bob)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = frame.UnmarshalFrame(saved.State, options.Decoder)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.TypeID != wantProtocol.StateTypeID {
		t.Fatalf("snapshot state type = %d, want %d", decoded.TypeID, wantProtocol.StateTypeID)
	}
	recovered, err := runtime.Restore(saved)
	if err != nil {
		t.Fatal(err)
	}
	if got := mustText(t, runtime, recovered); got != want {
		t.Fatalf("recovered text = %q, want %q", got, want)
	}
	if _, err := runtime.Insert(recovered, len([]rune(want)), "!"); err != nil {
		t.Fatalf("post-recovery local edit error = %v", err)
	}

	if !runtime.Drop(recovered) {
		t.Fatal("Drop() did not release recovered document")
	}
	if _, err := runtime.Text(recovered); !errors.Is(err, ErrUnknownDocument) {
		t.Fatalf("released handle error = %v, want %v", err, ErrUnknownDocument)
	}
}

func TestRuntimeRejectsUntrustedFramesWithoutMutation(t *testing.T) {
	if _, err := NewRuntime(RGAOptions{}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("zero options error = %v, want %v", err, ErrInvalidOptions)
	}
	unsupportedWire := DefaultRGAOptions()
	unsupportedWire.WireFormat = 0
	if _, err := NewRuntime(unsupportedWire); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("unsupported wire format error = %v, want %v", err, ErrInvalidOptions)
	}
	invalidOptions := DefaultRGAOptions()
	invalidOptions.Decoder.MaxPayload = invalidOptions.Decoder.MaxFrameBytes + 1
	if _, err := NewRuntime(invalidOptions); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("inconsistent decoder options error = %v, want %v", err, ErrInvalidOptions)
	}
	runtime, err := NewRuntime(DefaultRGAOptions())
	if err != nil {
		t.Fatal(err)
	}
	handle := mustCreate(t, runtime, "local")
	if _, err := runtime.Insert(handle, 0, "safe"); err != nil {
		t.Fatal(err)
	}
	before := mustText(t, runtime, handle)

	if err := runtime.ApplyDelta(handle, []byte("bad")); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("short frame error = %v, want %v", err, frame.ErrFrameLimit)
	}
	wrongType, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDGCounterDelta, Payload: []byte{0}})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ApplyDelta(handle, wrongType); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("wrong type error = %v, want %v", err, frame.ErrInvalidFrame)
	}
	overLimit := make([]byte, DefaultRGAOptions().Decoder.MaxFrameBytes+1)
	if err := runtime.ApplyDelta(handle, overLimit); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("oversized frame error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if got := mustText(t, runtime, handle); got != before {
		t.Fatalf("invalid frame changed text to %q, want %q", got, before)
	}
	runState, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRGARunState, Payload: []byte{0}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Restore(RGASnapshot{State: runState, Clock: clock.State{ReplicaID: "local"}}); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("unnegotiated RGA run snapshot error = %v, want %v", err, frame.ErrInvalidFrame)
	}

	saved, err := runtime.Snapshot(handle)
	if err != nil {
		t.Fatal(err)
	}
	saved.State[0] ^= 1
	if _, err := runtime.Restore(saved); err == nil {
		t.Fatal("corrupt snapshot was restored")
	}
	if got := mustText(t, runtime, handle); got != before {
		t.Fatalf("invalid snapshot changed source text to %q, want %q", got, before)
	}
	tooLargeState := saved
	tooLargeState.State = make([]byte, runtime.MaxFrameBytes()+1)
	if _, err := runtime.Restore(tooLargeState); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("oversized snapshot state error = %v, want %v", err, frame.ErrFrameLimit)
	}
	tooLongReplica := saved
	tooLongReplica.Clock.ReplicaID = strings.Repeat("r", runtime.MaxStringBytes()+1)
	if _, err := runtime.Restore(tooLongReplica); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("oversized snapshot replica ID error = %v, want %v", err, frame.ErrFrameLimit)
	}
	tightTags := DefaultRGAOptions()
	tightTags.Decoder.MaxTags = 1
	limitedTags, err := NewRuntime(tightTags)
	if err != nil {
		t.Fatal(err)
	}
	tooManyTags := saved
	tooManyTags.Frontier["second"] = crdt.Tag{ReplicaID: "second"}
	if _, err := limitedTags.Restore(tooManyTags); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("oversized snapshot frontier error = %v, want %v", err, frame.ErrFrameLimit)
	}

	if _, err := runtime.Create(strings.Repeat("r", runtime.options.Decoder.MaxStringBytes+1)); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("oversized replica ID error = %v, want %v", err, frame.ErrFrameLimit)
	}
	outputTight := DefaultRGAOptions()
	outputTight.Decoder.MaxPayload = 1
	bounded, err := NewRuntime(outputTight)
	if err != nil {
		t.Fatal(err)
	}
	boundedHandle := mustCreate(t, bounded, "bounded")
	if _, err := bounded.Insert(boundedHandle, 0, "a"); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("oversized local delta error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if got := mustText(t, bounded, boundedHandle); got != "" {
		t.Fatalf("rejected local delta mutated text to %q", got)
	}
	runOutputTight := DefaultRunRGAOptions()
	runOutputTight.Decoder.MaxPayload = 1
	runBounded, err := NewRuntime(runOutputTight)
	if err != nil {
		t.Fatal(err)
	}
	runBoundedHandle := mustCreate(t, runBounded, "run-bounded")
	if _, err := runBounded.Insert(runBoundedHandle, 0, "a"); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("oversized run local delta error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if got := mustText(t, runBounded, runBoundedHandle); got != "" {
		t.Fatalf("rejected run local delta mutated text to %q", got)
	}
	if _, err := runtime.Insert(handle, 0, strings.Repeat("a", runtime.MaxLocalEditRunes()+1)); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("oversized local edit rune count error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if _, err := runtime.Insert(handle, 0, strings.Repeat("a", runtime.MaxLocalEditBytes()+1)); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("oversized local edit byte count error = %v, want %v", err, frame.ErrFrameLimit)
	}
	if got := mustText(t, runtime, handle); got != before {
		t.Fatalf("rejected local edit changed text to %q, want %q", got, before)
	}
}

func TestRuntimeRejectsCrossWireFramesAndSnapshots(t *testing.T) {
	scalar, err := NewRuntime(DefaultRGAOptions())
	if err != nil {
		t.Fatal(err)
	}
	run, err := NewRuntime(DefaultRunRGAOptions())
	if err != nil {
		t.Fatal(err)
	}
	scalarSource := mustCreate(t, scalar, "scalar-source")
	runSource := mustCreate(t, run, "run-source")
	scalarTarget := mustCreate(t, scalar, "scalar-target")
	runTarget := mustCreate(t, run, "run-target")
	scalarDelta := mustInsert(t, scalar, scalarSource, 0, "scalar")
	runDelta := mustInsert(t, run, runSource, 0, "run")
	if err := scalar.ApplyDelta(scalarTarget, runDelta); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("scalar runtime accepted run-v2 delta: %v", err)
	}
	if err := run.ApplyDelta(runTarget, scalarDelta); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("run-v2 runtime accepted scalar delta: %v", err)
	}
	scalarSnapshot, err := scalar.Snapshot(scalarSource)
	if err != nil {
		t.Fatal(err)
	}
	runSnapshot, err := run.Snapshot(runSource)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scalar.Restore(runSnapshot); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("scalar runtime restored run-v2 snapshot: %v", err)
	}
	if _, err := run.Restore(scalarSnapshot); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("run-v2 runtime restored scalar snapshot: %v", err)
	}
}

func TestRuntimeDefaultLimitsBoundOutboundFrame(t *testing.T) {
	runtime, err := NewRuntime(DefaultRGAOptions())
	if err != nil {
		t.Fatal(err)
	}
	source := mustCreate(t, runtime, "source")
	target := mustCreate(t, runtime, "target")
	delta := mustInsert(t, runtime, source, 0, strings.Repeat("a", 10_000))
	if len(delta) > DefaultRGAOptions().Decoder.MaxFrameBytes {
		t.Fatalf("delta size = %d, limit = %d", len(delta), DefaultRGAOptions().Decoder.MaxFrameBytes)
	}
	mustApply(t, runtime, target, delta)
	if got := mustText(t, runtime, target); got != strings.Repeat("a", 10_000) {
		t.Fatalf("target text length = %d, want 10000", len([]rune(got)))
	}
}

func TestRuntimeBoundaryErrorsAndAccessors(t *testing.T) {
	var nilRuntime *Runtime
	if _, err := nilRuntime.Create("local"); !errors.Is(err, ErrUnknownDocument) {
		t.Fatalf("nil Create() error = %v, want %v", err, ErrUnknownDocument)
	}
	if _, err := nilRuntime.Restore(RGASnapshot{}); !errors.Is(err, ErrUnknownDocument) {
		t.Fatalf("nil Restore() error = %v, want %v", err, ErrUnknownDocument)
	}
	if nilRuntime.Drop(1) || nilRuntime.MaxFrameBytes() != 0 || nilRuntime.MaxTags() != 0 ||
		nilRuntime.MaxStringBytes() != 0 || nilRuntime.MaxLocalEditBytes() != 0 || nilRuntime.MaxLocalEditRunes() != 0 {
		t.Fatal("nil runtime exposed state")
	}

	runtime, err := NewRuntime(DefaultRGAOptions())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.MaxFrameBytes() != 1<<20 || runtime.MaxTags() != 100_000 || runtime.MaxStringBytes() != 64<<10 ||
		runtime.MaxLocalEditBytes() != 64<<10 || runtime.MaxLocalEditRunes() != 16<<10 {
		t.Fatalf("unexpected runtime limits")
	}
	if _, err := runtime.Create(" "); err == nil {
		t.Fatal("blank replica ID was accepted")
	}
	if _, err := runtime.Restore(RGASnapshot{}); err == nil {
		t.Fatal("empty snapshot was restored")
	}
	if runtime.Drop(0) || runtime.Drop(99) {
		t.Fatal("unknown document was dropped")
	}
	for _, operation := range []struct {
		name string
		err  error
	}{
		{"insert", func() error { _, err := runtime.Insert(99, 0, "x"); return err }()},
		{"delete", func() error { _, err := runtime.Delete(99, 0, 1); return err }()},
		{"apply", runtime.ApplyDelta(99, nil)},
		{"pending", func() error { _, err := runtime.PendingCount(99); return err }()},
		{"snapshot", func() error { _, err := runtime.Snapshot(99); return err }()},
	} {
		if !errors.Is(operation.err, ErrUnknownDocument) {
			t.Fatalf("unknown %s error = %v, want %v", operation.name, operation.err, ErrUnknownDocument)
		}
	}

	handle := mustCreate(t, runtime, "boundary")
	if _, err := runtime.Insert(handle, -1, "x"); err == nil {
		t.Fatal("negative insert offset was accepted")
	}
	if _, err := runtime.Delete(handle, -1, 0); err == nil {
		t.Fatal("negative delete offset was accepted")
	}
	if _, err := runtime.Delete(handle, 0, -1); err == nil {
		t.Fatal("negative delete count was accepted")
	}
	saved, err := runtime.Snapshot(handle)
	if err != nil {
		t.Fatal(err)
	}
	runtime.next = math.MaxUint64
	if _, err := runtime.Create("exhausted"); !errors.Is(err, ErrHandleExhausted) {
		t.Fatalf("exhausted Create() error = %v, want %v", err, ErrHandleExhausted)
	}
	if _, err := runtime.Restore(saved); !errors.Is(err, ErrHandleExhausted) {
		t.Fatalf("exhausted Restore() error = %v, want %v", err, ErrHandleExhausted)
	}
}

func mustCreate(t testing.TB, runtime *Runtime, replicaID string) uint64 {
	t.Helper()
	handle, err := runtime.Create(replicaID)
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

func mustInsert(t testing.TB, runtime *Runtime, handle uint64, offset int, value string) []byte {
	t.Helper()
	delta, err := runtime.Insert(handle, offset, value)
	if err != nil {
		t.Fatal(err)
	}
	return delta
}

func mustDelete(t testing.TB, runtime *Runtime, handle uint64, offset, count int) []byte {
	t.Helper()
	delta, err := runtime.Delete(handle, offset, count)
	if err != nil {
		t.Fatal(err)
	}
	return delta
}

func mustApply(t testing.TB, runtime *Runtime, handle uint64, delta []byte) {
	t.Helper()
	if err := runtime.ApplyDelta(handle, delta); err != nil {
		t.Fatal(err)
	}
}

func mustText(t testing.TB, runtime *Runtime, handle uint64) string {
	t.Helper()
	value, err := runtime.Text(handle)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func deliverDuplicatedAndShuffled(t testing.TB, runtime *Runtime, handle uint64, changes [][]byte, seed int64) {
	t.Helper()
	frames := make([][]byte, 0, len(changes)*2)
	for _, change := range changes {
		frames = append(frames, change, change)
	}
	random := rand.New(rand.NewSource(seed))
	random.Shuffle(len(frames), func(left, right int) { frames[left], frames[right] = frames[right], frames[left] })
	for _, encoded := range frames {
		mustApply(t, runtime, handle, encoded)
	}
}
