package attachment

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand"
	"reflect"
	"sync"
	"testing"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/clock"
	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/lww"
	"github.com/im10furry/crdt/replica"
)

func TestRegisterThreeReplicaMediaSessionOverUnreliableNetwork(t *testing.T) {
	alice := mustRegister(t, "alice")
	bob := mustRegister(t, "bob")
	carol := mustRegister(t, "carol")

	cover := testReference("cover-image", "image/png", 2_048)
	coverDelta, err := alice.Put("cover", cover)
	if err != nil {
		t.Fatal(err)
	}
	if err := bob.ApplyDelta(coverDelta); err != nil {
		t.Fatal(err)
	}
	audioDelta, err := bob.Put("ambient", testReference("ambient-audio", "audio/ogg", 4_096))
	if err != nil {
		t.Fatal(err)
	}
	videoDelta, err := carol.Put("intro", testReference("intro-video", "video/mp4", 8_192))
	if err != nil {
		t.Fatal(err)
	}
	dataDelta, err := alice.Put("dataset", testReference("dataset", "application/octet-stream", 16_384))
	if err != nil {
		t.Fatal(err)
	}
	deleteDelta, err := alice.Delete("cover")
	if err != nil {
		t.Fatal(err)
	}

	changes := []Delta{coverDelta, audioDelta, videoDelta, dataDelta, deleteDelta}
	for index, target := range []*Register{alice, bob, carol} {
		deliverAttachmentChanges(t, target, changes, int64(20260729+index))
	}
	wantKeys := []string{"ambient", "dataset", "intro"}
	for _, target := range []*Register{alice, bob, carol} {
		if got := target.Keys(); !reflect.DeepEqual(got, wantKeys) {
			t.Fatalf("keys = %#v, want %#v", got, wantKeys)
		}
		if _, ok := target.Get("cover"); ok {
			t.Fatal("delete did not win after duplicate and reordered delivery")
		}
		for _, key := range wantKeys {
			if _, ok := target.Get(key); !ok {
				t.Fatalf("missing replicated %s reference", key)
			}
		}
	}

	saved, err := bob.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := NewFromSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	if got := recovered.Keys(); !reflect.DeepEqual(got, wantKeys) {
		t.Fatalf("recovered keys = %#v, want %#v", got, wantKeys)
	}
	encoded, err := recovered.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("raw-video-or-audio-bytes")) {
		t.Fatal("attachment state contains media bytes rather than only references")
	}
}

func TestRegisterWireGoldenAndBoundaryRejectionAreAtomic(t *testing.T) {
	digest := sha256.Sum256([]byte("image object"))
	// Build the descriptor and the enclosing LWW-Map frame independently of
	// Register.Put and Delta.MarshalBinary to lock the wire shape down.
	descriptor := frame.AppendUvarint(nil, descriptorVersion)
	descriptor = frame.AppendUvarint(descriptor, uint64(len("obj-1")))
	descriptor = append(descriptor, "obj-1"...)
	descriptor = frame.AppendUvarint(descriptor, uint64(len("image/png")))
	descriptor = append(descriptor, "image/png"...)
	descriptor = frame.AppendUvarint(descriptor, 128)
	descriptor = append(descriptor, digest[:]...)
	payload := frame.AppendUvarint(nil, 1)
	payload = frame.AppendUvarint(payload, 5)
	payload = append(payload, "cover"...)
	payload = frame.AppendTag(payload, crdt.Tag{ReplicaID: "remote", WallTime: 7, Logical: 2})
	payload = frame.AppendUvarint(payload, 1)
	payload = frame.AppendUvarint(payload, uint64(len(descriptor)))
	payload = append(payload, descriptor...)
	golden, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDLWWMapDelta, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	change, err := UnmarshalDelta(golden)
	if err != nil {
		t.Fatal(err)
	}
	if encoded, err := change.MarshalBinary(); err != nil || !bytes.Equal(encoded, golden) {
		t.Fatalf("canonical delta = %x, %v; want %x", encoded, err, golden)
	}
	target := mustRegister(t, "target")
	if err := target.ApplyDelta(change); err != nil {
		t.Fatal(err)
	}
	if got, ok := target.Get("cover"); !ok || got != (Reference{ObjectID: "obj-1", MediaType: "image/png", Size: 128, Digest: digest}) {
		t.Fatalf("golden reference = %#v, %v", got, ok)
	}
	before, err := target.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	malformed, err := lww.NewMap("attacker")
	if err != nil {
		t.Fatal(err)
	}
	if err := malformed.Set("cover", []byte{descriptorVersion}); err != nil {
		t.Fatal(err)
	}
	badState, err := malformed.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := target.UnmarshalBinary(badState); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("malformed descriptor state = %v", err)
	}
	after, err := target.MarshalBinary()
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("receiver changed after rejected state: %v", err)
	}
	if _, err := UnmarshalDelta(mustMapDelta(t, "bad", []byte{descriptorVersion})); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("malformed descriptor delta = %v", err)
	}
}

