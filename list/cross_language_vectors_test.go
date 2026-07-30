package list

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/DarkInno/crdt"
)

type listVectorFile struct {
	Protocol         string `json:"protocol"`
	SemanticsVersion uint64 `json:"semantics_version"`
	FrameTypes       struct {
		State uint64 `json:"state"`
		Delta uint64 `json:"delta"`
	} `json:"frame_types"`
	Vectors []struct {
		Name          string   `json:"name"`
		FrameType     uint64   `json:"frame_type"`
		CompleteState bool     `json:"complete_state"`
		CodecID       string   `json:"codec_id"`
		Hex           string   `json:"hex"`
		Values        []string `json:"values"`
	} `json:"vectors"`
}

type vectorStringCodec struct{}

func (vectorStringCodec) ID() string                             { return "example.com/list-string/v1" }
func (vectorStringCodec) Marshal(value string) ([]byte, error)   { return []byte(value), nil }
func (vectorStringCodec) Unmarshal(value []byte) (string, error) { return string(value), nil }

func TestListRGAV1CrossLanguageVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "docs", "protocol", "testdata", "list-rga-v1-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors listVectorFile
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if vectors.Protocol != "list-rga-v1" || vectors.SemanticsVersion != SemanticsVersion || vectors.FrameTypes.State != crdt.TypeIDListRGAState || vectors.FrameTypes.Delta != crdt.TypeIDListRGADelta {
		t.Fatalf("invalid list vector metadata: %#v", vectors)
	}
	for _, vector := range vectors.Vectors {
		encoded, err := hex.DecodeString(vector.Hex)
		if err != nil {
			t.Fatalf("%s hex: %v", vector.Name, err)
		}
		target, err := New("target", vectorStringCodec{})
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
			delta, decodeErr := UnmarshalDelta(encoded, vectorStringCodec{})
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
		got, err := target.Values()
		if err != nil || !reflect.DeepEqual(got, vector.Values) {
			t.Fatalf("%s values = %#v, %v; want %#v", vector.Name, got, err, vector.Values)
		}
	}
}
