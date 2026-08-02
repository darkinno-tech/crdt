package text

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestAnchorBinaryRoundTripIsCanonical(t *testing.T) {
	document := mustRGA(t, "anchor-codec")
	if _, err := document.Insert(0, "abcd"); err != nil {
		t.Fatal(err)
	}
	middle, err := document.AnchorAt(2)
	if err != nil {
		t.Fatal(err)
	}
	for _, anchor := range []Anchor{
		{Association: AnchorBefore},
		middle,
		{Association: AnchorAfter},
	} {
		encoded, err := anchor.MarshalBinary()
		if err != nil {
			t.Fatalf("MarshalBinary(%#v) = %v", anchor, err)
		}
		decoded, err := UnmarshalAnchor(encoded)
		if err != nil {
			t.Fatalf("UnmarshalAnchor(%x) = %v", encoded, err)
		}
		if decoded != anchor {
			t.Fatalf("UnmarshalAnchor(%x) = %#v, want %#v", encoded, decoded, anchor)
		}
		reencoded, err := decoded.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(reencoded, encoded) {
			t.Fatalf("reencoded anchor = %x, want %x", reencoded, encoded)
		}
	}

	fixed := Anchor{Position: Position{ReplicaID: "a", WallTime: 2, Logical: 3}, Association: AnchorBefore}
	encoded, err := fixed.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{1, 1, 1, 1, 'a', 2, 3}; !bytes.Equal(encoded, want) {
		t.Fatalf("fixed anchor encoding = %x, want %x", encoded, want)
	}
}

func TestAnchorRangeBinaryRoundTripPreservesSelectionDirection(t *testing.T) {
	document := mustRGA(t, "anchor-range-codec")
	if _, err := document.Insert(0, "abcd"); err != nil {
		t.Fatal(err)
	}
	anchors, err := document.AnchorRangeAt(3, 1)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := anchors.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalAnchorRange(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != anchors {
		t.Fatalf("UnmarshalAnchorRange() = %#v, want %#v", decoded, anchors)
	}
	if _, err := document.Insert(0, "X"); err != nil {
		t.Fatal(err)
	}
	start, end, err := document.ResolveAnchorRange(decoded)
	if err != nil || start != 4 || end != 2 {
		t.Fatalf("ResolveAnchorRange() = %d, %d, %v; want 4, 2, nil", start, end, err)
	}

	rootRange := AnchorRange{Start: Anchor{Association: AnchorBefore}, End: Anchor{Association: AnchorAfter}}
	rootEncoded, err := rootRange.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{1, 1, 0, 2, 0}; !bytes.Equal(rootEncoded, want) {
		t.Fatalf("root range encoding = %x, want %x", rootEncoded, want)
	}
}

func TestAnchorBinaryRejectsMalformedAndOversizedMetadata(t *testing.T) {
	valid, err := (Anchor{Association: AnchorBefore}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	for name, encoded := range map[string][]byte{
		"empty":                   nil,
		"unknown-version":         {2, 1, 0},
		"non-canonical-version":   {0x81, 0x00, 1, 0},
		"unknown-association":     {1, 3, 0},
		"unknown-position-marker": {1, 1, 2},
		"trailing":                append(append([]byte(nil), valid...), 0),
		"truncated-tag":           {1, 1, 1, 3, 'a'},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := UnmarshalAnchor(encoded); !errors.Is(err, ErrInvalidAnchor) {
				t.Fatalf("UnmarshalAnchor(%x) = %v, want %v", encoded, err, ErrInvalidAnchor)
			}
		})
	}

	oversized := Anchor{Position: Position{ReplicaID: strings.Repeat("a", 9), WallTime: 1}, Association: AnchorBefore}
	limits := AnchorEncodingLimits{MaxBytes: 16, MaxReplicaIDBytes: 8}
	if _, err := oversized.MarshalBinaryWithLimits(limits); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("MarshalBinaryWithLimits(oversized) = %v, want %v", err, ErrResourceLimit)
	}
	if _, err := UnmarshalAnchorWithLimits([]byte{1, 1, 1, 9, 'a', 'a', 'a', 'a', 'a', 'a', 'a', 'a', 'a', 1, 0}, limits); !errors.Is(err, ErrInvalidAnchor) {
		t.Fatalf("UnmarshalAnchorWithLimits(oversized) = %v, want %v", err, ErrInvalidAnchor)
	}
	if _, err := UnmarshalAnchorRange([]byte{1, 1, 0}); !errors.Is(err, ErrInvalidAnchor) {
		t.Fatalf("UnmarshalAnchorRange(truncated) = %v, want %v", err, ErrInvalidAnchor)
	}
}

func TestAnchorRangeFailsClosedAfterCompaction(t *testing.T) {
	document := mustRGA(t, "anchor-range-gc")
	if _, err := document.Insert(0, "ab"); err != nil {
		t.Fatal(err)
	}
	anchors, err := document.AnchorRangeAt(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	tag := document.Positions()[1]
	if _, err := document.Delete(1, 1); err != nil {
		t.Fatal(err)
	}
	if removed, err := document.CompactTombstones([]Position{tag}); err != nil || removed != 1 {
		t.Fatalf("CompactTombstones() = %d, %v", removed, err)
	}
	if _, _, err := document.ResolveAnchorRange(anchors); !errors.Is(err, ErrAnchorGone) {
		t.Fatalf("ResolveAnchorRange(compacted) = %v, want %v", err, ErrAnchorGone)
	}
}

func FuzzAnchorMetadataUnmarshal(f *testing.F) {
	for _, seed := range [][]byte{
		nil,
		{1, 1, 0},
		{1, 1, 0, 2, 0},
		{0x81, 0x00, 1, 0},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, encoded []byte) {
		if anchor, err := UnmarshalAnchor(encoded); err == nil {
			reencoded, err := anchor.MarshalBinary()
			if err != nil || !bytes.Equal(reencoded, encoded) {
				t.Fatalf("anchor canonical round trip = %x, %v", reencoded, err)
			}
		}
		if anchors, err := UnmarshalAnchorRange(encoded); err == nil {
			reencoded, err := anchors.MarshalBinary()
			if err != nil || !bytes.Equal(reencoded, encoded) {
				t.Fatalf("range canonical round trip = %x, %v", reencoded, err)
			}
		}
	})
}
