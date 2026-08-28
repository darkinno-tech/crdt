package tree

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/im10furry/crdt"
)

type orTreeVectorFile struct {
	Protocol         string `json:"protocol"`
	SemanticsVersion uint64 `json:"semantics_version"`
	FrameTypes       struct {
		State uint64 `json:"state"`
		Delta uint64 `json:"delta"`
	} `json:"frame_types"`
	Vectors []orTreeVector `json:"vectors"`
}

type orTreeVector struct {
	Name           string   `json:"name"`
	FrameType      uint64   `json:"frame_type"`
	CompleteState  bool     `json:"complete_state"`
	Hex            string   `json:"hex"`
	VisibleValues  []string `json:"visible_values"`
	TombstoneCount int      `json:"tombstone_count"`
}

func TestORTreeV1CrossLanguageVectors(t *testing.T) {
	vectors := loadORTreeV1Vectors(t)
	if vectors.Protocol != "or-tree-v1" || vectors.SemanticsVersion != SemanticsVersion ||
		vectors.FrameTypes.State != crdt.TypeIDORTreeState || vectors.FrameTypes.Delta != crdt.TypeIDORTreeDelta {
		t.Fatalf("invalid OR-Tree vector metadata: %#v", vectors)
	}
	for _, vector := range vectors.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			encoded, err := hex.DecodeString(vector.Hex)
			if err != nil {
				t.Fatal(err)
			}
			value, err := New("vector-receiver")
			if err != nil {
				t.Fatal(err)
			}
			if vector.CompleteState {
				if vector.FrameType != crdt.TypeIDORTreeState {
					t.Fatalf("state vector TypeID = %d", vector.FrameType)
				}
				if err := value.UnmarshalBinary(encoded); err != nil {
					t.Fatal(err)
				}
				reencoded, err := value.MarshalBinary()
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(reencoded, encoded) {
					t.Fatalf("re-encoded state differs\n got: %x\nwant: %x", reencoded, encoded)
				}
			} else {
				if vector.FrameType != crdt.TypeIDORTreeDelta {
					t.Fatalf("delta vector TypeID = %d", vector.FrameType)
				}
				delta, err := UnmarshalDelta(encoded)
				if err != nil {
					t.Fatal(err)
				}
				reencoded, err := delta.MarshalBinary()
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(reencoded, encoded) {
					t.Fatalf("re-encoded delta differs\n got: %x\nwant: %x", reencoded, encoded)
				}
				if err := value.ApplyDelta(delta); err != nil {
					t.Fatal(err)
				}
			}
			values := make([]string, 0, len(value.Nodes()))
			for _, node := range value.Nodes() {
				values = append(values, string(node.Value))
			}
			if !reflect.DeepEqual(values, vector.VisibleValues) {
				t.Fatalf("visible values = %#v, want %#v", values, vector.VisibleValues)
			}
			if state := value.State(); state.TombstoneCount != vector.TombstoneCount {
				t.Fatalf("tombstone count = %d, want %d", state.TombstoneCount, vector.TombstoneCount)
			}
		})
	}
}

func loadORTreeV1Vectors(t *testing.T) orTreeVectorFile {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find vector test path")
	}
	encoded, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "docs", "protocol", "testdata", "or-tree-v1-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors orTreeVectorFile
	if err := json.Unmarshal(encoded, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Vectors) == 0 {
		t.Fatal("no OR-Tree vectors")
	}
	return vectors
}
