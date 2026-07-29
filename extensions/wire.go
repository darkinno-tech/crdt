package extensions

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/replica"
)

const (
	// Subprotocol identifies this package's control and binary change envelope.
	// It is independent from CRDT frame-format versions.
	Subprotocol      = "crdt-sync-v1"
	transportVersion = 1
	maxControlBytes  = 16 << 10
)

var errInvalidWireMessage = errors.New("crdt extensions: invalid wire message")

func controlLimit(maxMessageBytes int) int {
	if maxMessageBytes < maxControlBytes {
		return maxMessageBytes
	}
	return maxControlBytes
}

type helloMessage struct {
	Version  uint8            `json:"version"`
	Manifest replica.Manifest `json:"manifest"`
}

func marshalHello(manifest replica.Manifest) ([]byte, error) {
	return json.Marshal(helloMessage{Version: transportVersion, Manifest: manifest})
}

func unmarshalHello(data []byte) (replica.Manifest, error) {
	if len(data) == 0 || len(data) > maxControlBytes {
		return replica.Manifest{}, errInvalidWireMessage
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var message helloMessage
	if err := decoder.Decode(&message); err != nil || message.Version != transportVersion {
		return replica.Manifest{}, errInvalidWireMessage
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return replica.Manifest{}, errInvalidWireMessage
	}
	return message.Manifest, nil
}

func marshalChange(change replica.Change) ([]byte, error) {
	if strings.TrimSpace(change.Dot.Actor) == "" || change.Dot.Counter == 0 {
		return nil, errInvalidWireMessage
	}
	delta := change.Delta()
	if len(delta) == 0 {
		return nil, errInvalidWireMessage
	}
	encoded := make([]byte, 0, 1+len(change.Dot.Actor)+len(delta)+32)
	encoded = append(encoded, transportVersion)
	encoded = frame.AppendUvarint(encoded, uint64(len(change.Dot.Actor)))
	encoded = append(encoded, change.Dot.Actor...)
	encoded = frame.AppendUvarint(encoded, change.Dot.Counter)
	encoded = frame.AppendUvarint(encoded, uint64(len(delta)))
	return append(encoded, delta...), nil
}

func unmarshalChange(data []byte, maxMessageBytes, maxActorBytes int) (replica.Dot, []byte, error) {
	if len(data) == 0 || len(data) > maxMessageBytes || maxActorBytes <= 0 || data[0] != transportVersion {
		return replica.Dot{}, nil, errInvalidWireMessage
	}
	actor, position, ok := frame.ReadBytes(data, 1, maxActorBytes)
	if !ok {
		return replica.Dot{}, nil, errInvalidWireMessage
	}
	counter, position, ok := frame.ReadUvarint(data, position)
	if !ok || counter == 0 {
		return replica.Dot{}, nil, errInvalidWireMessage
	}
	delta, position, ok := frame.ReadBytes(data, position, maxMessageBytes)
	if !ok || position != len(data) || len(delta) == 0 {
		return replica.Dot{}, nil, errInvalidWireMessage
	}
	dot := replica.Dot{Actor: string(actor), Counter: counter}
	if strings.TrimSpace(dot.Actor) == "" {
		return replica.Dot{}, nil, errInvalidWireMessage
	}
	return dot, append([]byte(nil), delta...), nil
}
