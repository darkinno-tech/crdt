package richtext

import (
	"bytes"
	"sort"
	"unicode/utf8"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
	"github.com/DarkInno/crdt/text"
)

type richState struct {
	textState []byte
	marks     map[text.Position]markSet
}

// MarshalBinary returns one canonical rich-text delta frame.
func (d Delta) MarshalBinary() ([]byte, error) {
	return d.MarshalBinaryWithLimits(frame.DefaultLimits())
}

// MarshalBinaryWithLimits returns one canonical rich-text delta while applying
// caller-selected bounds before allocating an outer frame payload.
func (d Delta) MarshalBinaryWithLimits(limits frame.DecoderLimits) ([]byte, error) {
	return marshalDelta(d, limits)
}

// UnmarshalDelta decodes one bounded canonical rich-text delta frame.
func UnmarshalDelta(data []byte) (Delta, error) {
	return UnmarshalDeltaWithLimits(data, frame.DefaultLimits())
}

// UnmarshalDeltaWithLimits decodes one bounded canonical rich-text delta
// frame. It rejects wrong nested types, non-canonical ordering, and any
// trailing payload before returning a usable delta.
func UnmarshalDeltaWithLimits(data []byte, limits frame.DecoderLimits) (Delta, error) {
	decoded, err := frame.UnmarshalFrame(data, limits)
	if err != nil || decoded.TypeID != crdt.TypeIDRichTextDelta || decoded.CodecID != "" {
		return Delta{}, ErrInvalidDelta
	}
	delta, err := decodeDeltaPayload(decoded.Payload, limits)
	if err != nil {
		return Delta{}, err
	}
	canonical, err := marshalDelta(delta, limits)
	if err != nil || !bytes.Equal(canonical, data) {
		return Delta{}, ErrInvalidDelta
	}
	return delta, nil
}

func marshalDelta(delta Delta, limits frame.DecoderLimits) ([]byte, error) {
	if err := validateDelta(delta, limits); err != nil {
		return nil, err
	}
	payload := make([]byte, 0, len(delta.textDelta)+64)
	var err error
	if payload, err = appendBytes(payload, delta.textDelta, limits); err != nil {
		return nil, err
	}
	payload = frame.AppendUvarint(payload, uint64(len(delta.operations)))
	if err := checkPayloadLimit(payload, limits); err != nil {
		return nil, err
	}
	for _, operation := range delta.operations {
		payload = frame.AppendTag(payload, operation.tag)
		payload = frame.AppendUvarint(payload, uint64(len(operation.targets)))
		for _, target := range operation.targets {
			payload = frame.AppendTag(payload, target)
		}
		payload = frame.AppendUvarint(payload, uint64(len(operation.changes)))
		for _, change := range operation.changes {
			if payload, err = appendString(payload, change.Key, limits); err != nil {
				return nil, err
			}
			if change.Remove {
				payload = frame.AppendUvarint(payload, 1)
			} else {
				payload = frame.AppendUvarint(payload, 0)
				if payload, err = appendString(payload, change.Value, limits); err != nil {
					return nil, err
				}
			}
			if err := checkPayloadLimit(payload, limits); err != nil {
				return nil, err
			}
		}
	}
	if err := checkPayloadLimit(payload, limits); err != nil {
		return nil, err
	}
	return frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRichTextDelta, Payload: payload})
}

func decodeDeltaPayload(payload []byte, limits frame.DecoderLimits) (Delta, error) {
	position := 0
	textBytes, next, ok := frame.ReadBytes(payload, position, limits.MaxPayload)
	if !ok {
		return Delta{}, ErrInvalidDelta
	}
	position = next
	delta := Delta{textDelta: append([]byte(nil), textBytes...)}
	if len(delta.textDelta) > 0 {
		if _, err := text.UnmarshalRGARunDeltaWithLimits(delta.textDelta, limits); err != nil {
			return Delta{}, ErrInvalidDelta
		}
	}
	operationCount, next, ok := frame.ReadUvarint(payload, position)
	if !ok || operationCount > uint64(limits.MaxElements) {
		return Delta{}, ErrInvalidDelta
	}
	position = next
	delta.operations = make([]formatOperation, 0, int(operationCount))
	targetCount := 0
	for index := uint64(0); index < operationCount; index++ {
		tag, next, ok := frame.ReadTag(payload, position, limits.MaxStringBytes)
		if !ok {
			return Delta{}, ErrInvalidDelta
		}
		position = next
		targets, next, ok := readTargets(payload, position, limits, &targetCount)
		if !ok {
			return Delta{}, ErrInvalidDelta
		}
		position = next
		changes, next, ok := readChanges(payload, position, limits)
		if !ok {
			return Delta{}, ErrInvalidDelta
		}
		position = next
		delta.operations = append(delta.operations, formatOperation{tag: tag, targets: targets, changes: changes})
	}
	if position != len(payload) || validateDelta(delta, limits) != nil {
		return Delta{}, ErrInvalidDelta
	}
	return delta, nil
}

