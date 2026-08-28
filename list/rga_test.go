package list

import (
	"bytes"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/im10furry/crdt"
	frame "github.com/im10furry/crdt/encoding"
)

type stringCodec struct{}

func (stringCodec) ID() string { return "list-test-string/v1" }

func (stringCodec) Marshal(value string) ([]byte, error) {
	if !utf8.ValidString(value) {
		return nil, errors.New("invalid utf-8")
	}
	return []byte(value), nil
}

func (stringCodec) Unmarshal(value []byte) (string, error) {
	if !utf8.Valid(value) {
		return "", errors.New("invalid utf-8")
	}
	return string(value), nil
}

type nonCanonicalCodec struct{}

func (nonCanonicalCodec) ID() string { return "list-test-noncanonical/v1" }
func (nonCanonicalCodec) Marshal(value string) ([]byte, error) {
	return []byte(strings.ToLower(value)), nil
}
func (nonCanonicalCodec) Unmarshal(value []byte) (string, error) { return string(value), nil }

func TestRGAConvergesAcrossDuplicateOutOfOrderDelivery(t *testing.T) {
	left := mustList(t, "left")
	right := mustList(t, "right")
	base := mustInsert(t, left, 0, []string{"a", "b"})
	if err := right.ApplyDelta(base); err != nil {
		t.Fatal(err)
	}
	leftEdit := mustInsert(t, left, 2, []string{"left"})
	rightEdit := mustInsert(t, right, 2, []string{"right"})
	deleteEdit, err := left.Delete(0, 1)
	if err != nil {
		t.Fatal(err)
	}

	changes := []Delta{base, leftEdit, rightEdit, deleteEdit}
	deliver := func(target *RGA[string], seed int64) {
		t.Helper()
		frames := make([][]byte, 0, len(changes)*2)
		for _, change := range changes {
			encoded, err := change.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			frames = append(frames, encoded, encoded)
		}
		random := rand.New(rand.NewSource(seed))
		random.Shuffle(len(frames), func(left, right int) { frames[left], frames[right] = frames[right], frames[left] })
		for _, encoded := range frames {
			change, err := UnmarshalDelta(encoded, stringCodec{})
			if err != nil {
				t.Fatal(err)
			}
			if err := target.ApplyDelta(change); err != nil {
				t.Fatal(err)
			}
		}
	}
	deliver(left, 1)
	deliver(right, 2)
	leftValues := mustValues(t, left)
	rightValues := mustValues(t, right)
	if len(leftValues) != 3 || !sameStrings(leftValues, rightValues) {
		t.Fatalf("convergence left=%q right=%q", leftValues, rightValues)
	}
	if left.PendingCount() != 0 || right.PendingCount() != 0 {
		t.Fatalf("pending left=%d right=%d", left.PendingCount(), right.PendingCount())
	}
}

func TestStableFrameTypeUsesGenericListV1Contract(t *testing.T) {
	if SemanticsVersion != 1 {
		t.Fatalf("SemanticsVersion = %d, want 1", SemanticsVersion)
	}
	if got, want := StableFrameType(), (crdt.FrameType{StateID: crdt.TypeIDListRGAState, DeltaID: crdt.TypeIDListRGADelta, SemanticsVersion: SemanticsVersion, UsesHLC: true}); got != want {
		t.Fatalf("StableFrameType() = %#v, want %#v", got, want)
	}
}

