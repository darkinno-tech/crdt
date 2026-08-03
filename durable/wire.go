package durable

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/replica"
)

const (
	protocolVersion            = 1
	stateVectorProtocolVersion = 2
	merkleProtocolVersion      = 3
	maxControlBytes            = 16 << 10

	changeMessage      = 1
	eventMessage       = 2
	merkleEventMessage = 3
)

var errInvalidWire = errors.New("crdt durable: invalid wire message")

type helloMessage struct {
	Version  uint8            `json:"version"`
	Manifest replica.Manifest `json:"manifest"`
	Resume   uint64           `json:"resume"`
}

type stateVectorEntry struct {
	Actor   string `json:"actor"`
	Counter uint64 `json:"counter"`
}

type stateVectorHelloMessage struct {
	Version     uint8              `json:"version"`
	Manifest    replica.Manifest   `json:"manifest"`
	StateVector []stateVectorEntry `json:"state_vector"`
}

type welcomeMessage struct {
	Version   uint8            `json:"version"`
	Manifest  replica.Manifest `json:"manifest"`
	HighWater uint64           `json:"high_water"`
}

type stateVectorWelcomeMessage struct {
	Version   uint8            `json:"version"`
	Manifest  replica.Manifest `json:"manifest"`
	HighWater uint64           `json:"high_water"`
}

type catchUpCompleteMessage struct {
	Version   uint8  `json:"version"`
	HighWater uint64 `json:"high_water"`
}

type errorMessage struct {
	Version uint8  `json:"version"`
	Code    string `json:"code"`
}

type merkleTagMessage struct {
	ReplicaID string `json:"replica_id"`
	WallTime  uint64 `json:"wall_time"`
	Logical   uint64 `json:"logical"`
}

type merkleLeafMessage struct {
	HLC    merkleTagMessage `json:"hlc"`
	Digest string           `json:"digest"`
}

type merkleHelloMessage struct {
	Version  uint8            `json:"version"`
	Kind     string           `json:"kind"`
	Manifest replica.Manifest `json:"manifest"`
	Root     string           `json:"root"`
}

type merkleWelcomeMessage struct {
	Version   uint8             `json:"version"`
	Kind      string            `json:"kind"`
	Manifest  replica.Manifest  `json:"manifest"`
	Root      string            `json:"root"`
	HighWater uint64            `json:"high_water"`
	HLC       *merkleTagMessage `json:"hlc,omitempty"`
}

type merkleInventoryMessage struct {
	Version uint8               `json:"version"`
	Kind    string              `json:"kind"`
	Leaves  []merkleLeafMessage `json:"leaves"`
	Done    bool                `json:"done"`
}

type merkleRequestMessage struct {
	Version    uint8              `json:"version"`
	Kind       string             `json:"kind"`
	Identities []merkleTagMessage `json:"identities"`
	Done       bool               `json:"done"`
}

