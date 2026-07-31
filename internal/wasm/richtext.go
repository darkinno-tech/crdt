package wasm

import (
	"math"
	"sync"
	"unicode/utf8"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/richtext"
	"github.com/DarkInno/crdt/snapshot"
	"github.com/DarkInno/crdt/text"
)

const (
	// RichTextStateTypeID and RichTextDeltaTypeID identify the only rich-text
	// v1 frames accepted by the browser runtime.
	RichTextStateTypeID uint64 = crdt.TypeIDRichTextState
	RichTextDeltaTypeID uint64 = crdt.TypeIDRichTextDelta

	// RichTextSemanticsVersion must be bound to the rich-text renderer schema
	// in the authenticated manifest. It is not an RGA run-v2 protocol.
	RichTextSemanticsVersion uint64 = richtext.SemanticsVersion
)

// RichTextProtocol identifies the one rich-text contract exported to a
// browser/WebView. Attribute schemas remain manifest-selected application
// policy; this value identifies only the CRDT wire and merge semantics.
type RichTextProtocol struct {
	StateTypeID      uint64
	DeltaTypeID      uint64
	SemanticsVersion uint64
}

func (p RichTextProtocol) valid() bool {
	return p == (RichTextProtocol{
		StateTypeID: RichTextStateTypeID, DeltaTypeID: RichTextDeltaTypeID, SemanticsVersion: RichTextSemanticsVersion,
	})
}

// RichTextOptions bounds untrusted rich-text frames and one local editor
// transaction. They are intentionally browser-sized rather than inheriting
// the process-wide library defaults.
type RichTextOptions struct {
	Document          richtext.Options
	Decoder           frame.DecoderLimits
	MaxLocalEditRunes int
	MaxLocalEditBytes int
	MaxLocalEditorOps int
}

// DefaultRichTextOptions returns the rich-text v1 browser/WebView limits.
func DefaultRichTextOptions() RichTextOptions {
	const maxFrameBytes = 1 << 20
	return RichTextOptions{
		Document: richtext.Options{
			Text: text.Options{
				MaxNodes:        100_000,
				MaxTombstones:   100_000,
				MaxPendingNodes: 10_000,
				MaxPendingBytes: 512 << 10,
			},
			// Text retention is 100,000 nodes. Keep mark retention smaller
			// so a remote peer cannot turn every character into an unbounded map.
			MaxMarkEntries:            200_000,
			MaxAttributesPerOperation: 32,
		},
		Decoder: frame.DecoderLimits{
			MaxFrameBytes:  maxFrameBytes,
			MaxPayload:     maxFrameBytes - 4096,
			MaxCodecID:     256,
			MaxElements:    100_000,
			MaxTags:        100_000,
			MaxStringBytes: 64 << 10,
		},
		MaxLocalEditRunes: 16 << 10,
		MaxLocalEditBytes: 64 << 10,
		MaxLocalEditorOps: 512,
	}
}

func (o RichTextOptions) valid() bool {
	return o.Document.Text.MaxNodes > 0 && o.Document.Text.MaxTombstones > 0 &&
		o.Document.Text.MaxPendingNodes > 0 && o.Document.Text.MaxPendingBytes > 0 &&
		o.Document.MaxMarkEntries > 0 && o.Document.MaxAttributesPerOperation > 0 &&
		o.Decoder.MaxFrameBytes > 0 && o.Decoder.MaxPayload > 0 && o.Decoder.MaxPayload <= o.Decoder.MaxFrameBytes &&
		o.Decoder.MaxCodecID > 0 && o.Decoder.MaxElements > 0 && o.Decoder.MaxTags > 0 && o.Decoder.MaxStringBytes > 0 &&
		o.MaxLocalEditRunes > 0 && o.MaxLocalEditRunes <= o.Document.Text.MaxNodes &&
		o.MaxLocalEditBytes > 0 && o.MaxLocalEditBytes <= o.Decoder.MaxFrameBytes && o.MaxLocalEditorOps > 0
}