func TestRGADeleteBeforeInsertAndSnapshotRecovery(t *testing.T) {
	source := mustList(t, "source")
	insert := mustInsert(t, source, 0, []string{"one", "two"})
	deleteDelta, err := source.Delete(0, 1)
	if err != nil {
		t.Fatal(err)
	}
	target := mustList(t, "target")
	if err := target.ApplyDelta(deleteDelta); err != nil {
		t.Fatal(err)
	}
	if got := mustValues(t, target); len(got) != 0 {
		t.Fatalf("before insert = %q", got)
	}
	if err := target.ApplyDelta(insert); err != nil {
		t.Fatal(err)
	}
	if got, want := mustValues(t, target), []string{"two"}; !sameStrings(got, want) {
		t.Fatalf("after insert = %q, want %q", got, want)
	}
	saved, err := target.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewFromSnapshot(saved, stringCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := mustValues(t, recovered), []string{"two"}; !sameStrings(got, want) {
		t.Fatalf("recovered = %q, want %q", got, want)
	}
}

func TestRGARejectsNonCanonicalValuesWithoutMutation(t *testing.T) {
	value, err := New("writer", nonCanonicalCodec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Insert(0, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	state, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	invalid := Delta{
		codecID: value.codecID,
		nodes: map[Position]node{
			{ReplicaID: "remote", WallTime: 1}: {value: []byte("A")},
		},
		tombstones: map[Position]struct{}{},
	}
	if err := value.ApplyDelta(invalid); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("ApplyDelta(noncanonical) = %v", err)
	}
	if after, err := value.MarshalBinary(); err != nil || !bytes.Equal(state, after) {
		t.Fatalf("rejected codec mutated state: %v", err)
	}
}

func TestRGARejectsMalformedFrameWithoutMutation(t *testing.T) {
	value := mustList(t, "writer")
	mustInsert(t, value, 0, []string{"safe"})
	before, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := value.UnmarshalBinary([]byte("not a frame")); err == nil {
		t.Fatal("UnmarshalBinary accepted malformed frame")
	}
	after, err := value.MarshalBinary()
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("malformed frame mutated state: %v", err)
	}
	limits := frame.DefaultLimits()
	limits.MaxPayload = 1
	if _, err := value.MarshalBinaryWithLimits(limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("MarshalBinaryWithLimits = %v", err)
	}
}

func TestRGACompactionRequiresLeaves(t *testing.T) {
	value := mustList(t, "writer")
	if _, err := value.Insert(0, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := value.Delete(0, 2); err != nil {
		t.Fatal(err)
	}
	if removed, err := value.CompactTombstones(value.TombstoneTags()); !errors.Is(err, ErrUnsafeCompaction) || removed != 0 {
		t.Fatalf("CompactTombstones all = %d, %v", removed, err)
	}
	tags := value.TombstoneTags()
	if removed, err := value.CompactTombstones([]Position{tags[len(tags)-1]}); err != nil || removed != 1 {
		t.Fatalf("CompactTombstones leaf = %d, %v", removed, err)
	}
	if removed, err := value.CompactTombstones([]Position{tags[0]}); err != nil || removed != 1 {
		t.Fatalf("CompactTombstones parent = %d, %v", removed, err)
	}
}

func mustList(t testing.TB, replicaID string) *RGA[string] {
	t.Helper()
	value, err := New(replicaID, stringCodec{})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustInsert(t testing.TB, value *RGA[string], offset int, values []string) Delta {
	t.Helper()
	delta, err := value.Insert(offset, values)
	if err != nil {
		t.Fatal(err)
	}
	return delta
}

func mustValues(t testing.TB, value *RGA[string]) []string {
	t.Helper()
	values, err := value.Values()
	if err != nil {
		t.Fatal(err)
	}
	return values
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func FuzzRGAUnmarshal(f *testing.F) {
	value, err := New("seed", stringCodec{})
	if err != nil {
		f.Fatal(err)
	}
	if _, err := value.Insert(0, []string{"seed"}); err != nil {
		f.Fatal(err)
	}
	state, err := value.MarshalBinary()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(state)
	f.Fuzz(func(t *testing.T, data []byte) {
		target := mustList(t, "target")
		if err := target.UnmarshalBinary(data); err == nil && target.State().ElementCount < 0 {
			t.Fatal("negative visible count")
		}
		if delta, err := UnmarshalDelta(data, stringCodec{}); err == nil {
			if err := target.ApplyDelta(delta); err != nil {
				t.Fatalf("decoded delta rejected: %v", err)
			}
		}
	})
}
