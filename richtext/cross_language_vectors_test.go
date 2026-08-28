package richtext

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/darkinno-tech/crdt"
)

type richTextVectorFile struct {
	Protocol         string `json:"protocol"`
	SemanticsVersion uint64 `json:"semantics_version"`
	FrameTypes       struct {
		State uint64 `json:"state"`
		Delta uint64 `json:"delta"`
	} `json:"frame_types"`
	Vectors []richTextVector `json:"vectors"`
}

type richTextVector struct {
	Name          string         `json:"name"`
	FrameType     uint64         `json:"frame_type"`
	CompleteState bool           `json:"complete_state"`
	Hex           string         `json:"hex"`
	Text          string         `json:"text"`
	Spans         []richTextSpan `json:"spans"`
}

type richTextSpan struct {
	Text       string            `json:"text"`
	Attributes map[string]string `json:"attributes"`
}

func TestRichTextV1CrossLanguageVectors(t *testing.T) {
	vectors := loadRichTextV1Vectors(t)
	if vectors.Protocol != "richtext-v1" || vectors.SemanticsVersion != SemanticsVersion ||
		vectors.FrameTypes.State != crdt.TypeIDRichTextState || vectors.FrameTypes.Delta != crdt.TypeIDRichTextDelta {
		t.Fatalf("invalid rich-text vector metadata: %#v", vectors)
	}
	for _, vector := range vectors.Vectors {
		t.Run(vector.Name, func(t *testing.T) {
			encoded, err := hex.DecodeString(vector.Hex)
			if err != nil {
				t.Fatal(err)
			}
			document := mustDocument(t, "vector-receiver")
			if vector.CompleteState {
				if vector.FrameType != crdt.TypeIDRichTextState {
					t.Fatalf("state vector TypeID = %d", vector.FrameType)
				}
				if err := document.UnmarshalBinary(encoded); err != nil {
					t.Fatal(err)
				}
				reencoded, err := document.MarshalBinary()
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(reencoded, encoded) {
					t.Fatalf("re-encoded state differs\n got: %x\nwant: %x", reencoded, encoded)
				}
			} else {
				if vector.FrameType != crdt.TypeIDRichTextDelta {
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
				if err := document.ApplyDelta(delta); err != nil {
					t.Fatal(err)
				}
			}
			if got := document.String(); got != vector.Text {
				t.Fatalf("text = %q, want %q", got, vector.Text)
			}
			if got, want := vectorSpans(document.Spans()), vector.Spans; !reflect.DeepEqual(got, want) {
				t.Fatalf("spans = %#v, want %#v", got, want)
			}
		})
	}
}

func loadRichTextV1Vectors(t *testing.T) richTextVectorFile {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find vector test path")
	}
	encoded, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "docs", "protocol", "testdata", "richtext-v1-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors richTextVectorFile
	if err := json.Unmarshal(encoded, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Vectors) == 0 {
		t.Fatal("no rich-text vectors")
	}
	return vectors
}

func vectorSpans(spans []Span) []richTextSpan {
	result := make([]richTextSpan, len(spans))
	for index, span := range spans {
		result[index] = richTextSpan{Text: span.Text, Attributes: span.Attributes}
	}
	return result
}
