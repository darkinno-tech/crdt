package durable

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/replica"
)

const (
	protocolVersion = 1
	maxControlBytes = 16 << 10

	changeMessage = 1
	eventMessage  = 2
)

var errInvalidWire = errors.New("crdt durable: invalid wire message")

type helloMessage struct {
	Version  uint8            `json:"version"`
	Manifest replica.Manifest `json:"manifest"`
	Resume   uint64           `json:"resume"`
}

type welcomeMessage struct {
	Version   uint8            `json:"version"`
	Manifest  replica.Manifest `json:"manifest"`
	HighWater uint64           `json:"high_water"`
}

type errorMessage struct {
	Version uint8  `json:"version"`
	Code    string `json:"code"`
}

func controlLimit(maxMessageBytes int) int {
	if maxMessageBytes < maxControlBytes {
		return maxMessageBytes
	}
	return maxControlBytes
}

func marshalHello(manifest replica.Manifest, resume uint64) ([]byte, error) {
	return json.Marshal(helloMessage{Version: protocolVersion, Manifest: manifest, Resume: resume})
}

func unmarshalHello(data []byte) (replica.Manifest, uint64, error) {
	if len(data) == 0 || len(data) > maxControlBytes {
		return replica.Manifest{}, 0, errInvalidWire
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var message helloMessage
	if err := decoder.Decode(&message); err != nil || message.Version != protocolVersion || message.Manifest.Compatible(message.Manifest) != nil {
		return replica.Manifest{}, 0, errInvalidWire
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return replica.Manifest{}, 0, errInvalidWire
	}
	return message.Manifest, message.Resume, nil
}

func marshalWelcome(manifest replica.Manifest, highWater uint64) ([]byte, error) {
	return json.Marshal(welcomeMessage{Version: protocolVersion, Manifest: manifest, HighWater: highWater})
}

func unmarshalWelcome(data []byte) (replica.Manifest, uint64, error) {
	if len(data) == 0 || len(data) > maxControlBytes {
		return replica.Manifest{}, 0, errInvalidWire
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var message welcomeMessage
	if err := decoder.Decode(&message); err != nil || message.Version != protocolVersion || message.Manifest.Compatible(message.Manifest) != nil {
		return replica.Manifest{}, 0, errInvalidWire
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return replica.Manifest{}, 0, errInvalidWire
	}
	return message.Manifest, message.HighWater, nil
}

func marshalError(code string) ([]byte, error) {
	if code != "replay_unavailable" {
		return nil, errInvalidWire
	}
	return json.Marshal(errorMessage{Version: protocolVersion, Code: code})
}

func unmarshalError(data []byte) error {
	if len(data) == 0 || len(data) > maxControlBytes {
		return errInvalidWire
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var message errorMessage
	if err := decoder.Decode(&message); err != nil || message.Version != protocolVersion {
		return errInvalidWire
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errInvalidWire
	}
	if message.Code == "replay_unavailable" {
		return ErrReplayUnavailable
	}
	return errInvalidWire
}

func marshalChange(change replica.Change) ([]byte, error) {
	if !utf8.ValidString(change.Dot.Actor) || strings.TrimSpace(change.Dot.Actor) == "" || change.Dot.Counter == 0 {
		return nil, errInvalidWire
	}
	delta := change.Delta()
	if len(delta) == 0 {
		return nil, errInvalidWire
	}
	encoded := make([]byte, 0, 1+len(change.Dot.Actor)+len(delta)+32)
	encoded = append(encoded, changeMessage)
	encoded = frame.AppendUvarint(encoded, uint64(len(change.Dot.Actor)))
	encoded = append(encoded, change.Dot.Actor...)
	encoded = frame.AppendUvarint(encoded, change.Dot.Counter)
	encoded = frame.AppendUvarint(encoded, uint64(len(delta)))
	return append(encoded, delta...), nil
}

// EncodeChange produces the canonical durable-log envelope for one validated
// CRDT change. The envelope is not authentication; callers still bind it to
// an authenticated manifest and actor at their transport boundary.
func EncodeChange(change replica.Change) ([]byte, error) {
	return marshalChange(change)
}

func unmarshalChange(data []byte, maxMessageBytes, maxActorBytes int) (replica.Dot, []byte, error) {
	if len(data) == 0 || len(data) > maxMessageBytes || maxActorBytes <= 0 || data[0] != changeMessage {
		return replica.Dot{}, nil, errInvalidWire
	}
	return unmarshalChangeFields(data, 1, maxMessageBytes, maxActorBytes)
}

// DecodeChange decodes a bounded durable-log envelope. Storage providers must
// construct a replica.Change with the expected manifest and policy before
// returning this data to a relay.
func DecodeChange(data []byte, maxMessageBytes, maxActorBytes int) (replica.Dot, []byte, error) {
	return unmarshalChange(data, maxMessageBytes, maxActorBytes)
}

func marshalEvent(event Event) ([]byte, error) {
	if event.Sequence == 0 || !utf8.ValidString(event.Change.Dot.Actor) || strings.TrimSpace(event.Change.Dot.Actor) == "" || event.Change.Dot.Counter == 0 {
		return nil, errInvalidWire
	}
	delta := event.Change.Delta()
	if len(delta) == 0 {
		return nil, errInvalidWire
	}
	encoded := make([]byte, 0, 1+len(event.Change.Dot.Actor)+len(delta)+40)
	encoded = append(encoded, eventMessage)
	encoded = frame.AppendUvarint(encoded, event.Sequence)
	encoded = frame.AppendUvarint(encoded, uint64(len(event.Change.Dot.Actor)))
	encoded = append(encoded, event.Change.Dot.Actor...)
	encoded = frame.AppendUvarint(encoded, event.Change.Dot.Counter)
	encoded = frame.AppendUvarint(encoded, uint64(len(delta)))
	return append(encoded, delta...), nil
}

func unmarshalEvent(data []byte, maxMessageBytes, maxActorBytes int) (uint64, replica.Dot, []byte, error) {
	if len(data) == 0 || len(data) > maxMessageBytes || maxActorBytes <= 0 || data[0] != eventMessage {
		return 0, replica.Dot{}, nil, errInvalidWire
	}
	sequence, position, ok := frame.ReadUvarint(data, 1)
	if !ok || sequence == 0 {
		return 0, replica.Dot{}, nil, errInvalidWire
	}
	dot, delta, err := unmarshalChangeFields(data, position, maxMessageBytes, maxActorBytes)
	if err != nil {
		return 0, replica.Dot{}, nil, err
	}
	return sequence, dot, delta, nil
}

func newEventFromWire(manifest replica.Manifest, policy crdt.ProtocolPolicy, sequence uint64, dot replica.Dot, delta []byte) (Event, error) {
	change, err := replica.NewChangeWithPolicy(manifest, dot, delta, policy)
	if err != nil {
		return Event{}, err
	}
	return Event{Sequence: sequence, Change: change}, nil
}

func unmarshalChangeFields(data []byte, position, maxMessageBytes, maxActorBytes int) (replica.Dot, []byte, error) {
	actor, position, ok := frame.ReadBytes(data, position, maxActorBytes)
	if !ok {
		return replica.Dot{}, nil, errInvalidWire
	}
	counter, position, ok := frame.ReadUvarint(data, position)
	if !ok || counter == 0 {
		return replica.Dot{}, nil, errInvalidWire
	}
	delta, position, ok := frame.ReadBytes(data, position, maxMessageBytes)
	if !ok || position != len(data) || len(delta) == 0 {
		return replica.Dot{}, nil, errInvalidWire
	}
	dot := replica.Dot{Actor: string(actor), Counter: counter}
	if !utf8.ValidString(dot.Actor) || strings.TrimSpace(dot.Actor) == "" {
		return replica.Dot{}, nil, errInvalidWire
	}
	return dot, append([]byte(nil), delta...), nil
}