// RichTextSnapshot is the complete persistence unit for one browser document.
// State, frontier, and HLC clock must be written atomically before restoring a
// replica ID. It deliberately has the same data contract as RGASnapshot but
// can only contain rich-text v1 frames.
type RichTextSnapshot struct {
	State    []byte
	Frontier map[string]crdt.Tag
	Clock    clock.State
}

// RichTextRuntime owns bounded browser rich-text document handles.
type RichTextRuntime struct {
	options RichTextOptions
	next    uint64
	docs    map[uint64]*richtext.Document
	mu      sync.RWMutex
}

// NewRichTextRuntime constructs one bounded rich-text v1 browser runtime.
func NewRichTextRuntime(options RichTextOptions) (*RichTextRuntime, error) {
	if !options.valid() {
		return nil, ErrInvalidOptions
	}
	return &RichTextRuntime{options: options, docs: make(map[uint64]*richtext.Document)}, nil
}

// Protocol returns the exact rich-text v1 frame contract emitted and accepted.
func (r *RichTextRuntime) Protocol() RichTextProtocol {
	if r == nil {
		return RichTextProtocol{}
	}
	return RichTextProtocol{StateTypeID: RichTextStateTypeID, DeltaTypeID: RichTextDeltaTypeID, SemanticsVersion: RichTextSemanticsVersion}
}

// MaxFrameBytes reports the largest rich-text frame accepted by this runtime.
func (r *RichTextRuntime) MaxFrameBytes() int {
	if r == nil {
		return 0
	}
	return r.options.Decoder.MaxFrameBytes
}

// MaxTags reports the largest retained tag set accepted from one frame.
func (r *RichTextRuntime) MaxTags() int {
	if r == nil {
		return 0
	}
	return r.options.Decoder.MaxTags
}

// MaxStringBytes reports the maximum attribute, replica, and editor string size.
func (r *RichTextRuntime) MaxStringBytes() int {
	if r == nil {
		return 0
	}
	return r.options.Decoder.MaxStringBytes
}

// MaxLocalEditBytes reports the combined inserted bytes accepted per editor transaction.
func (r *RichTextRuntime) MaxLocalEditBytes() int {
	if r == nil {
		return 0
	}
	return r.options.MaxLocalEditBytes
}

// MaxLocalEditRunes reports the combined inserted runes accepted per editor transaction.
func (r *RichTextRuntime) MaxLocalEditRunes() int {
	if r == nil {
		return 0
	}
	return r.options.MaxLocalEditRunes
}

// MaxLocalEditorOps reports the accepted operation count for one local transaction.
func (r *RichTextRuntime) MaxLocalEditorOps() int {
	if r == nil {
		return 0
	}
	return r.options.MaxLocalEditorOps
}

// MaxAttributesPerOperation reports the configured retained-format fan-out.
func (r *RichTextRuntime) MaxAttributesPerOperation() int {
	if r == nil {
		return 0
	}
	return r.options.Document.MaxAttributesPerOperation
}

// Create allocates one empty rich-text document for replicaID.
func (r *RichTextRuntime) Create(replicaID string) (uint64, error) {
	if r == nil {
		return 0, ErrUnknownDocument
	}
	if len(replicaID) > r.options.Decoder.MaxStringBytes {
		return 0, frame.ErrFrameLimit
	}
	document, err := richtext.NewWithOptions(replicaID, r.options.Document)
	if err != nil {
		return 0, err
	}
	return r.install(document)
}

// Restore validates and installs one atomic rich-text persistence unit.
func (r *RichTextRuntime) Restore(saved RichTextSnapshot) (uint64, error) {
	if r == nil {
		return 0, ErrUnknownDocument
	}
	if err := r.validateSnapshotBounds(saved); err != nil {
		return 0, err
	}
	decoded, err := frame.UnmarshalFrame(saved.State, r.options.Decoder)
	if err != nil || decoded.TypeID != RichTextStateTypeID {
		return 0, frame.ErrInvalidFrame
	}
	validated, err := snapshot.NewWithClockState(saved.State, saved.Frontier, saved.Clock)
	if err != nil {
		return 0, err
	}
	document, err := richtext.NewFromSnapshotWithOptions(validated, r.options.Document, r.options.Decoder)
	if err != nil {
		return 0, err
	}
	return r.install(document)
}

