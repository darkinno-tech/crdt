package persistence

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"sort"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

const (
	// recordVersion remains the v1 value for legacy test fixtures. New writes
	// take their version from Config.Format.
	recordVersion      byte = RecordFormatV1
	recordClockPresent byte = 1
)

var recordMagic = [...]byte{'C', 'R', 'C', 'P'}

func normalizeCheckpoint(checkpoint Checkpoint, config Config) (Checkpoint, error) {
	state := checkpoint.Snapshot.Bytes()
	frontier := checkpoint.Snapshot.Frontier()
	if len(state) == 0 || len(state) > config.MaxStateBytes || len(frontier) > config.MaxFrontierEntries || len(checkpoint.Outbox) > config.MaxOutboxBytes {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	for replicaID, tag := range frontier {
		if len(replicaID) == 0 || len(replicaID) > config.MaxReplicaIDBytes || replicaID != tag.ReplicaID || !tag.Valid() {
			return Checkpoint{}, ErrInvalidCheckpoint
		}
	}
	decoded, err := frame.UnmarshalFrame(state, frame.DefaultLimits())
	if err != nil {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	kind, ok := crdt.FrameTypeForState(decoded.TypeID)
	if !ok {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	clockState, hasClock := checkpoint.Snapshot.ClockState()
	var normalized snapshot.Snapshot
	if kind.UsesHLC {
		if !hasClock || len(clockState.ReplicaID) > config.MaxReplicaIDBytes {
			return Checkpoint{}, ErrInvalidCheckpoint
		}
		normalized, err = snapshot.NewValidatedWithClockState(state, frontier, clockState, config.Validate)
	} else {
		if hasClock {
			return Checkpoint{}, ErrInvalidCheckpoint
		}
		normalized, err = snapshot.NewValidated(state, frontier, config.Validate)
	}
	if err != nil {
		return Checkpoint{}, ErrInvalidCheckpoint
	}
	result := Checkpoint{Snapshot: normalized, Cursor: checkpoint.Cursor, Outbox: append([]byte(nil), checkpoint.Outbox...)}
	if _, err := checkpointSize(result, config); err != nil {
		return Checkpoint{}, err
	}
	return result, nil
}

func marshalCheckpoint(checkpoint Checkpoint, config Config) ([]byte, error) {
	format, err := config.Format.normalized()
	if err != nil {
		return nil, ErrInvalidCheckpoint
	}
	size, err := checkpointSize(checkpoint, config)
	if err != nil {
		return nil, err
	}
	state := checkpoint.Snapshot.Bytes()
	frontier := checkpoint.Snapshot.Frontier()
	clockState, hasClock := checkpoint.Snapshot.ClockState()
	encoded := make([]byte, 0, size)
	encoded = append(encoded, recordMagic[:]...)
	encoded = append(encoded, format.Version)
	if hasClock {
		encoded = append(encoded, recordClockPresent)
	} else {
		encoded = append(encoded, 0)
	}
	encoded = appendBytes(encoded, state)
	encoded = frame.AppendUvarint(encoded, uint64(len(frontier)))
	for _, replicaID := range sortedReplicaIDs(frontier) {
		encoded = frame.AppendTag(encoded, frontier[replicaID])
	}
	if hasClock {
		encoded = frame.AppendTag(encoded, crdt.Tag{ReplicaID: clockState.ReplicaID, WallTime: clockState.WallTime, Logical: clockState.Logical})
	}
	encoded = frame.AppendUvarint(encoded, checkpoint.Cursor)
	encoded = appendBytes(encoded, checkpoint.Outbox)
	digest := sha256.Sum256(encoded)
	encoded = append(encoded, digest[:]...)
	if len(encoded) != size {
		return nil, ErrInvalidCheckpoint
	}
	return encoded, nil
}

func unmarshalCheckpoint(data []byte, config Config) (Checkpoint, error) {
	checkpoint, version, err := decodeCheckpoint(data, config)
	if err != nil {
		return Checkpoint{}, err
	}
	if !config.Format.MigrateOnLoad || version == effectiveFormat(config).Version {
		return checkpoint, nil
	}
	return migrateCheckpoint(checkpoint, version, config)
}

func decodeCheckpoint(data []byte, config Config) (Checkpoint, byte, error) {
	format, err := config.Format.normalized()
	if err != nil {
		return Checkpoint{}, 0, ErrCorruptStore
	}
	if len(data) < len(recordMagic)+2+sha256.Size || len(data) > config.MaxRecordBytes {
		return Checkpoint{}, 0, ErrCorruptStore
	}
	payloadEnd := len(data) - sha256.Size
	actual := sha256.Sum256(data[:payloadEnd])
	if !bytes.Equal(actual[:], data[payloadEnd:]) {
		return Checkpoint{}, 0, ErrCorruptStore
	}
	version := data[len(recordMagic)]
	if string(data[:len(recordMagic)]) != string(recordMagic[:]) || !format.accepts(version) {
		return Checkpoint{}, 0, ErrCorruptStore
	}
	flags := data[len(recordMagic)+1]
	if flags != 0 && flags != recordClockPresent {
		return Checkpoint{}, 0, ErrCorruptStore
	}
	position := len(recordMagic) + 2
	state, next, ok := frame.ReadBytes(data[:payloadEnd], position, config.MaxStateBytes)
	if !ok || len(state) == 0 {
		return Checkpoint{}, 0, ErrCorruptStore
	}
	position = next
	frontierCount, next, ok := frame.ReadUvarint(data[:payloadEnd], position)
	if !ok || frontierCount > uint64(config.MaxFrontierEntries) {
		return Checkpoint{}, 0, ErrCorruptStore
	}
	position = next
	frontier := make(map[string]crdt.Tag, int(frontierCount))
	previousReplicaID := ""
	for index := uint64(0); index < frontierCount; index++ {
		tag, next, ok := frame.ReadTag(data[:payloadEnd], position, config.MaxReplicaIDBytes)
		if !ok || tag.ReplicaID <= previousReplicaID {
			return Checkpoint{}, 0, ErrCorruptStore
		}
		frontier[tag.ReplicaID] = tag
		previousReplicaID = tag.ReplicaID
		position = next
	}
	var clockState *clock.State
	if flags == recordClockPresent {
		tag, next, ok := frame.ReadTag(data[:payloadEnd], position, config.MaxReplicaIDBytes)
		if !ok {
			return Checkpoint{}, 0, ErrCorruptStore
		}
		clockState = &clock.State{ReplicaID: tag.ReplicaID, WallTime: tag.WallTime, Logical: tag.Logical}
		position = next
	}
	cursor, next, ok := frame.ReadUvarint(data[:payloadEnd], position)
	if !ok {
		return Checkpoint{}, 0, ErrCorruptStore
	}
	position = next
	outbox, next, ok := frame.ReadBytes(data[:payloadEnd], position, config.MaxOutboxBytes)
	if !ok || next != payloadEnd {
		return Checkpoint{}, 0, ErrCorruptStore
	}
	checkpoint, err := decodedCheckpointWithValidator(state, frontier, clockState, cursor, outbox, config, config.validatorFor(version))
	if err != nil {
		return Checkpoint{}, 0, ErrCorruptStore
	}
	return checkpoint, version, nil
}

func decodedCheckpoint(state []byte, frontier map[string]crdt.Tag, clockState *clock.State, cursor uint64, outbox []byte, config Config) (Checkpoint, error) {
	return decodedCheckpointWithValidator(state, frontier, clockState, cursor, outbox, config, config.Validate)
}

func decodedCheckpointWithValidator(state []byte, frontier map[string]crdt.Tag, clockState *clock.State, cursor uint64, outbox []byte, config Config, validator snapshot.StateValidator) (Checkpoint, error) {
	decoded, err := frame.UnmarshalFrame(state, frame.DefaultLimits())
	if err != nil {
		return Checkpoint{}, err
	}
	kind, ok := crdt.FrameTypeForState(decoded.TypeID)
	if !ok {
		return Checkpoint{}, errors.New("unknown state type")
	}
	var saved snapshot.Snapshot
	if kind.UsesHLC {
		if clockState == nil {
			return Checkpoint{}, errors.New("missing HLC state")
		}
		saved, err = snapshot.NewValidatedWithClockState(state, frontier, *clockState, validator)
	} else {
		if clockState != nil {
			return Checkpoint{}, errors.New("unexpected HLC state")
		}
		saved, err = snapshot.NewValidated(state, frontier, validator)
	}
	if err != nil {
		return Checkpoint{}, err
	}
	return Checkpoint{Snapshot: saved, Cursor: cursor, Outbox: append([]byte(nil), outbox...)}, nil
}

func effectiveFormat(config Config) FormatConfig {
	format, err := config.Format.normalized()
	if err != nil {
		return FormatConfig{}
	}
	return format
}

func migrateCheckpoint(checkpoint Checkpoint, fromVersion byte, config Config) (Checkpoint, error) {
	format := effectiveFormat(config)
	if fromVersion == format.Version || !format.accepts(fromVersion) {
		return Checkpoint{}, ErrMigration
	}
	if migration, ok := format.migration(fromVersion); ok && migration.Transform != nil {
		var err error
		checkpoint, err = applyMigration(migration.Transform, checkpoint)
		if err != nil {
			return Checkpoint{}, ErrMigration
		}
	}
	normalized, err := normalizeCheckpoint(checkpoint, config)
	if err != nil {
		return Checkpoint{}, ErrMigration
	}
	return normalized, nil
}

func applyMigration(transform CheckpointMigration, checkpoint Checkpoint) (result Checkpoint, err error) {
	defer func() {
		if recover() != nil {
			result = Checkpoint{}
			err = ErrMigration
		}
	}()
	return transform(checkpoint)
}

func checkpointSize(checkpoint Checkpoint, config Config) (int, error) {
	state := checkpoint.Snapshot.Bytes()
	frontier := checkpoint.Snapshot.Frontier()
	clockState, hasClock := checkpoint.Snapshot.ClockState()
	size := 0
	for _, part := range []int{
		len(recordMagic),
		2,
		frame.UvarintSize(uint64(len(state))),
		len(state),
		frame.UvarintSize(uint64(len(frontier))),
		frame.UvarintSize(checkpoint.Cursor),
		frame.UvarintSize(uint64(len(checkpoint.Outbox))),
		len(checkpoint.Outbox),
		sha256.Size,
	} {
		if !addRecordSize(&size, part, config.MaxRecordBytes) {
			return 0, ErrInvalidCheckpoint
		}
	}
	for _, replicaID := range sortedReplicaIDs(frontier) {
		tag := frontier[replicaID]
		if !addRecordSize(&size, frame.TagSize(tag), config.MaxRecordBytes) {
			return 0, ErrInvalidCheckpoint
		}
	}
	if hasClock {
		tag := crdt.Tag{ReplicaID: clockState.ReplicaID, WallTime: clockState.WallTime, Logical: clockState.Logical}
		if !addRecordSize(&size, frame.TagSize(tag), config.MaxRecordBytes) {
			return 0, ErrInvalidCheckpoint
		}
	}
	return size, nil
}

func addRecordSize(size *int, additional, maximum int) bool {
	if size == nil || additional < 0 || maximum < 0 || *size > maximum || additional > maximum-*size {
		return false
	}
	*size += additional
	return true
}

func sortedReplicaIDs(frontier map[string]crdt.Tag) []string {
	ids := make([]string, 0, len(frontier))
	for replicaID := range frontier {
		ids = append(ids, replicaID)
	}
	sort.Strings(ids)
	return ids
}

func appendBytes(dst, value []byte) []byte {
	dst = frame.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}