func readTargets(payload []byte, position int, limits frame.DecoderLimits, total *int) ([]text.Position, int, bool) {
	count, next, ok := frame.ReadUvarint(payload, position)
	if !ok || count == 0 || count > uint64(limits.MaxElements-*total) {
		return nil, position, false
	}
	position = next
	targets := make([]text.Position, 0, int(count))
	for index := uint64(0); index < count; index++ {
		target, next, ok := frame.ReadTag(payload, position, limits.MaxStringBytes)
		if !ok || (len(targets) > 0 && targets[len(targets)-1].Compare(target) >= 0) {
			return nil, position, false
		}
		targets = append(targets, target)
		position = next
	}
	*total += len(targets)
	return targets, position, true
}

func readChanges(payload []byte, position int, limits frame.DecoderLimits) ([]AttributeChange, int, bool) {
	count, next, ok := frame.ReadUvarint(payload, position)
	if !ok || count == 0 || count > uint64(limits.MaxElements) {
		return nil, position, false
	}
	position = next
	changes := make([]AttributeChange, 0, int(count))
	for index := uint64(0); index < count; index++ {
		key, next, ok := frame.ReadBytes(payload, position, limits.MaxStringBytes)
		if !ok {
			return nil, position, false
		}
		position = next
		kind, next, ok := frame.ReadUvarint(payload, position)
		if !ok || kind > 1 {
			return nil, position, false
		}
		position = next
		change := AttributeChange{Key: string(key), Remove: kind == 1}
		if !change.Remove {
			value, next, ok := frame.ReadBytes(payload, position, limits.MaxStringBytes)
			if !ok {
				return nil, position, false
			}
			change.Value = string(value)
			position = next
		}
		if (len(changes) > 0 && changes[len(changes)-1].Key >= change.Key) || validateChanges([]AttributeChange{change}) != nil {
			return nil, position, false
		}
		changes = append(changes, change)
	}
	return changes, position, true
}

func validateDelta(delta Delta, limits frame.DecoderLimits) error {
	if !validLimits(limits) || len(delta.textDelta) > limits.MaxPayload || len(delta.operations) > limits.MaxElements {
		return ErrInvalidDelta
	}
	if len(delta.textDelta) > 0 {
		if _, err := text.UnmarshalRGARunDeltaWithLimits(delta.textDelta, limits); err != nil {
			return ErrInvalidDelta
		}
	}
	if err := validateOperations(delta.operations); err != nil {
		return ErrInvalidDelta
	}
	targetCount := 0
	for _, operation := range delta.operations {
		if len(operation.changes) > limits.MaxElements || len(operation.targets) > limits.MaxElements-targetCount {
			return ErrInvalidDelta
		}
		targetCount += len(operation.targets)
		for _, change := range operation.changes {
			if len(change.Key) > limits.MaxStringBytes || len(change.Value) > limits.MaxStringBytes {
				return ErrInvalidDelta
			}
		}
	}
	return nil
}

// MarshalBinary returns one canonical rich-text state frame.
func (d *Document) MarshalBinary() ([]byte, error) {
	return d.MarshalBinaryWithLimits(frame.DefaultLimits())
}

