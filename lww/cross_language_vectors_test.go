package lww

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/darkinno-tech/crdt"
)

type lwwVectorFile struct {
	Protocol         string `json:"protocol"`
	SemanticsVersion uint64 `json:"semantics_version"`
	FrameTypes       struct {
		SetState uint64 `json:"set_state"`
		SetDelta uint64 `json:"set_delta"`
		MapState uint64 `json:"map_state"`
		MapDelta uint64 `json:"map_delta"`
	} `json:"frame_types"`
	Vectors []lwwVector `json:"vectors"`
}

type lwwVector struct {
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`
	FrameType     uint64            `json:"frame_type"`
	CompleteState bool              `json:"complete_state"`
	CodecID       string            `json:"codec_id"`
	Hex           string            `json:"hex"`
	Values        map[string]string `json:"values"`
	Elements      []string          `json:"elements"`
}

func TestLWWV1CrossLanguageVectors(t *testing.T) {
	vectors := loadLWWV1Vectors(t)
	if vectors.Protocol != "lww-v1" || vectors.SemanticsVersion != SemanticsVersion ||
		vectors.FrameTypes.SetState != crdt.TypeIDLWWSetState || vectors.FrameTypes.SetDelta != crdt.TypeIDLWWSetDelta ||
		vectors.FrameTypes.MapState != crdt.TypeIDLWWMapState || vectors.FrameTypes.MapDelta != crdt.TypeIDLWWMapDelta {
		t.Fatalf("invalid LWW vector metadata: %#v", vectors)
	}
	for _, vector := range vectors.Vectors {
		encoded, err := hex.DecodeString(vector.Hex)
		if err != nil {
			t.Fatalf("%s hex: %v", vector.Name, err)
		}
		switch vector.Kind {
		case "map":
			target, err := NewMap("target")
			if err != nil {
				t.Fatal(err)
			}
			var reencoded []byte
			if vector.CompleteState {
				if err := target.UnmarshalBinary(encoded); err != nil {
					t.Fatalf("%s decode state: %v", vector.Name, err)
				}
				reencoded, err = target.MarshalBinary()
			} else {
				delta, decodeErr := UnmarshalMapDelta(encoded)
				if decodeErr != nil {
					t.Fatalf("%s decode delta: %v", vector.Name, decodeErr)
				}
				reencoded, err = delta.MarshalBinary()
				if err == nil {
					err = target.ApplyDelta(delta)
				}
			}
			if err != nil || !bytes.Equal(reencoded, encoded) {
				t.Fatalf("%s map re-encoding = %x, %v; want %x", vector.Name, reencoded, err, encoded)
			}
			for key, want := range vector.Values {
				got, ok := target.Get(key)
				if !ok || string(got) != want {
					t.Fatalf("%s map value %q = %q, %v; want %q", vector.Name, key, got, ok, want)
				}
			}
		case "set":
			codec := setStringCodec{id: vector.CodecID}
			target, err := NewSet[string]("target")
			if err != nil {
				t.Fatal(err)
			}
			var reencoded []byte
			if vector.CompleteState {
				if err := target.UnmarshalBinary(encoded, codec); err != nil {
					t.Fatalf("%s decode state: %v", vector.Name, err)
				}
				reencoded, err = target.MarshalBinary(codec)
			} else {
				delta, decodeErr := UnmarshalSetDelta(encoded, codec)
				if decodeErr != nil {
					t.Fatalf("%s decode delta: %v", vector.Name, decodeErr)
				}
				reencoded, err = delta.MarshalBinary(codec)
				if err == nil {
					err = target.ApplyDelta(delta)
				}
			}
			if err != nil || !bytes.Equal(reencoded, encoded) {
				t.Fatalf("%s set re-encoding = %x, %v; want %x", vector.Name, reencoded, err, encoded)
			}
			if got := target.Elements(); !reflect.DeepEqual(got, vector.Elements) {
				t.Fatalf("%s elements = %#v, want %#v", vector.Name, got, vector.Elements)
			}
		default:
			t.Fatalf("%s has unknown kind %q", vector.Name, vector.Kind)
		}
	}
}

func loadLWWV1Vectors(t *testing.T) lwwVectorFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "docs", "protocol", "testdata", "lww-v1-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors lwwVectorFile
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	return vectors
}
