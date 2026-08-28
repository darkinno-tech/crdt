package text

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/im10furry/crdt"
)

type scalarRGAVectorFile struct {
	Protocol         string `json:"protocol"`
	SemanticsVersion uint64 `json:"semantics_version"`
	FrameTypes       struct {
		State uint64 `json:"state"`
		Delta uint64 `json:"delta"`
	} `json:"frame_types"`
	Vectors []struct {
		Name          string `json:"name"`
		CompleteState bool   `json:"complete_state"`
		Hex           string `json:"hex"`
		Text          string `json:"text"`
	} `json:"vectors"`
}

func TestRGAScalarV1CrossLanguageVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "docs", "protocol", "testdata", "rga-scalar-v1-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors scalarRGAVectorFile
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if vectors.Protocol != "rga-scalar-v1" || vectors.SemanticsVersion != LegacySemanticsVersion || vectors.FrameTypes.State != crdt.TypeIDRGAState || vectors.FrameTypes.Delta != crdt.TypeIDRGADelta {
		t.Fatalf("invalid scalar RGA vector metadata: %#v", vectors)
	}
	for _, vector := range vectors.Vectors {
		encoded, err := hex.DecodeString(vector.Hex)
		if err != nil {
			t.Fatalf("%s hex: %v", vector.Name, err)
		}
		target, err := New("target")
		if err != nil {
			t.Fatal(err)
		}
		var reencoded []byte
		if vector.CompleteState {
			err = target.UnmarshalBinary(encoded)
			if err == nil {
				reencoded, err = target.MarshalBinary()
			}
		} else {
			delta, decodeErr := UnmarshalRGADelta(encoded)
			if decodeErr != nil {
				t.Fatalf("%s decode delta: %v", vector.Name, decodeErr)
			}
			reencoded, err = delta.MarshalBinary()
			if err == nil {
				err = target.ApplyDelta(delta)
			}
		}
		if err != nil || !bytes.Equal(reencoded, encoded) {
			t.Fatalf("%s re-encoding = %x, %v; want %x", vector.Name, reencoded, err, encoded)
		}
		if got := target.String(); got != vector.Text {
			t.Fatalf("%s text = %q, want %q", vector.Name, got, vector.Text)
		}
	}
}