// MarshalBinaryWithLimits returns a complete rich-text state. It refuses any
// incomplete RGA state through the nested run-v2 state encoder.
func (d *Document) MarshalBinaryWithLimits(limits frame.DecoderLimits) ([]byte, error) {
	if d == nil || d.text == nil {
		return nil, ErrNilDocument
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	textState, err := d.text.MarshalRunBinaryWithLimits(limits)
	if err != nil {
		return nil, err
	}
	return marshalState(richState{textState: textState, marks: cloneMarks(d.marks)}, limits)
}

func marshalState(state richState, limits frame.DecoderLimits) ([]byte, error) {
	if !validLimits(limits) || len(state.marks) > limits.MaxElements {
		return nil, ErrInvalidDelta
	}
	if err := validateNestedState(state.textState, limits); err != nil {
		return nil, err
	}
	entries := sortedMarkEntries(state.marks)
	if len(entries) > limits.MaxElements {
		return nil, ErrInvalidDelta
	}
	for _, entry := range entries {
		if !entry.position.Valid() || entry.key == "" || !utf8ValidString(entry.key) || !entry.value.tag.Valid() ||
			!utf8ValidString(entry.value.value) || (entry.value.deleted && entry.value.value != "") {
			return nil, ErrInvalidDelta
		}
	}
	payload := make([]byte, 0, len(state.textState)+64)
	var err error
	if payload, err = appendBytes(payload, state.textState, limits); err != nil {
		return nil, err
	}
	payload = frame.AppendUvarint(payload, uint64(len(entries)))
	for _, entry := range entries {
		payload = frame.AppendTag(payload, entry.position)
		if payload, err = appendString(payload, entry.key, limits); err != nil {
			return nil, err
		}
		payload = frame.AppendTag(payload, entry.value.tag)
		if entry.value.deleted {
			payload = frame.AppendUvarint(payload, 1)
		} else {
			payload = frame.AppendUvarint(payload, 0)
			if payload, err = appendString(payload, entry.value.value, limits); err != nil {
				return nil, err
			}
		}
		if err := checkPayloadLimit(payload, limits); err != nil {
			return nil, err
		}
	}
	if err := checkPayloadLimit(payload, limits); err != nil {
		return nil, err
	}
	return frame.MarshalFrame(frame.Frame{TypeID: crdt.TypeIDRichTextState, Payload: payload})
}

// UnmarshalBinary installs one complete rich-text state after full decode and
// canonical validation. A failed state leaves document content unchanged.
func (d *Document) UnmarshalBinary(data []byte) error {
	return d.UnmarshalBinaryWithLimits(data, frame.DefaultLimits())
}

// UnmarshalBinaryWithLimits installs a complete rich-text state with caller
// selected decoder limits.
func (d *Document) UnmarshalBinaryWithLimits(data []byte, limits frame.DecoderLimits) error {
	if d == nil || d.text == nil {
		return ErrNilDocument
	}
	state, err := unmarshalState(data, limits)
	if err != nil {
		return err
	}
	if countMarks(state.marks) > d.options.MaxMarkEntries {
		return ErrResourceLimit
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.text.UnmarshalRunBinaryWithLimits(state.textState, limits); err != nil {
		return err
	}
	if tag, ok := greatestMarkTag(state.marks); ok {
		if err := d.text.WitnessTag(tag); err != nil {
			return err
		}
	}
	d.marks = state.marks
	d.markCount = countMarks(state.marks)
	return nil
}

func unmarshalState(data []byte, limits frame.DecoderLimits) (richState, error) {
	decoded, err := frame.UnmarshalFrame(data, limits)
	if err != nil || decoded.TypeID != crdt.TypeIDRichTextState || decoded.CodecID != "" {
		return richState{}, ErrInvalidDelta
	}
	position := 0
	textState, next, ok := frame.ReadBytes(decoded.Payload, position, limits.MaxPayload)
	if !ok || validateNestedState(textState, limits) != nil {
		return richState{}, ErrInvalidDelta
	}
	position = next
	count, next, ok := frame.ReadUvarint(decoded.Payload, position)
	if !ok || count > uint64(limits.MaxElements) {
		return richState{}, ErrInvalidDelta
	}
	position = next
	marks := make(map[text.Position]markSet, int(count))
	var previousPosition text.Position
	previousKey := ""
	for index := uint64(0); index < count; index++ {
		markPosition, next, ok := frame.ReadTag(decoded.Payload, position, limits.MaxStringBytes)
		if !ok {
			return richState{}, ErrInvalidDelta
		}
		position = next
		key, next, ok := frame.ReadBytes(decoded.Payload, position, limits.MaxStringBytes)
		if !ok || len(key) == 0 {
			return richState{}, ErrInvalidDelta
		}
		position = next
		tag, next, ok := frame.ReadTag(decoded.Payload, position, limits.MaxStringBytes)
		if !ok {
			return richState{}, ErrInvalidDelta
		}
		position = next
		kind, next, ok := frame.ReadUvarint(decoded.Payload, position)
		if !ok || kind > 1 {
			return richState{}, ErrInvalidDelta
		}
		position = next
		value := markValue{tag: tag, deleted: kind == 1}
		if !value.deleted {
			encodedValue, next, ok := frame.ReadBytes(decoded.Payload, position, limits.MaxStringBytes)
			if !ok {
				return richState{}, ErrInvalidDelta
			}
			value.value = string(encodedValue)
			position = next
		}
		if (index > 0 && (previousPosition.Compare(markPosition) > 0 || (previousPosition == markPosition && previousKey >= string(key)))) ||
			!markPosition.Valid() || !utf8ValidString(string(key)) || !utf8ValidString(value.value) || (value.deleted && value.value != "") {
			return richState{}, ErrInvalidDelta
		}
		entries := marks[markPosition]
		if _, exists := entries.get(string(key)); exists {
			return richState{}, ErrInvalidDelta
		}
		entries.put(string(key), value)
		marks[markPosition] = entries
		previousPosition, previousKey = markPosition, string(key)
	}
	state := richState{textState: append([]byte(nil), textState...), marks: marks}
	if position != len(decoded.Payload) {
		return richState{}, ErrInvalidDelta
	}
	canonical, err := marshalState(state, limits)
	if err != nil || !bytes.Equal(canonical, data) {
		return richState{}, ErrInvalidDelta
	}
	return state, nil
}

func validateNestedState(data []byte, limits frame.DecoderLimits) error {
	validator, err := text.New("richtext-validator")
	if err != nil {
		return err
	}
	return validator.UnmarshalRunBinaryWithLimits(data, limits)
}

// SnapshotCurrentState returns an HLC-backed rich-text snapshot. Persist the
// state and its clock atomically before the replica ID is reused.
func (d *Document) SnapshotCurrentState() (snapshot.Snapshot, error) {
	return d.SnapshotCurrentStateWithLimits(frame.DefaultLimits())
}

// SnapshotCurrentStateWithLimits returns a validated HLC-backed snapshot with
// caller-selected frame limits.
func (d *Document) SnapshotCurrentStateWithLimits(limits frame.DecoderLimits) (snapshot.Snapshot, error) {
	if d == nil || d.text == nil {
		return snapshot.Snapshot{}, ErrNilDocument
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	textState, err := d.text.MarshalRunBinaryWithLimits(limits)
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	state, err := marshalState(richState{textState: textState, marks: cloneMarks(d.marks)}, limits)
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	textSnapshot, err := d.text.SnapshotRunCurrentStateWithLimits(limits)
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	frontier := textSnapshot.Frontier()
	for _, entries := range d.marks {
		entries.rangeValues(func(_ string, value markValue) {
			if current, exists := frontier[value.tag.ReplicaID]; !exists || current.Compare(value.tag) < 0 {
				frontier[value.tag.ReplicaID] = value.tag
			}
		})
	}
	return snapshot.NewValidatedWithClockState(state, frontier, d.text.ClockState(), validateRichTextState)
}

func validateRichTextState(data []byte) error {
	_, err := unmarshalState(data, frame.DefaultLimits())
	return err
}

// MarshalJSON returns a diagnostic summary for one delta and never includes
// text content, attributes, tags, or frame bytes.
func (d Delta) MarshalJSON() ([]byte, error) {
	return crdt.MarshalDiagnosticJSON(crdt.StateSnapshot{Type: "rich-text-delta", ElementCount: len(d.operations)})
}

type markEntry struct {
	position text.Position
	key      string
	value    markValue
}

func sortedMarkEntries(marks map[text.Position]markSet) []markEntry {
	entries := make([]markEntry, 0, countMarks(marks))
	for position, attributes := range marks {
		attributes.rangeValues(func(key string, value markValue) {
			entries = append(entries, markEntry{position: position, key: key, value: value})
		})
	}
	sort.Slice(entries, func(left, right int) bool {
		if comparison := entries[left].position.Compare(entries[right].position); comparison != 0 {
			return comparison < 0
		}
		return entries[left].key < entries[right].key
	})
	return entries
}

func countMarks(marks map[text.Position]markSet) int {
	count := 0
	for _, entries := range marks {
		count += entries.len()
	}
	return count
}

func appendBytes(payload, value []byte, limits frame.DecoderLimits) ([]byte, error) {
	if len(value) > limits.MaxPayload {
		return nil, frame.ErrFrameLimit
	}
	payload = frame.AppendUvarint(payload, uint64(len(value)))
	payload = append(payload, value...)
	if err := checkPayloadLimit(payload, limits); err != nil {
		return nil, err
	}
	return payload, nil
}

func appendString(payload []byte, value string, limits frame.DecoderLimits) ([]byte, error) {
	if len(value) > limits.MaxStringBytes || !utf8ValidString(value) {
		return nil, frame.ErrFrameLimit
	}
	payload = frame.AppendUvarint(payload, uint64(len(value)))
	payload = append(payload, value...)
	if err := checkPayloadLimit(payload, limits); err != nil {
		return nil, err
	}
	return payload, nil
}

func checkPayloadLimit(payload []byte, limits frame.DecoderLimits) error {
	if len(payload) > limits.MaxPayload {
		return frame.ErrFrameLimit
	}
	return nil
}

func validLimits(limits frame.DecoderLimits) bool {
	return limits.MaxFrameBytes > 0 && limits.MaxPayload > 0 && limits.MaxCodecID > 0 && limits.MaxElements > 0 &&
		limits.MaxTags > 0 && limits.MaxStringBytes > 0
}

func utf8ValidString(value string) bool { return utf8.ValidString(value) }
