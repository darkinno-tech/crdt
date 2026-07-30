// Package richtext implements bounded, inline formatted collaborative text.
//
// It composes a private run-v2 text RGA with per-position LWW attribute
// registers. The package deliberately accepts opaque string attributes rather
// than HTML, CSS, or executable values; rendering and attribute schemas remain
// application-owned.
package richtext

import (
	"errors"
	"sort"
	"sync"
	"unicode/utf8"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
	"github.com/DarkInno/crdt/text"
)

const (
	// SemanticsVersion identifies the rich-text inline-formatting contract.
	// It must match the value negotiated in a replica manifest.
	SemanticsVersion uint64 = 1
)

var (
	ErrNilDocument      = errors.New("richtext: nil document")
	ErrInvalidAttribute = errors.New("richtext: invalid attribute")
	ErrInvalidDelta     = errors.New("richtext: invalid delta")
	ErrTagConflict      = errors.New("richtext: conflicting attribute for one tag")
	ErrResourceLimit    = errors.New("richtext: resource limit exceeded")
)

// Attributes is the presentation-safe view of one span's live attributes.
// Values are opaque UTF-8 strings. Their meaning, validation, and rendering
// policy are owned by the application.
type Attributes map[string]string

// AttributeChange describes one formatting assignment or retained removal.
// Remove must be true with an empty Value to make deletion unambiguous.
type AttributeChange struct {
	Key    string
	Value  string
	Remove bool
}

// Span is a maximal contiguous visible text run with equal live attributes.
// A nil Attributes map means that the run has no active attributes.
type Span struct {
	Text       string
	Attributes Attributes
}

// Options bounds the text and formatting metadata retained for one document.
// Every value must be positive.
type Options struct {
	Text                      text.Options
	MaxMarkEntries            int
	MaxAttributesPerOperation int
}

// DefaultOptions returns conservative per-document limits. Applications that
// accept untrusted peers should lower them to their authenticated group budget.
func DefaultOptions() Options {
	return Options{
		Text:                      text.DefaultOptions(),
		MaxMarkEntries:            1 << 20,
		MaxAttributesPerOperation: 64,
	}
}

func (o Options) valid() bool {
	return o.Text.MaxNodes > 0 && o.Text.MaxTombstones > 0 && o.Text.MaxPendingNodes > 0 &&
		o.Text.MaxPendingBytes > 0 && o.MaxMarkEntries > 0 && o.MaxAttributesPerOperation > 0
}

type markValue struct {
	tag     crdt.Tag
	value   string
	deleted bool
}

type formatOperation struct {
	tag     crdt.Tag
	targets []text.Position
	changes []AttributeChange
}

// Delta is an opaque, canonical rich-text delta. It may contain a nested
// run-v2 RGA delta, formatting operations, or both.
type Delta struct {
	textDelta  []byte
	operations []formatOperation
}

// Document is a concurrent-safe, inline rich-text CRDT. Its RGA is private so
// compound text-and-format deltas cannot be bypassed by a caller mutating the
// text substrate independently.
type Document struct {
	mu        sync.RWMutex
	text      *text.RGA
	options   Options
	marks     map[text.Position]map[string]markValue
	markCount int
}

var _ crdt.CRDT[*Document] = (*Document)(nil)
var _ crdt.DeltaCapable[*Document, Delta] = (*Document)(nil)

// New constructs a document with default bounds.
func New(replicaID string) (*Document, error) { return NewWithOptions(replicaID, DefaultOptions()) }

// NewWithOptions constructs a document with explicit text and format bounds.
func NewWithOptions(replicaID string, options Options) (*Document, error) {
	if !options.valid() {
		return nil, ErrResourceLimit
	}
	value, err := text.NewWithOptions(replicaID, options.Text)
	if err != nil {
		return nil, err
	}
	return &Document{text: value, options: options, marks: make(map[text.Position]map[string]markValue)}, nil
}

