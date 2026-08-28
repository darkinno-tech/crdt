package documenttree

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/im10furry/crdt"
)

// MarshalJSON returns a payload-free diagnostic summary.
func (d *Document) MarshalJSON() ([]byte, error) { return crdt.MarshalStateJSON(d) }

// MarshalJSON returns a payload-free diagnostic summary for a delta.
func (d Delta) MarshalJSON() ([]byte, error) {
	return crdt.MarshalDiagnosticJSON(crdt.StateSnapshot{
		Type:           "document-tree-delta",
		ElementCount:   len(d.state.objects),
		TombstoneCount: countTombstones(d.state),
	})
}

// marshalJSONScalar canonicalizes one JSON scalar before it reaches a map or
// array byte value. A nested object or array must be represented by an owned
// document-tree child so concurrent field and position edits remain mergeable.
func marshalJSONScalar(value any, options Options) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, ErrInvalidJSON
	}
	_, canonical, err := decodeJSONScalar(encoded, options)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

func unmarshalJSONScalar(encoded []byte, options Options) (any, error) {
	decoded, canonical, err := decodeJSONScalar(encoded, options)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(encoded, canonical) {
		return nil, ErrInvalidJSON
	}
	return decoded, nil
}

func decodeJSONScalar(encoded []byte, options Options) (any, []byte, error) {
	if !validJSONShape(encoded, options.MaxValueBytes, options.MaxDepth) {
		return nil, nil, ErrInvalidJSON
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, nil, ErrInvalidJSON
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, nil, ErrInvalidJSON
	}
	switch decoded.(type) {
	case nil, bool, string, json.Number:
	default:
		return nil, nil, ErrInvalidJSON
	}
	canonical, err := json.Marshal(decoded)
	if err != nil || len(canonical) > options.MaxValueBytes {
		return nil, nil, ErrInvalidJSON
	}
	return decoded, canonical, nil
}

// validJSONShape rejects over-budget nesting before encoding/json constructs a
// composite value. The decoder remains the syntax authority; this small scan
// only recognizes strings well enough to count structural delimiters safely.
func validJSONShape(encoded []byte, maxBytes, maxDepth int) bool {
	if len(encoded) == 0 || len(encoded) > maxBytes || maxDepth <= 0 {
		return false
	}
	depth := 0
	inString := false
	escaped := false
	for _, byteValue := range encoded {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch byteValue {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch byteValue {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maxDepth {
				return false
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return !inString && !escaped && depth == 0
}