func TestRegisterLimitsAndSchemaValidation(t *testing.T) {
	options := Options{MaxEntries: 1, MaxKeyBytes: 8, MaxObjectIDBytes: 32, MaxObjectBytes: 1 << 10}
	value, err := NewWithOptions("local", options)
	if err != nil {
		t.Fatal(err)
	}
	valid := testReference("object", "application/json", 42)
	first, err := value.Put("one", valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := value.ApplyDelta(first); err != nil {
		t.Fatalf("duplicate at capacity = %v", err)
	}
	if _, err := value.Put("two", valid); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("new entry at capacity = %v", err)
	}
	if _, err := value.Put(" too", valid); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("invalid key = %v", err)
	}
	if _, err := value.Put("bad\nkey", valid); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("control key = %v", err)
	}
	for _, ref := range []Reference{
		{ObjectID: "object", MediaType: "image/png; charset=utf-8", Size: 1, Digest: valid.Digest},
		{ObjectID: "object", MediaType: "IMAGE/PNG", Size: 1, Digest: valid.Digest},
		{ObjectID: "object", MediaType: "image/png", Size: 1},
		{ObjectID: "object", MediaType: "image/png", Size: 1 << 20, Digest: valid.Digest},
		{ObjectID: "bad\nobject", MediaType: "image/png", Size: 1, Digest: valid.Digest},
	} {
		if _, err := value.Put("one", ref); !errors.Is(err, ErrInvalidReference) {
			t.Fatalf("invalid reference %#v: %v", ref, err)
		}
	}
	if _, err := NewWithOptions("local", Options{}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("invalid options = %v", err)
	}
}

func TestRegisterUsesExplicitLWWMapManifestBoundary(t *testing.T) {
	value := mustRegister(t, "writer")
	change, err := value.Put("cover", testReference("manifest", "image/png", 1))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := change.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := replica.NewManifest(
		"document-17/attachments",
		"github.com/im10furry/crdt/attachment-reference/v1",
		1,
		replica.Protocol{
			StateID:          crdt.TypeIDLWWMapState,
			DeltaID:          crdt.TypeIDLWWMapDelta,
			SemanticsVersion: SemanticsVersion,
		},
		crdt.ProtocolPolicy{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replica.NewChangeWithPolicy(manifest, replica.Dot{Actor: "writer", Counter: 1}, encoded, crdt.ProtocolPolicy{}); err != nil {
		t.Fatalf("attachment delta crossed authenticated manifest boundary: %v", err)
	}
}

func TestRegisterConcurrentAccess(t *testing.T) {
	value := mustRegister(t, "local")
	ref := testReference("concurrent", "audio/mpeg", 512)
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for index := 0; index < 100; index++ {
				key := string(rune('a' + worker))
				if _, err := value.Put(key, ref); err != nil {
					t.Errorf("Put() = %v", err)
					return
				}
				if index%3 == 0 {
					if _, err := value.Delete(key); err != nil {
						t.Errorf("Delete() = %v", err)
						return
					}
				}
				_, _ = value.Get(key)
				_, _ = value.MarshalBinary()
			}
		}(worker)
	}
	group.Wait()
	if _, err := value.MarshalBinary(); err != nil {
		t.Fatal(err)
	}
}

