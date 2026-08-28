package text

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"

	frame "github.com/im10furry/crdt/encoding"
)

func TestRGAObfuscatedStatePreservesStructureWithoutMutatingSource(t *testing.T) {
	source := mustRGA(t, "diagnostic-author")
	if _, err := source.Insert(0, "sensitive: 密码 🚀"); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Delete(2, 2); err != nil {
		t.Fatal(err)
	}
	originalText := source.String()
	legacy, err := source.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	run, err := source.MarshalRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	obfuscatedLegacy, err := source.MarshalObfuscatedBinary()
	if err != nil {
		t.Fatal(err)
	}
	obfuscatedRun, err := source.MarshalObfuscatedRunBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(obfuscatedLegacy) != len(legacy) || len(obfuscatedRun) != len(run) {
		t.Fatalf("obfuscated frame sizes = %d/%d, originals = %d/%d", len(obfuscatedLegacy), len(obfuscatedRun), len(legacy), len(run))
	}
	if bytes.Contains(obfuscatedLegacy, []byte("sensitive")) || bytes.Contains(obfuscatedRun, []byte("sensitive")) {
		t.Fatal("obfuscated diagnostic frame retained source text")
	}
	if got := source.String(); got != originalText {
		t.Fatalf("obfuscation mutated source text to %q, want %q", got, originalText)
	}

	legacyState := mustRGA(t, "legacy-debug")
	if err := legacyState.UnmarshalBinary(obfuscatedLegacy); err != nil {
		t.Fatal(err)
	}
	runState := mustRGA(t, "run-debug")
	if err := runState.UnmarshalRunBinary(obfuscatedRun); err != nil {
		t.Fatal(err)
	}
	if legacyState.Len() != source.Len() || runState.Len() != source.Len() {
		t.Fatalf("obfuscated lengths = %d/%d, want %d", legacyState.Len(), runState.Len(), source.Len())
	}
	if legacyState.String() == originalText || runState.String() == originalText {
		t.Fatal("obfuscated state retained visible source text")
	}
}

func TestObfuscatedRunDeltasConvergeOverUnreliableDebugTransport(t *testing.T) {
	alice := mustRGA(t, "alice")
	bob := mustRGA(t, "bob")
	carol := mustRGA(t, "carol")

	base := mustInsertRGA(t, alice, 0, "private draft")
	mustApplyRGA(t, bob, base)
	mustApplyRGA(t, carol, base)
	bobEdit := mustInsertRGA(t, bob, 3, "机密")
	carolEdit := mustInsertRGA(t, carol, carol.Len(), " 🚀")
	mustApplyRGA(t, alice, bobEdit)
	mustApplyRGA(t, alice, carolEdit)
	deleteEdit, err := alice.Delete(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	changes := []Delta{base, bobEdit, carolEdit, deleteEdit}

	first := mustRGA(t, "debug-first")
	second := mustRGA(t, "debug-second")
	for index, target := range []*RGA{first, second} {
		deliverObfuscatedRunChanges(t, target, changes, int64(20260730+index))
		if target.PendingCount() != 0 {
			t.Fatalf("debug target retained %d unresolved dependencies", target.PendingCount())
		}
	}
	if first.String() != second.String() {
		t.Fatalf("obfuscated debug replicas diverged: %q != %q", first.String(), second.String())
	}
	if first.String() == alice.String() {
		t.Fatal("obfuscated debug replica retained original visible content")
	}
	if err := first.ApplyDelta(base); !errors.Is(err, ErrTagConflict) {
		t.Fatalf("original delta after obfuscation error = %v, want ErrTagConflict", err)
	}
}

func TestDeltaObfuscateRejectsInvalidInput(t *testing.T) {
	invalid := Delta{
		nodes:      map[Position]node{{ReplicaID: "writer", WallTime: 1}: {rune: -1}},
		tombstones: map[Position]struct{}{},
	}
	if _, err := invalid.Obfuscate(); !errors.Is(err, ErrInvalidDelta) {
		t.Fatalf("invalid delta obfuscation error = %v, want ErrInvalidDelta", err)
	}
}

func TestObfuscatedScalarDeltaAndStateLimits(t *testing.T) {
	source := mustRGA(t, "scalar-obfuscation")
	delta := mustInsertRGA(t, source, 0, "abc")
	encoded, err := delta.MarshalObfuscatedBinary()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalRGADelta(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.nodes) != 3 {
		t.Fatalf("obfuscated scalar nodes = %d, want 3", len(decoded.nodes))
	}
	tight := frame.DefaultLimits()
	tight.MaxElements = 1
	if _, err := delta.MarshalObfuscatedBinaryWithLimits(tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("bounded scalar delta error = %v", err)
	}
	if _, err := source.MarshalObfuscatedBinaryWithLimits(tight); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("bounded scalar state error = %v", err)
	}
	var nilDocument *RGA
	if _, err := nilDocument.MarshalObfuscatedBinary(); !errors.Is(err, ErrNilText) {
		t.Fatalf("nil scalar state error = %v", err)
	}
	if _, err := nilDocument.MarshalObfuscatedRunBinary(); !errors.Is(err, ErrNilText) {
		t.Fatalf("nil run state error = %v", err)
	}
}

func deliverObfuscatedRunChanges(t testing.TB, target *RGA, changes []Delta, seed int64) {
	t.Helper()
	frames := make([][]byte, 0, len(changes)*2)
	for _, change := range changes {
		encoded, err := change.MarshalObfuscatedRunBinary()
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, encoded, encoded)
	}
	random := rand.New(rand.NewSource(seed))
	random.Shuffle(len(frames), func(left, right int) { frames[left], frames[right] = frames[right], frames[left] })
	for _, encoded := range frames {
		change, err := UnmarshalRGARunDelta(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if err := target.ApplyDelta(change); err != nil {
			t.Fatal(err)
		}
	}
}
