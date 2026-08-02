package text

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"unicode/utf8"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
)

type rgaWireVectorFile struct {
	Protocol         string `json:"protocol"`
	SemanticsVersion uint64 `json:"semantics_version"`
	FrameTypes       struct {
		State uint64 `json:"state"`
		Delta uint64 `json:"delta"`
	} `json:"frame_types"`
	Vectors []rgaWireVector `json:"vectors"`
}

type rgaWireVector struct {
	Name                     string        `json:"name"`
	FrameType                uint64        `json:"frame_type"`
	CompleteState            bool          `json:"complete_state"`
	Hex                      string        `json:"hex"`
	Nodes                    []rgaWireNode `json:"nodes"`
	Tombstones               []rgaWireTag  `json:"tombstones"`
	VisibleTextAfterEmptyUse string        `json:"visible_text_after_apply_to_empty"`
}

type rgaWireNode struct {
	ID     rgaWireTag  `json:"id"`
	Parent *rgaWireTag `json:"parent"`
	Rune   string      `json:"rune"`
}

type rgaWireTag struct {
	ReplicaID string `json:"replica_id"`
	WallTime  string `json:"wall_time"`
	Logical   string `json:"logical"`
}

func TestRGARunV2CrossLanguageVectors(t *testing.T) {
	vectors := loadRGARunV2Vectors(t)
	if vectors.Protocol != "rga-run-v2" || vectors.SemanticsVersion != 2 ||
		vectors.FrameTypes.State != crdt.TypeIDRGARunState || vectors.FrameTypes.Delta != crdt.TypeIDRGARunDelta {
		t.Fatalf("invalid run-v2 vector metadata: %#v", vectors)
	}
	for _, vector := range vectors.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			nodes, tombstones := vector.delta(t)
			want, err := hex.DecodeString(vector.Hex)
			if err != nil {
				t.Fatal(err)
			}
			got, err := marshalRGARun(vector.FrameType, nodes, tombstones, frame.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("canonical vector differs\n got: %x\nwant: %x", got, want)
			}

			decodedNodes, decodedTombstones, err := unmarshalRGARun(want, vector.FrameType, frame.DefaultLimits(), vector.CompleteState, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !sameRGADelta(nodes, tombstones, decodedNodes, decodedTombstones) {
				t.Fatalf("decoded vector differs: nodes=%#v tombstones=%#v", decodedNodes, decodedTombstones)
			}

			document, err := New("vector-receiver")
			if err != nil {
				t.Fatal(err)
			}
			if vector.CompleteState {
				err = document.UnmarshalRunBinary(want)
			} else {
				err = document.ApplyDelta(Delta{nodes: decodedNodes, tombstones: decodedTombstones})
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := document.String(); got != vector.VisibleTextAfterEmptyUse {
				t.Fatalf("visible text = %q, want %q", got, vector.VisibleTextAfterEmptyUse)
			}
		})
	}
}

func TestRGAPackedV3CrossLanguageVectors(t *testing.T) {
	vectors := loadRGAPackedV3Vectors(t)
	if vectors.Protocol != "rga-packed-v3" || vectors.SemanticsVersion != PackedV3SemanticsVersion ||
		vectors.FrameTypes.State != crdt.TypeIDRGAPackedState || vectors.FrameTypes.Delta != crdt.TypeIDRGAPackedDelta {
		t.Fatalf("invalid packed-v3 vector metadata: %#v", vectors)
	}
	for _, vector := range vectors.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			nodes, tombstones := vector.delta(t)
			want, err := hex.DecodeString(vector.Hex)
			if err != nil {
				t.Fatal(err)
			}
			got, err := marshalRGAPacked(vector.FrameType, nodes, tombstones, frame.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("canonical packed vector differs\n got: %x\nwant: %x", got, want)
			}
			decodedNodes, decodedTombstones, err := unmarshalRGAPacked(want, vector.FrameType, frame.DefaultLimits(), vector.CompleteState, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !sameRGADelta(nodes, tombstones, decodedNodes, decodedTombstones) {
				t.Fatalf("decoded packed vector differs: nodes=%#v tombstones=%#v", decodedNodes, decodedTombstones)
			}
			document := mustRGA(t, "packed-vector-receiver")
			if err := document.UnmarshalPackedBinary(want); err != nil {
				t.Fatal(err)
			}
			if got := document.String(); got != vector.VisibleTextAfterEmptyUse {
				t.Fatalf("visible packed text = %q, want %q", got, vector.VisibleTextAfterEmptyUse)
			}
		})
	}
}

func loadRGARunV2Vectors(t *testing.T) rgaWireVectorFile {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find vector test path")
	}
	encoded, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "docs", "protocol", "testdata", "rga-run-v2-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors rgaWireVectorFile
	if err := json.Unmarshal(encoded, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Vectors) == 0 {
		t.Fatal("no run-v2 vectors")
	}
	return vectors
}

func loadRGAPackedV3Vectors(t *testing.T) rgaWireVectorFile {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find vector test path")
	}
	encoded, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "docs", "protocol", "testdata", "rga-packed-v3-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors rgaWireVectorFile
	if err := json.Unmarshal(encoded, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Vectors) == 0 {
		t.Fatal("no packed-v3 vectors")
	}
	return vectors
}

func (v rgaWireVector) delta(t *testing.T) (map[Position]node, map[Position]struct{}) {
	t.Helper()
	nodes := make(map[Position]node, len(v.Nodes))
	for _, encoded := range v.Nodes {
		id := encoded.ID.position(t)
		value := []rune(encoded.Rune)
		if len(value) != 1 || !utf8.ValidRune(value[0]) {
			t.Fatalf("invalid node rune %q", encoded.Rune)
		}
		item := node{rune: value[0]}
		if encoded.Parent != nil {
			item.parent = encoded.Parent.position(t)
		}
		if _, exists := nodes[id]; exists {
			t.Fatalf("duplicate vector node %v", id)
		}
		nodes[id] = item
	}
	tombstones := make(map[Position]struct{}, len(v.Tombstones))
	for _, encoded := range v.Tombstones {
		id := encoded.position(t)
		if _, exists := tombstones[id]; exists {
			t.Fatalf("duplicate vector tombstone %v", id)
		}
		tombstones[id] = struct{}{}
	}
	return nodes, tombstones
}

func (tag rgaWireTag) position(t *testing.T) Position {
	t.Helper()
	wallTime, err := strconv.ParseUint(tag.WallTime, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := strconv.ParseUint(tag.Logical, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	position := Position{ReplicaID: tag.ReplicaID, WallTime: wallTime, Logical: logical}
	if !position.Valid() {
		t.Fatalf("invalid vector tag %#v", tag)
	}
	return position
}

func sameRGADelta(wantNodes map[Position]node, wantTombstones map[Position]struct{}, gotNodes map[Position]node, gotTombstones map[Position]struct{}) bool {
	if len(wantNodes) != len(gotNodes) || len(wantTombstones) != len(gotTombstones) {
		return false
	}
	for id, want := range wantNodes {
		if got, ok := gotNodes[id]; !ok || got != want {
			return false
		}
	}
	for id := range wantTombstones {
		if _, ok := gotTombstones[id]; !ok {
			return false
		}
	}
	return true
}