func TestReferenceVerifyStreamsExactContent(t *testing.T) {
	content := bytes.Repeat([]byte("attachment-content"), 4_096)
	ref := Reference{
		ObjectID:  "object-verified",
		MediaType: "video/mp4",
		Size:      uint64(len(content)),
		Digest:    sha256.Sum256(content),
	}
	if err := ref.Verify(bytes.NewReader(content)); err != nil {
		t.Fatalf("Verify() = %v", err)
	}
	if err := ref.Verify(bytes.NewReader(content[:len(content)-1])); !errors.Is(err, ErrContentMismatch) {
		t.Fatalf("short content = %v", err)
	}
	if err := ref.Verify(bytes.NewReader(append(append([]byte(nil), content...), 'x'))); !errors.Is(err, ErrContentMismatch) {
		t.Fatalf("oversized content = %v", err)
	}
	tampered := append([]byte(nil), content...)
	tampered[0] ^= 1
	if err := ref.Verify(bytes.NewReader(tampered)); !errors.Is(err, ErrContentMismatch) {
		t.Fatalf("wrong digest = %v", err)
	}
	if err := ref.Verify(nil); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("nil reader = %v", err)
	}
	invalid := ref
	invalid.MediaType = "video/mp4; codecs=hvc1"
	if err := invalid.Verify(bytes.NewReader(content)); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("invalid reference = %v", err)
	}
	readerErr := errors.New("storage unavailable")
	if err := ref.Verify(errorReader{err: readerErr}); !errors.Is(err, readerErr) {
		t.Fatalf("reader error = %v", err)
	}
	if err := ref.Verify(zeroReader{}); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("zero-progress reader = %v", err)
	}
	if err := ref.Verify(invalidCountReader{}); !errors.Is(err, ErrContentMismatch) {
		t.Fatalf("invalid reader count = %v", err)
	}
}