type merkleCompleteMessage struct {
	Version   uint8             `json:"version"`
	Kind      string            `json:"kind"`
	Root      string            `json:"root"`
	HighWater uint64            `json:"high_water"`
	HLC       *merkleTagMessage `json:"hlc,omitempty"`
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

func marshalStateVectorHello(manifest replica.Manifest, vector replica.Frontier, maxEntries, maxActorBytes int) ([]byte, error) {
	entries, err := stateVectorEntries(vector, maxEntries, maxActorBytes)
	if err != nil {
		return nil, errInvalidWire
	}
	return json.Marshal(stateVectorHelloMessage{
		Version:     stateVectorProtocolVersion,
		Manifest:    manifest,
		StateVector: entries,
	})
}

func unmarshalStateVectorHello(data []byte, maxEntries, maxActorBytes int) (replica.Manifest, replica.Frontier, error) {
	if len(data) == 0 || len(data) > maxControlBytes {
		return replica.Manifest{}, replica.Frontier{}, errInvalidWire
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var message stateVectorHelloMessage
	if err := decoder.Decode(&message); err != nil || message.Version != stateVectorProtocolVersion || message.Manifest.Compatible(message.Manifest) != nil {
		return replica.Manifest{}, replica.Frontier{}, errInvalidWire
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return replica.Manifest{}, replica.Frontier{}, errInvalidWire
	}
	vector, err := frontierFromStateVectorEntries(message.StateVector, maxEntries, maxActorBytes)
	if err != nil {
		return replica.Manifest{}, replica.Frontier{}, errInvalidWire
	}
	return message.Manifest, vector, nil
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

func marshalStateVectorWelcome(manifest replica.Manifest, highWater uint64) ([]byte, error) {
	return json.Marshal(stateVectorWelcomeMessage{Version: stateVectorProtocolVersion, Manifest: manifest, HighWater: highWater})
}

func unmarshalStateVectorWelcome(data []byte) (replica.Manifest, uint64, error) {
	if len(data) == 0 || len(data) > maxControlBytes {
		return replica.Manifest{}, 0, errInvalidWire
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var message stateVectorWelcomeMessage
	if err := decoder.Decode(&message); err != nil || message.Version != stateVectorProtocolVersion || message.Manifest.Compatible(message.Manifest) != nil {
		return replica.Manifest{}, 0, errInvalidWire
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return replica.Manifest{}, 0, errInvalidWire
	}
	return message.Manifest, message.HighWater, nil
}

func marshalCatchUpComplete(highWater uint64) ([]byte, error) {
	return json.Marshal(catchUpCompleteMessage{Version: stateVectorProtocolVersion, HighWater: highWater})
}

func unmarshalCatchUpComplete(data []byte) (uint64, error) {
	if len(data) == 0 || len(data) > maxControlBytes {
		return 0, errInvalidWire
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var message catchUpCompleteMessage
	if err := decoder.Decode(&message); err != nil || message.Version != stateVectorProtocolVersion {
		return 0, errInvalidWire
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return 0, errInvalidWire
	}
	return message.HighWater, nil
}

func marshalError(code string) ([]byte, error) {
	if code != "replay_unavailable" && code != "anti_entropy_unavailable" {
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
	if message.Code == "anti_entropy_unavailable" {
		return ErrAntiEntropyUnavailable
	}
	return errInvalidWire
}

func marshalMerkleHello(manifest replica.Manifest, root [sha256.Size]byte) ([]byte, error) {
	return json.Marshal(merkleHelloMessage{
		Version:  merkleProtocolVersion,
		Kind:     "hello",
		Manifest: manifest,
		Root:     encodeMerkleDigest(root),
	})
}

func unmarshalMerkleHello(data []byte) (replica.Manifest, [sha256.Size]byte, error) {
	var message merkleHelloMessage
	if err := decodeMerkleControl(data, &message); err != nil || message.Version != merkleProtocolVersion || message.Kind != "hello" || message.Manifest.Compatible(message.Manifest) != nil {
		return replica.Manifest{}, [sha256.Size]byte{}, errInvalidWire
	}
	root, err := decodeMerkleDigest(message.Root)
	if err != nil {
		return replica.Manifest{}, [sha256.Size]byte{}, errInvalidWire
	}
	return message.Manifest, root, nil
}

func marshalMerkleWelcome(manifest replica.Manifest, boundary MerkleBoundary) ([]byte, error) {
	message := merkleWelcomeMessage{
		Version:   merkleProtocolVersion,
		Kind:      "welcome",
		Manifest:  manifest,
		Root:      encodeMerkleDigest(boundary.Root),
		HighWater: boundary.HighWater,
	}
	if boundary.HLC.Valid() {
		tag := marshalMerkleTag(boundary.HLC)
		message.HLC = &tag
	}
	return json.Marshal(message)
}

func unmarshalMerkleWelcome(data []byte, maxReplicaBytes int) (replica.Manifest, MerkleBoundary, error) {
	var message merkleWelcomeMessage
	if err := decodeMerkleControl(data, &message); err != nil || message.Version != merkleProtocolVersion || message.Kind != "welcome" || message.Manifest.Compatible(message.Manifest) != nil {
		return replica.Manifest{}, MerkleBoundary{}, errInvalidWire
	}
	root, err := decodeMerkleDigest(message.Root)
	if err != nil {
		return replica.Manifest{}, MerkleBoundary{}, errInvalidWire
	}
	boundary := MerkleBoundary{Root: root, HighWater: message.HighWater}
	if message.HLC != nil {
		boundary.HLC, err = unmarshalMerkleTag(*message.HLC, maxReplicaBytes)
		if err != nil {
			return replica.Manifest{}, MerkleBoundary{}, errInvalidWire
		}
	} else if message.HighWater != 0 {
		return replica.Manifest{}, MerkleBoundary{}, errInvalidWire
	}
	return message.Manifest, boundary, nil
}

func marshalMerkleInventoryChunks(leaves []MerkleLeaf, maxControl int) ([][]byte, error) {
	if maxControl <= 0 {
		return nil, errInvalidWire
	}
	if len(leaves) == 0 {
		encoded, err := json.Marshal(merkleInventoryMessage{Version: merkleProtocolVersion, Kind: "inventory", Leaves: []merkleLeafMessage{}, Done: true})
		if err != nil || len(encoded) > maxControl {
			return nil, errInvalidWire
		}
		return [][]byte{encoded}, nil
	}
	if validateMerkleLeaves(leaves, uint64(len(leaves)), ^uint64(0), frame.DefaultLimits().MaxStringBytes) != nil {
		return nil, errInvalidWire
	}
	chunks := make([][]byte, 0, 1)
	current := make([]merkleLeafMessage, 0, len(leaves))
	flush := func(done bool) error {
		encoded, err := json.Marshal(merkleInventoryMessage{Version: merkleProtocolVersion, Kind: "inventory", Leaves: current, Done: done})
		if err != nil || len(encoded) > maxControl {
			return errInvalidWire
		}
		chunks = append(chunks, encoded)
		current = make([]merkleLeafMessage, 0, len(leaves))
		return nil
	}
	for _, leaf := range leaves {
		encodedLeaf := marshalMerkleLeaf(leaf)
		current = append(current, encodedLeaf)
		encoded, err := json.Marshal(merkleInventoryMessage{Version: merkleProtocolVersion, Kind: "inventory", Leaves: current})
		if err != nil || len(encoded) > maxControl {
			current = current[:len(current)-1]
			if len(current) == 0 || flush(false) != nil {
				return nil, errInvalidWire
			}
			current = append(current, encodedLeaf)
			encoded, err = json.Marshal(merkleInventoryMessage{Version: merkleProtocolVersion, Kind: "inventory", Leaves: current})
			if err != nil || len(encoded) > maxControl {
				return nil, errInvalidWire
			}
		}
	}
	if err := flush(true); err != nil {
		return nil, err
	}
	return chunks, nil
}

func unmarshalMerkleInventory(data []byte, maxReplicaBytes int) ([]MerkleLeaf, bool, error) {
	var message merkleInventoryMessage
	if err := decodeMerkleControl(data, &message); err != nil || message.Version != merkleProtocolVersion || message.Kind != "inventory" {
		return nil, false, errInvalidWire
	}
	leaves := make([]MerkleLeaf, 0, len(message.Leaves))
	for _, encoded := range message.Leaves {
		leaf, err := unmarshalMerkleLeaf(encoded, maxReplicaBytes)
		if err != nil {
			return nil, false, errInvalidWire
		}
		leaves = append(leaves, leaf)
	}
	return leaves, message.Done, nil
}

func marshalMerkleRequestChunks(identities []crdt.Tag, maxControl int) ([][]byte, error) {
	if maxControl <= 0 {
		return nil, errInvalidWire
	}
	if len(identities) == 0 {
		encoded, err := json.Marshal(merkleRequestMessage{Version: merkleProtocolVersion, Kind: "request", Identities: []merkleTagMessage{}, Done: true})
		if err != nil || len(encoded) > maxControl {
			return nil, errInvalidWire
		}
		return [][]byte{encoded}, nil
	}
	if validateMerkleIdentityRequest(identities, uint64(len(identities)), ^uint64(0), frame.DefaultLimits().MaxStringBytes) != nil {
		return nil, errInvalidWire
	}
	chunks := make([][]byte, 0, 1)
	current := make([]merkleTagMessage, 0, len(identities))
	flush := func(done bool) error {
		encoded, err := json.Marshal(merkleRequestMessage{Version: merkleProtocolVersion, Kind: "request", Identities: current, Done: done})
		if err != nil || len(encoded) > maxControl {
			return errInvalidWire
		}
		chunks = append(chunks, encoded)
		current = make([]merkleTagMessage, 0, len(identities))
		return nil
	}
	for _, identity := range identities {
		encodedIdentity := marshalMerkleTag(identity)
		current = append(current, encodedIdentity)
		encoded, err := json.Marshal(merkleRequestMessage{Version: merkleProtocolVersion, Kind: "request", Identities: current})
		if err != nil || len(encoded) > maxControl {
			current = current[:len(current)-1]
			if len(current) == 0 || flush(false) != nil {
				return nil, errInvalidWire
			}
			current = append(current, encodedIdentity)
			encoded, err = json.Marshal(merkleRequestMessage{Version: merkleProtocolVersion, Kind: "request", Identities: current})
			if err != nil || len(encoded) > maxControl {
				return nil, errInvalidWire
			}
		}
	}
	if err := flush(true); err != nil {
		return nil, err
	}
	return chunks, nil
}

func unmarshalMerkleRequest(data []byte, maxReplicaBytes int) ([]crdt.Tag, bool, error) {
	var message merkleRequestMessage
	if err := decodeMerkleControl(data, &message); err != nil || message.Version != merkleProtocolVersion || message.Kind != "request" {
		return nil, false, errInvalidWire
	}
	identities := make([]crdt.Tag, 0, len(message.Identities))
	for _, encoded := range message.Identities {
		identity, err := unmarshalMerkleTag(encoded, maxReplicaBytes)
		if err != nil {
			return nil, false, errInvalidWire
		}
		identities = append(identities, identity)
	}
	return identities, message.Done, nil
}

func marshalMerkleComplete(boundary MerkleBoundary) ([]byte, error) {
	message := merkleCompleteMessage{Version: merkleProtocolVersion, Kind: "complete", Root: encodeMerkleDigest(boundary.Root), HighWater: boundary.HighWater}
	if boundary.HLC.Valid() {
		tag := marshalMerkleTag(boundary.HLC)
		message.HLC = &tag
	}
	return json.Marshal(message)
}

func unmarshalMerkleComplete(data []byte, maxReplicaBytes int) (MerkleBoundary, error) {
	var message merkleCompleteMessage
	if err := decodeMerkleControl(data, &message); err != nil || message.Version != merkleProtocolVersion || message.Kind != "complete" {
		return MerkleBoundary{}, errInvalidWire
	}
	root, err := decodeMerkleDigest(message.Root)
	if err != nil {
		return MerkleBoundary{}, errInvalidWire
	}
	boundary := MerkleBoundary{Root: root, HighWater: message.HighWater}
	if message.HLC != nil {
		boundary.HLC, err = unmarshalMerkleTag(*message.HLC, maxReplicaBytes)
		if err != nil {
			return MerkleBoundary{}, errInvalidWire
		}
	} else if message.HighWater != 0 {
		return MerkleBoundary{}, errInvalidWire
	}
	return boundary, nil
}

func marshalMerkleEvent(event Event) ([]byte, error) {
	if event.Sequence == 0 || !validMerkleHLC(event.HLC, frame.DefaultLimits().MaxStringBytes) {
		return nil, errInvalidWire
	}
	change, err := marshalChange(event.Change)
	if err != nil {
		return nil, err
	}
	encoded := make([]byte, 0, 1+frame.UvarintSize(event.Sequence)+frame.TagSize(event.HLC)+len(change))
	encoded = append(encoded, merkleEventMessage)
	encoded = frame.AppendUvarint(encoded, event.Sequence)
	encoded = frame.AppendTag(encoded, event.HLC)
	return append(encoded, change...), nil
}

func unmarshalMerkleEvent(data []byte, maxMessageBytes, maxActorBytes int) (uint64, crdt.Tag, replica.Dot, []byte, error) {
	if len(data) == 0 || len(data) > maxMessageBytes || maxActorBytes <= 0 || data[0] != merkleEventMessage {
		return 0, crdt.Tag{}, replica.Dot{}, nil, errInvalidWire
	}
	sequence, position, ok := frame.ReadUvarint(data, 1)
	if !ok || sequence == 0 {
		return 0, crdt.Tag{}, replica.Dot{}, nil, errInvalidWire
	}
	tag, position, ok := frame.ReadTag(data, position, maxActorBytes)
	if !ok || !validMerkleHLC(tag, maxActorBytes) {
		return 0, crdt.Tag{}, replica.Dot{}, nil, errInvalidWire
	}
	if position >= len(data) || data[position] != changeMessage {
		return 0, crdt.Tag{}, replica.Dot{}, nil, errInvalidWire
	}
	dot, delta, err := unmarshalChangeFields(data, position+1, maxMessageBytes, maxActorBytes)
	if err != nil {
		return 0, crdt.Tag{}, replica.Dot{}, nil, err
	}
	return sequence, tag, dot, delta, nil
}

func marshalMerkleTag(tag crdt.Tag) merkleTagMessage {
	return merkleTagMessage{ReplicaID: tag.ReplicaID, WallTime: tag.WallTime, Logical: tag.Logical}
}

func unmarshalMerkleTag(message merkleTagMessage, maxReplicaBytes int) (crdt.Tag, error) {
	tag := crdt.Tag{ReplicaID: message.ReplicaID, WallTime: message.WallTime, Logical: message.Logical}
	if !validMerkleHLC(tag, maxReplicaBytes) {
		return crdt.Tag{}, errInvalidWire
	}
	return tag, nil
}

func marshalMerkleLeaf(leaf MerkleLeaf) merkleLeafMessage {
	return merkleLeafMessage{HLC: marshalMerkleTag(leaf.HLC), Digest: encodeMerkleDigest(leaf.Digest)}
}

func unmarshalMerkleLeaf(message merkleLeafMessage, maxReplicaBytes int) (MerkleLeaf, error) {
	tag, err := unmarshalMerkleTag(message.HLC, maxReplicaBytes)
	if err != nil {
		return MerkleLeaf{}, errInvalidWire
	}
	digest, err := decodeMerkleDigest(message.Digest)
	if err != nil {
		return MerkleLeaf{}, errInvalidWire
	}
	return MerkleLeaf{HLC: tag, Digest: digest}, nil
}

func encodeMerkleDigest(digest [sha256.Size]byte) string {
	return base64.RawStdEncoding.EncodeToString(digest[:])
}

func decodeMerkleDigest(encoded string) ([sha256.Size]byte, error) {
	data, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(data) != sha256.Size || base64.RawStdEncoding.EncodeToString(data) != encoded {
		return [sha256.Size]byte{}, errInvalidWire
	}
	var digest [sha256.Size]byte
	copy(digest[:], data)
	return digest, nil
}

func decodeMerkleControl(data []byte, destination any) error {
	if len(data) == 0 || len(data) > maxControlBytes {
		return errInvalidWire
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errInvalidWire
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errInvalidWire
	}
	return nil
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

func stateVectorEntries(vector replica.Frontier, maxEntries, maxActorBytes int) ([]stateVectorEntry, error) {
	if maxEntries <= 0 || maxActorBytes <= 0 {
		return nil, errInvalidWire
	}
	entries := vector.Entries()
	if len(entries) > maxEntries {
		return nil, errInvalidWire
	}
	actors := make([]string, 0, len(entries))
	for actor := range entries {
		actors = append(actors, actor)
	}
	sort.Strings(actors)
	encoded := make([]stateVectorEntry, 0, len(actors))
	for _, actor := range actors {
		counter := entries[actor]
		if !utf8.ValidString(actor) || strings.TrimSpace(actor) == "" || len(actor) > maxActorBytes || counter == 0 {
			return nil, errInvalidWire
		}
		encoded = append(encoded, stateVectorEntry{Actor: actor, Counter: counter})
	}
	return encoded, nil
}

func frontierFromStateVectorEntries(entries []stateVectorEntry, maxEntries, maxActorBytes int) (replica.Frontier, error) {
	if maxEntries <= 0 || maxActorBytes <= 0 || len(entries) > maxEntries {
		return replica.Frontier{}, errInvalidWire
	}
	frontier := make(map[string]uint64, len(entries))
	previous := ""
	for index, entry := range entries {
		if !utf8.ValidString(entry.Actor) || strings.TrimSpace(entry.Actor) == "" || len(entry.Actor) > maxActorBytes || entry.Counter == 0 || (index > 0 && entry.Actor <= previous) {
			return replica.Frontier{}, errInvalidWire
		}
		frontier[entry.Actor] = entry.Counter
		previous = entry.Actor
	}
	vector, err := replica.NewFrontier(frontier)
	if err != nil {
		return replica.Frontier{}, errInvalidWire
	}
	return vector, nil
}