func (r *RichTextRuntime) install(document *richtext.Document) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next == math.MaxUint64 {
		return 0, ErrHandleExhausted
	}
	r.next++
	r.docs[r.next] = document
	return r.next, nil
}

// Drop releases one document handle.
func (r *RichTextRuntime) Drop(handle uint64) bool {
	if r == nil || handle == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.docs[handle]; !exists {
		return false
	}
	delete(r.docs, handle)
	return true
}

// ApplyEditorDelta turns one local rich-editor transaction into exactly one
// canonical rich-text frame. The core document preflights complete text and
// formatting state before it applies anything.
func (r *RichTextRuntime) ApplyEditorDelta(handle uint64, operations []richtext.EditorOperation) ([]byte, error) {
	document, err := r.document(handle)
	if err != nil {
		return nil, err
	}
	if len(operations) > r.options.MaxLocalEditorOps || !localEditorOperationsWithin(operations, r.options) {
		return nil, frame.ErrFrameLimit
	}
	delta, err := document.ApplyEditorDeltaWithLimits(operations, r.options.Decoder)
	if err != nil {
		return nil, err
	}
	return delta.MarshalBinaryWithLimits(r.options.Decoder)
}

// ApplyDelta validates then joins one untrusted rich-text v1 frame.
func (r *RichTextRuntime) ApplyDelta(handle uint64, encoded []byte) error {
	document, err := r.document(handle)
	if err != nil {
		return err
	}
	delta, err := richtext.UnmarshalDeltaWithLimits(encoded, r.options.Decoder)
	if err != nil {
		return err
	}
	return document.ApplyDeltaWithLimits(delta, r.options.Decoder)
}

// Spans returns a caller-owned presentation projection. It does not authorize
// attribute values; renderers must still enforce their manifest schema.
func (r *RichTextRuntime) Spans(handle uint64) ([]richtext.Span, error) {
	document, err := r.document(handle)
	if err != nil {
		return nil, err
	}
	return document.Spans(), nil
}

// Snapshot returns a complete, bounded rich-text persistence unit.
func (r *RichTextRuntime) Snapshot(handle uint64) (RichTextSnapshot, error) {
	document, err := r.document(handle)
	if err != nil {
		return RichTextSnapshot{}, err
	}
	saved, err := document.SnapshotCurrentStateWithLimits(r.options.Decoder)
	if err != nil {
		return RichTextSnapshot{}, err
	}
	clockState, ok := saved.ClockState()
	if !ok {
		return RichTextSnapshot{}, richtext.ErrInvalidDelta
	}
	return RichTextSnapshot{State: saved.Bytes(), Frontier: saved.Frontier(), Clock: clockState}, nil
}

func (r *RichTextRuntime) document(handle uint64) (*richtext.Document, error) {
	if r == nil || handle == 0 {
		return nil, ErrUnknownDocument
	}
	r.mu.RLock()
	document := r.docs[handle]
	r.mu.RUnlock()
	if document == nil {
		return nil, ErrUnknownDocument
	}
	return document, nil
}

func (r *RichTextRuntime) validateSnapshotBounds(saved RichTextSnapshot) error {
	if len(saved.State) > r.options.Decoder.MaxFrameBytes || len(saved.Frontier) > r.options.Decoder.MaxTags ||
		len(saved.Clock.ReplicaID) > r.options.Decoder.MaxStringBytes {
		return frame.ErrFrameLimit
	}
	for replicaID, tag := range saved.Frontier {
		if len(replicaID) > r.options.Decoder.MaxStringBytes || len(tag.ReplicaID) > r.options.Decoder.MaxStringBytes {
			return frame.ErrFrameLimit
		}
	}
	return nil
}

func localEditorOperationsWithin(operations []richtext.EditorOperation, options RichTextOptions) bool {
	bytes, runes := 0, 0
	for _, operation := range operations {
		if len(operation.Insert) > options.MaxLocalEditBytes-bytes {
			return false
		}
		bytes += len(operation.Insert)
		runes += utf8.RuneCountInString(operation.Insert)
		if runes > options.MaxLocalEditRunes {
			return false
		}
	}
	return true
}