// NewFromClock restores a document whose RGA HLC state was persisted with a
// rich-text snapshot before its replica ID is reused.
func NewFromClock(state clock.State) (*Document, error) {
	return NewFromClockWithOptions(state, DefaultOptions())
}

// NewFromClockWithOptions restores a document with explicit bounds.
func NewFromClockWithOptions(state clock.State, options Options) (*Document, error) {
	if !options.valid() {
		return nil, ErrResourceLimit
	}
	value, err := text.NewFromClockWithOptions(state, options.Text)
	if err != nil {
		return nil, err
	}
	return &Document{text: value, options: options, marks: make(map[text.Position]map[string]markValue)}, nil
}

// ClockState returns the shared RGA clock state that must be saved atomically
// with MarshalBinary or SnapshotCurrentState before a replica ID is reused.
func (d *Document) ClockState() clock.State {
	if d == nil || d.text == nil {
		return clock.State{}
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.text.ClockState()
}

// String returns visible text without formatting metadata.
func (d *Document) String() string {
	if d == nil || d.text == nil {
		return ""
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.text.String()
}

// Len returns the number of visible Unicode scalar values.
func (d *Document) Len() int {
	if d == nil || d.text == nil {
		return 0
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.text.Positions())
}

// AttributesAt returns a copy of the live attributes at a visible rune offset.
func (d *Document) AttributesAt(offset int) (Attributes, bool) {
	if d == nil || d.text == nil || offset < 0 {
		return nil, false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	positions := d.text.Positions()
	if offset >= len(positions) {
		return nil, false
	}
	return d.attributesForPositionLocked(positions[offset]), true
}

// Spans materializes visible text into maximal adjacent runs that share equal
// live attributes. Returned maps are safe for the caller to modify.
func (d *Document) Spans() []Span {
	if d == nil || d.text == nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	positions, runes := d.text.Positions(), []rune(d.text.String())
	if len(positions) == 0 || len(runes) == 0 {
		return nil
	}
	spanCapacity := len(runes)
	if spanCapacity > 64 {
		spanCapacity = 64
	}
	spans := make([]Span, 0, spanCapacity)
	start := 0
	for index := 1; index < len(runes); index++ {
		if liveAttributesEqual(d.marks[positions[start]], d.marks[positions[index]]) {
			continue
		}
		spans = append(spans, Span{
			Text:       string(runes[start:index]),
			Attributes: d.attributesForPositionLocked(positions[start]),
		})
		start = index
	}
	return append(spans, Span{
		Text:       string(runes[start:]),
		Attributes: d.attributesForPositionLocked(positions[start]),
	})
}

// Insert inserts unformatted UTF-8 text at a visible rune offset.
func (d *Document) Insert(offset int, value string) (Delta, error) {
	return d.InsertWithAttributes(offset, value, nil)
}

// InsertWithAttributes inserts text and explicitly applies attributes to only
// the newly inserted positions. It never infers inherited formatting.
func (d *Document) InsertWithAttributes(offset int, value string, attributes Attributes) (Delta, error) {
	return d.InsertWithAttributesWithLimits(offset, value, attributes, frame.DefaultLimits())
}

// InsertWithAttributesWithLimits preflights the complete outer delta before
// mutating document content. A failed preflight may advance the persisted HLC
// while reserving safe-to-skip tags, but it does not add text or formatting.
func (d *Document) InsertWithAttributesWithLimits(offset int, value string, attributes Attributes, limits frame.DecoderLimits) (Delta, error) {
	if d == nil || d.text == nil {
		return Delta{}, ErrNilDocument
	}
	changes, err := attributesToChanges(attributes)
	if err != nil {
		return Delta{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	textDelta, encodedText, err := d.text.PrepareInsertRunBinaryWithLimits(offset, value, limits)
	if err != nil {
		return Delta{}, err
	}
	delta := Delta{textDelta: encodedText}
	if len(changes) > 0 && len(textDelta.NodePositions()) > 0 {
		if len(changes) > d.options.MaxAttributesPerOperation {
			return Delta{}, ErrResourceLimit
		}
		tag, err := d.text.NextTag()
		if err != nil {
			return Delta{}, err
		}
		delta.operations = []formatOperation{{tag: tag, targets: textDelta.NodePositions(), changes: changes}}
	}
	if _, err := delta.MarshalBinaryWithLimits(limits); err != nil {
		return Delta{}, err
	}
	if err := d.preflightOperationsLocked(delta.operations); err != nil {
		return Delta{}, err
	}
	if err := d.text.ApplyDelta(textDelta); err != nil {
		return Delta{}, err
	}
	if err := d.applyOperationsLocked(delta.operations); err != nil {
		return Delta{}, err
	}
	return delta, nil
}

// Delete removes count visible runes beginning at offset.
func (d *Document) Delete(offset, count int) (Delta, error) {
	return d.DeleteWithLimits(offset, count, frame.DefaultLimits())
}

// DeleteWithLimits preflights the outer rich-text delta before adding RGA
// tombstones.
func (d *Document) DeleteWithLimits(offset, count int, limits frame.DecoderLimits) (Delta, error) {
	if d == nil || d.text == nil {
		return Delta{}, ErrNilDocument
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	textDelta, encodedText, err := d.text.PrepareDeleteRunBinaryWithLimits(offset, count, limits)
	if err != nil {
		return Delta{}, err
	}
	delta := Delta{textDelta: encodedText}
	if _, err := delta.MarshalBinaryWithLimits(limits); err != nil {
		return Delta{}, err
	}
	if err := d.text.ApplyDelta(textDelta); err != nil {
		return Delta{}, err
	}
	return delta, nil
}

// Format applies changes to the exact visible positions selected at the time
// of the call. An empty selection returns an empty canonical delta.
func (d *Document) Format(offset, count int, changes []AttributeChange) (Delta, error) {
	return d.FormatWithLimits(offset, count, changes, frame.DefaultLimits())
}

// FormatWithLimits preflights a formatting delta before retained LWW registers
// change. Remove records a tombstone and therefore wins over delayed values.
func (d *Document) FormatWithLimits(offset, count int, changes []AttributeChange, limits frame.DecoderLimits) (Delta, error) {
	if d == nil || d.text == nil {
		return Delta{}, ErrNilDocument
	}
	changes = canonicalChanges(changes)
	if err := validateChanges(changes); err != nil {
		return Delta{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	positions := d.text.Positions()
	if offset < 0 || count < 0 || offset > len(positions) || count > len(positions)-offset {
		return Delta{}, text.ErrRange
	}
	if count == 0 || len(changes) == 0 {
		delta := Delta{}
		_, err := delta.MarshalBinaryWithLimits(limits)
		return delta, err
	}
	if len(changes) > d.options.MaxAttributesPerOperation {
		return Delta{}, ErrResourceLimit
	}
	tag, err := d.text.NextTag()
	if err != nil {
		return Delta{}, err
	}
	delta := Delta{operations: []formatOperation{{
		tag: tag, targets: append([]text.Position(nil), positions[offset:offset+count]...), changes: changes,
	}}}
	if _, err := delta.MarshalBinaryWithLimits(limits); err != nil {
		return Delta{}, err
	}
	if err := d.preflightOperationsLocked(delta.operations); err != nil {
		return Delta{}, err
	}
	if err := d.applyOperationsLocked(delta.operations); err != nil {
		return Delta{}, err
	}
	return delta, nil
}

// ApplyDelta joins one canonical rich-text delta. The entire frame is decoded
// and resource-checked before text or formatting metadata is changed.
func (d *Document) ApplyDelta(delta Delta) error {
	if d == nil || d.text == nil {
		return ErrNilDocument
	}
	if err := validateDelta(delta, frame.DefaultLimits()); err != nil {
		return err
	}
	var textDelta text.Delta
	var err error
	if len(delta.textDelta) > 0 {
		textDelta, err = text.UnmarshalRGARunDelta(delta.textDelta)
		if err != nil {
			return ErrInvalidDelta
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.preflightOperationsLocked(delta.operations); err != nil {
		return err
	}
	if tag, ok := greatestOperationTag(delta.operations); ok {
		if err := d.text.WitnessTag(tag); err != nil {
			return err
		}
	}
	if len(delta.textDelta) > 0 {
		if err := d.text.ApplyDelta(textDelta); err != nil {
			return err
		}
	}
	return d.applyOperationsLocked(delta.operations)
}

// Merge joins another rich-text document without exposing either document's
// mutable RGA. A concurrent update on other is captured as a single state.
func (d *Document) Merge(other *Document) error {
	if d == nil || d.text == nil || other == nil || other.text == nil {
		return ErrNilDocument
	}
	if d == other {
		return nil
	}
	other.mu.RLock()
	state, err := other.text.MarshalRunBinary()
	marks := cloneMarks(other.marks)
	other.mu.RUnlock()
	if err != nil {
		return err
	}
	otherText, err := text.NewWithOptions("richtext-merge", other.options.Text)
	if err != nil {
		return err
	}
	if err := otherText.UnmarshalRunBinary(state); err != nil {
		return ErrInvalidDelta
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.preflightMarksLocked(marks); err != nil {
		return err
	}
	if tag, ok := greatestMarkTag(marks); ok {
		if err := d.text.WitnessTag(tag); err != nil {
			return err
		}
	}
	if err := d.text.Merge(otherText); err != nil {
		return err
	}
	return d.applyMarksLocked(marks)
}

// State returns a diagnostic summary without text, attributes, positions, or
// HLC state. It is not a replication or persistence format.
func (d *Document) State() crdt.StateSnapshot {
	if d == nil || d.text == nil {
		return crdt.StateSnapshot{Type: "rich-text"}
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	state := d.text.State()
	state.Type = "rich-text"
	for _, entries := range d.marks {
		for _, value := range entries {
			if value.deleted {
				state.TombstoneCount++
			}
		}
	}
	return state
}

// MarshalJSON returns the diagnostic state summary, never rich text content.
func (d *Document) MarshalJSON() ([]byte, error) { return crdt.MarshalStateJSON(d) }

func (d *Document) attributesForPositionLocked(position text.Position) Attributes {
	entries := d.marks[position]
	if len(entries) == 0 {
		return nil
	}
	attributes := make(Attributes, len(entries))
	for key, value := range entries {
		if !value.deleted {
			attributes[key] = value.value
		}
	}
	if len(attributes) == 0 {
		return nil
	}
	return attributes
}

func (d *Document) preflightOperationsLocked(operations []formatOperation) error {
	if len(operations) == 0 {
		return nil
	}
	if err := validateOperations(operations); err != nil {
		return err
	}
	// One frame is never allowed to multiply its target and attribute counts
	// into more work than this document can retain. Check this before building
	// the transient pending map: a frame that repeatedly rewrites existing
	// registers could otherwise consume unbounded CPU and memory without adding
	// any retained entries.
	updates := 0
	for _, operation := range operations {
		if len(operation.targets) > (d.options.MaxMarkEntries-updates)/len(operation.changes) {
			return ErrResourceLimit
		}
		updates += len(operation.targets) * len(operation.changes)
	}
	pending := make(map[markKey]markValue)
	for _, operation := range operations {
		if len(operation.changes) > d.options.MaxAttributesPerOperation {
			return ErrResourceLimit
		}
		for _, target := range operation.targets {
			for _, change := range operation.changes {
				key := markKey{position: target, key: change.Key}
				incoming := markValue{tag: operation.tag, value: change.Value, deleted: change.Remove}
				if current, exists := pending[key]; exists {
					if current != incoming {
						return ErrTagConflict
					}
					continue
				}
				if entries := d.marks[target]; entries != nil {
					if current, exists := entries[change.Key]; exists && current.tag == incoming.tag && current != incoming {
						return ErrTagConflict
					}
				}
				pending[key] = incoming
			}
		}
	}
	newEntries := 0
	for key := range pending {
		if entries := d.marks[key.position]; entries == nil {
			newEntries++
		} else if _, exists := entries[key.key]; !exists {
			newEntries++
		}
	}
	if newEntries > d.options.MaxMarkEntries-d.markCount {
		return ErrResourceLimit
	}
	return nil
}

func (d *Document) applyOperationsLocked(operations []formatOperation) error {
	for _, operation := range operations {
		for _, target := range operation.targets {
			entries := d.marks[target]
			if entries == nil {
				entries = make(map[string]markValue, len(operation.changes))
				d.marks[target] = entries
			}
			for _, change := range operation.changes {
				incoming := markValue{tag: operation.tag, value: change.Value, deleted: change.Remove}
				current, exists := entries[change.Key]
				if exists && current.tag.Compare(incoming.tag) > 0 {
					continue
				}
				if !exists {
					d.markCount++
				}
				entries[change.Key] = incoming
			}
		}
	}
	return nil
}

func (d *Document) preflightMarksLocked(marks map[text.Position]map[string]markValue) error {
	pending := make(map[markKey]markValue)
	for position, entries := range marks {
		if !position.Valid() {
			return ErrInvalidDelta
		}
		for key, incoming := range entries {
			if key == "" || !utf8.ValidString(key) || !utf8.ValidString(incoming.value) || !incoming.tag.Valid() ||
				(incoming.deleted && incoming.value != "") {
				return ErrInvalidDelta
			}
			markKey := markKey{position: position, key: key}
			if current, exists := pending[markKey]; exists && current != incoming {
				return ErrTagConflict
			}
			if current, exists := d.marks[position][key]; exists && current.tag == incoming.tag && current != incoming {
				return ErrTagConflict
			}
			pending[markKey] = incoming
		}
	}
	newEntries := 0
	for key := range pending {
		if entries := d.marks[key.position]; entries == nil {
			newEntries++
		} else if _, exists := entries[key.key]; !exists {
			newEntries++
		}
	}
	if newEntries > d.options.MaxMarkEntries-d.markCount {
		return ErrResourceLimit
	}
	return nil
}

func (d *Document) applyMarksLocked(marks map[text.Position]map[string]markValue) error {
	for position, incomingEntries := range marks {
		entries := d.marks[position]
		if entries == nil {
			entries = make(map[string]markValue, len(incomingEntries))
			d.marks[position] = entries
		}
		for key, incoming := range incomingEntries {
			current, exists := entries[key]
			if exists && current.tag.Compare(incoming.tag) > 0 {
				continue
			}
			if !exists {
				d.markCount++
			}
			entries[key] = incoming
		}
	}
	return nil
}

type markKey struct {
	position text.Position
	key      string
}

func attributesToChanges(attributes Attributes) ([]AttributeChange, error) {
	if len(attributes) == 0 {
		return nil, nil
	}
	changes := make([]AttributeChange, 0, len(attributes))
	for key, value := range attributes {
		changes = append(changes, AttributeChange{Key: key, Value: value})
	}
	sort.Slice(changes, func(left, right int) bool { return changes[left].Key < changes[right].Key })
	if err := validateChanges(changes); err != nil {
		return nil, err
	}
	return changes, nil
}

func cloneChanges(changes []AttributeChange) []AttributeChange {
	return append([]AttributeChange(nil), changes...)
}

func canonicalChanges(changes []AttributeChange) []AttributeChange {
	changes = cloneChanges(changes)
	sort.Slice(changes, func(left, right int) bool { return changes[left].Key < changes[right].Key })
	return changes
}

func validateChanges(changes []AttributeChange) error {
	previous := ""
	for index, change := range changes {
		if change.Key == "" || !utf8.ValidString(change.Key) || !utf8.ValidString(change.Value) || (change.Remove && change.Value != "") ||
			(index > 0 && previous >= change.Key) {
			return ErrInvalidAttribute
		}
		previous = change.Key
	}
	return nil
}

func validateOperations(operations []formatOperation) error {
	var previous crdt.Tag
	for index, operation := range operations {
		if !operation.tag.Valid() || len(operation.targets) == 0 || len(operation.changes) == 0 ||
			(index > 0 && previous.Compare(operation.tag) >= 0) || validateChanges(operation.changes) != nil {
			return ErrInvalidDelta
		}
		for targetIndex, target := range operation.targets {
			if !target.Valid() || (targetIndex > 0 && operation.targets[targetIndex-1].Compare(target) >= 0) {
				return ErrInvalidDelta
			}
		}
		previous = operation.tag
	}
	return nil
}

func greatestOperationTag(operations []formatOperation) (crdt.Tag, bool) {
	if len(operations) == 0 {
		return crdt.Tag{}, false
	}
	return operations[len(operations)-1].tag, true
}

func cloneMarks(source map[text.Position]map[string]markValue) map[text.Position]map[string]markValue {
	cloned := make(map[text.Position]map[string]markValue, len(source))
	for position, entries := range source {
		copied := make(map[string]markValue, len(entries))
		for key, value := range entries {
			copied[key] = value
		}
		cloned[position] = copied
	}
	return cloned
}

func greatestMarkTag(marks map[text.Position]map[string]markValue) (crdt.Tag, bool) {
	var greatest crdt.Tag
	ok := false
	for _, entries := range marks {
		for _, value := range entries {
			if !ok || greatest.Compare(value.tag) < 0 {
				greatest, ok = value.tag, true
			}
		}
	}
	return greatest, ok
}

func attributesEqual(left, right Attributes) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

// liveAttributesEqual compares presentation values without allocating the
// caller-owned maps returned by AttributesAt or Spans. HLC tags are excluded:
// two registers with equal live values render as one span even when they were
// written by different replicas.
func liveAttributesEqual(left, right map[string]markValue) bool {
	leftLive := 0
	for _, value := range left {
		if !value.deleted {
			leftLive++
		}
	}
	rightLive := 0
	for _, value := range right {
		if !value.deleted {
			rightLive++
		}
	}
	if leftLive != rightLive {
		return false
	}
	for key, value := range left {
		if value.deleted {
			continue
		}
		other, exists := right[key]
		if !exists || other.deleted || other.value != value.value {
			return false
		}
	}
	return true
}

// NewFromSnapshot restores a complete rich-text snapshot with default bounds.
func NewFromSnapshot(saved snapshot.Snapshot) (*Document, error) {
	return NewFromSnapshotWithOptions(saved, DefaultOptions(), frame.DefaultLimits())
}

// NewFromSnapshotWithOptions validates and restores a rich-text snapshot
// under explicit document and decoder limits.
func NewFromSnapshotWithOptions(saved snapshot.Snapshot, options Options, limits frame.DecoderLimits) (*Document, error) {
	if saved.TypeID != crdt.TypeIDRichTextState {
		return nil, ErrInvalidDelta
	}
	clockState, ok := saved.ClockState()
	if !ok {
		return nil, ErrInvalidDelta
	}
	document, err := NewFromClockWithOptions(clockState, options)
	if err != nil {
		return nil, err
	}
	if err := document.UnmarshalBinaryWithLimits(saved.Bytes(), limits); err != nil {
		return nil, err
	}
	return document, nil
}
