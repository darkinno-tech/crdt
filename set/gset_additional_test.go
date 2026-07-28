package set

import (
	"bytes"
	"errors"
	"testing"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

type failingGSetCodec struct{ err error }

func (failingGSetCodec) ID() string                       { return "example.com/gset-failing/v1" }
func (c failingGSetCodec) Marshal(string) ([]byte, error) { return nil, c.err }
func (failingGSetCodec) Unmarshal([]byte) (string, error) { return "", errors.New("decode failed") }

func TestGSetGoldenDeltaMergeAndPublicErrorPaths(t *testing.T) {
	codec := stringCodec{id: "example.com/gset-golden/v1"}
	// Construct the payload without GSetDelta.MarshalBinary so the frame layout
	// is independently fixed as count followed by length-prefixed elements.
	payload := frame.AppendUvarint(nil, 1)
	payload = appendBytes(payload, []byte("a"))
	golden, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDGSetDelta, CodecID: codec.ID(), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalGSetDelta(golden, codec)
	if err != nil {
		t.Fatal(err)
	}
	if encoded, err := decoded.MarshalBinary(codec); err != nil || !bytes.Equal(encoded, golden) {
		t.Fatalf("golden re-encoding = %x, %v", encoded, err)
	}
	second, err := NewGSet("second", codec)
	if err != nil {
		t.Fatal(err)
	}
	other, err := second.Add("b")
	if err != nil {
		t.Fatal(err)
	}
	merged, err := decoded.Merge(other)
	if err != nil {
		t.Fatal(err)
	}
	if got := merged.Elements(); len(got) != 2 {
		t.Fatalf("merged delta elements = %#v", got)
	}
	if err := second.ApplyDelta(merged); err != nil || !second.Contains("a") || !second.Contains("b") {
		t.Fatalf("apply merged delta = %v", err)
	}

	otherCodec := stringCodec{id: "example.com/other/v1"}
	mismatched, err := NewGSet("mismatched", otherCodec)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Merge(mismatched); !errors.Is(err, ErrCodecMismatch) {
		t.Fatalf("codec mismatch = %v", err)
	}
	if _, err := NewGSetFromSnapshot("wrong", snapshot.Snapshot{}, codec); !errors.Is(err, ErrInvalidGSetSnap) {
		t.Fatalf("invalid snapshot = %v", err)
	}

	var nilSet *GSet[string]
	if _, err := nilSet.Add("x"); !errors.Is(err, ErrNilGSet) {
		t.Fatalf("nil Add = %v", err)
	}
	if nilSet.Contains("x") || nilSet.Elements() != nil || nilSet.State().Type != "gset" {
		t.Fatal("nil G-Set accessors")
	}
	if err := nilSet.Merge(second); !errors.Is(err, ErrNilGSet) {
		t.Fatalf("nil Merge = %v", err)
	}
	if _, err := nilSet.MarshalBinary(); !errors.Is(err, ErrNilGSet) {
		t.Fatalf("nil MarshalBinary = %v", err)
	}
	if _, err := nilSet.Snapshot(); !errors.Is(err, ErrNilGSet) {
		t.Fatalf("nil Snapshot = %v", err)
	}
	if err := nilSet.UnmarshalBinary(nil); !errors.Is(err, ErrNilGSet) {
		t.Fatalf("nil UnmarshalBinary = %v", err)
	}
}

func TestGSetBoundedDecodeAndMarshalFailuresAreAtomic(t *testing.T) {
	codec := stringCodec{id: "example.com/gset-limits/v1"}
	value, err := NewGSet("local", codec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.Add("safe"); err != nil {
		t.Fatal(err)
	}
	before, err := value.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	limits := frame.DefaultLimits()
	limits.MaxElements = 0
	if err := value.UnmarshalBinaryWithLimits(before, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("element limit = %v", err)
	}
	after, _ := value.MarshalBinary()
	if !bytes.Equal(before, after) {
		t.Fatal("limit rejection mutated G-Set")
	}

	trailingPayload := frame.AppendUvarint(nil, 1)
	trailingPayload = appendBytes(trailingPayload, []byte("a"))
	trailingPayload = append(trailingPayload, 0)
	trailing, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDGSetState, CodecID: codec.ID(), Payload: trailingPayload})
	if err != nil {
		t.Fatal(err)
	}
	if err := value.UnmarshalBinary(trailing); !errors.Is(err, frame.ErrInvalidFrame) {
		t.Fatalf("trailing state = %v", err)
	}

	if _, err := marshalGSet(crdt.TypeIDGSetState, codec, map[string]struct{}{"a": {}}, limits); !errors.Is(err, frame.ErrFrameLimit) {
		t.Fatalf("marshal element limit = %v", err)
	}
	if _, err := marshalGSet(crdt.TypeIDGSetState, failingGSetCodec{err: errors.New("encode failed")}, map[string]struct{}{"a": {}}, frame.DefaultLimits()); !errors.Is(err, ErrInvalidCodec) {
		t.Fatalf("codec failure = %v", err)
	}
	if _, err := UnmarshalGSetDelta(goldenGSetState(t, codec), failingGSetCodec{err: errors.New("encode failed")}); !errors.Is(err, ErrCodecMismatch) {
		t.Fatalf("codec mismatch precedes decode = %v", err)
	}
}

func goldenGSetState(t *testing.T, codec ElementCodec[string]) []byte {
	t.Helper()
	payload := frame.AppendUvarint(nil, 1)
	payload = appendBytes(payload, []byte("a"))
	encoded, err := frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDGSetState, CodecID: codec.ID(), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