func TestRegisterLifecycleMergeAndErrorPaths(t *testing.T) {
	if _, err := NewFromClockWithOptions(clock.State{}, DefaultOptions()); err == nil {
		t.Fatal("invalid replica clock accepted")
	}
	var nilRegister *Register
	if nilRegister.ClockState() != (clock.State{}) || nilRegister.Keys() != nil || nilRegister.Frontier() != nil {
		t.Fatal("nil accessors")
	}
	if state := nilRegister.State(); state.Type != "attachment-register" {
		t.Fatalf("nil State() = %#v", state)
	}
	if _, err := nilRegister.Put("key", testReference("nil", "image/png", 1)); !errors.Is(err, ErrNilRegister) {
		t.Fatalf("nil Put = %v", err)
	}
	if _, err := nilRegister.Delete("key"); !errors.Is(err, ErrNilRegister) {
		t.Fatalf("nil Delete = %v", err)
	}
	if _, ok := nilRegister.Get("key"); ok {
		t.Fatal("nil Get is visible")
	}
	if err := nilRegister.ApplyDelta(Delta{}); !errors.Is(err, ErrNilRegister) {
		t.Fatalf("nil ApplyDelta = %v", err)
	}
	if err := nilRegister.Merge(nil); !errors.Is(err, ErrNilRegister) {
		t.Fatalf("nil Merge = %v", err)
	}
	if _, err := nilRegister.MarshalBinary(); !errors.Is(err, ErrNilRegister) {
		t.Fatalf("nil MarshalBinary = %v", err)
	}
	if err := nilRegister.UnmarshalBinary(nil); !errors.Is(err, ErrNilRegister) {
		t.Fatalf("nil UnmarshalBinary = %v", err)
	}
	if _, err := nilRegister.Snapshot(nil); !errors.Is(err, ErrNilRegister) {
		t.Fatalf("nil Snapshot = %v", err)
	}
	if _, err := nilRegister.SnapshotCurrentState(); !errors.Is(err, ErrNilRegister) {
		t.Fatalf("nil SnapshotCurrentState = %v", err)
	}

	left := mustRegister(t, "left")
	right := mustRegister(t, "right")
	leftDelta, err := left.Put("image", testReference("left", "image/jpeg", 100))
	if err != nil {
		t.Fatal(err)
	}
	rightDelta, err := right.Put("sound", testReference("right", "audio/wav", 200))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := leftDelta.Merge(rightDelta)
	if err != nil {
		t.Fatal(err)
	}
	third := mustRegister(t, "third")
	if err := third.ApplyDelta(merged); err != nil {
		t.Fatal(err)
	}
	if err := left.Merge(right); err != nil {
		t.Fatal(err)
	}
	if err := left.Merge(left); err != nil {
		t.Fatal(err)
	}
	if got := left.Keys(); !reflect.DeepEqual(got, []string{"image", "sound"}) {
		t.Fatalf("merged keys = %#v", got)
	}
	if left.State().ElementCount != 2 || len(left.Frontier()) != 2 || left.ClockState().ReplicaID != "left" {
		t.Fatalf("state/frontier/clock = %#v %#v %#v", left.State(), left.Frontier(), left.ClockState())
	}
	saved, err := left.Snapshot(map[string]crdt.Tag{"remote": {ReplicaID: "remote", WallTime: 9}})
	if err != nil {
		t.Fatal(err)
	}
	if restored, err := NewFromSnapshotWithOptions(saved, DefaultOptions()); err != nil || !reflect.DeepEqual(restored.Keys(), left.Keys()) {
		t.Fatalf("snapshot restore = %#v, %v", restored, err)
	}
	if _, err := NewFromSnapshotWithOptions(saved, Options{}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("snapshot invalid options = %v", err)
	}
	badSnapshotMap, err := lww.NewMap("bad-snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if err := badSnapshotMap.Set("bad", []byte{descriptorVersion}); err != nil {
		t.Fatal(err)
	}
	badSnapshot, err := badSnapshotMap.SnapshotCurrentState()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewFromSnapshot(badSnapshot); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("malformed snapshot = %v", err)
	}

	badOther := mustRegister(t, "bad")
	if err := badOther.values.Set("bad", []byte{descriptorVersion}); err != nil {
		t.Fatal(err)
	}
	if err := left.Merge(badOther); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("merge malformed reference = %v", err)
	}
	state, err := left.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}

	limited, err := NewWithOptions("limited", Options{MaxEntries: 1, MaxKeyBytes: 32, MaxObjectIDBytes: 64, MaxObjectBytes: 1 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limited.Delete("new"); err != nil {
		t.Fatal(err)
	}
	if _, err := limited.Delete("other"); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("delete new key at capacity = %v", err)
	}
	if err := limited.ApplyDelta(merged); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("apply new entries at capacity = %v", err)
	}
	if err := limited.UnmarshalBinary(state); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("oversized state = %v", err)
	}

	limits := frame.DefaultLimits()
	limits.MaxElements = 0
	if err := right.UnmarshalBinaryWithLimits(state, limits); err == nil {
		t.Fatal("invalid frame limits accepted")
	}
	if _, err := UnmarshalDeltaWithLimits(mustMapDelta(t, "bad", []byte{descriptorVersion}), frame.DefaultLimits(), Options{}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("delta invalid options = %v", err)
	}
}

func testReference(seed, mediaType string, size uint64) Reference {
	return Reference{ObjectID: "object-" + seed, MediaType: mediaType, Size: size, Digest: sha256.Sum256([]byte(seed))}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

type zeroReader struct{}

func (zeroReader) Read([]byte) (int, error) { return 0, nil }

type invalidCountReader struct{}

func (invalidCountReader) Read(data []byte) (int, error) { return len(data) + 1, nil }

func mustRegister(t testing.TB, replicaID string) *Register {
	t.Helper()
	value, err := New(replicaID)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustMapDelta(t testing.TB, key string, value []byte) []byte {
	t.Helper()
	mapValue, err := lww.NewMap("remote")
	if err != nil {
		t.Fatal(err)
	}
	change, err := mapValue.SetWithDelta(key, value)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := change.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func deliverAttachmentChanges(t testing.TB, target *Register, changes []Delta, seed int64) {
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
		change, err := UnmarshalDelta(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if err := target.ApplyDelta(change); err != nil {
			t.Fatal(err)
		}
	}
}
