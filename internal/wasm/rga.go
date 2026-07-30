// Package wasm contains host-neutral state used by the browser-facing Wasm
// command. Keeping it separate from syscall/js lets the protocol boundary run
// under the normal Go test and race-detector toolchains.
package wasm

import (
	"errors"
	"math"
	"sync"
	"unicode/utf8"

	"github.com/DarkInno/crdt"
	"github.com/DarkInno/crdt/clock"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
	"github.com/DarkInno/crdt/text"
)

const (
	// RGAStateTypeID and RGADeltaTypeID are the only framed payloads accepted
	// by a scalar-v1 runtime. RGA remains an explicitly negotiated protocol;
	// accepting a frame here never authenticates its sender.
	RGAStateTypeID uint64 = crdt.TypeIDRGAState
	RGADeltaTypeID uint64 = crdt.TypeIDRGADelta

	// RGASemanticsVersion identifies scalar RGA v1 semantics.
	RGASemanticsVersion uint64 = 1

	// RGARunStateTypeID and RGARunDeltaTypeID identify the compact run-v2
	// protocol used by new Go RGA replication groups.
	RGARunStateTypeID uint64 = crdt.TypeIDRGARunState
	RGARunDeltaTypeID uint64 = crdt.TypeIDRGARunDelta

	// RGARunSemanticsVersion identifies compact RGA run-v2 semantics.
	RGARunSemanticsVersion uint64 = 2
)

var (
	ErrInvalidOptions  = errors.New("wasm: invalid RGA runtime options")
	ErrUnknownDocument = errors.New("wasm: unknown RGA document")
	ErrHandleExhausted = errors.New("wasm: document handle space exhausted")
)

// RGAWireFormat selects exactly one separately negotiated RGA frame contract.
// A runtime never accepts both formats because their compatibility is a
// manifest-level decision, not a best-effort decoder choice.
type RGAWireFormat uint8

const (
	RGAWireFormatV1 RGAWireFormat = iota + 1
	RGAWireFormatRunV2
)

// RGAProtocol identifies the framed RGA semantics exported by one runtime.
type RGAProtocol struct {
	StateTypeID      uint64
	DeltaTypeID      uint64
	SemanticsVersion uint64
}

func (p RGAProtocol) valid() bool {
	return p == (RGAProtocol{StateTypeID: RGAStateTypeID, DeltaTypeID: RGADeltaTypeID, SemanticsVersion: RGASemanticsVersion}) ||
		p == (RGAProtocol{StateTypeID: RGARunStateTypeID, DeltaTypeID: RGARunDeltaTypeID, SemanticsVersion: RGARunSemanticsVersion})
}

// RGAOptions bounds both externally received frames and retained document
// state. These are deliberately smaller than the library defaults because a
// browser tab or WebView is commonly exposed to untrusted network peers.
type RGAOptions struct {
	Text              text.Options
	Decoder           frame.DecoderLimits
	MaxLocalEditRunes int
	MaxLocalEditBytes int
	WireFormat        RGAWireFormat
}

// DefaultRGAOptions returns the legacy scalar-v1 browser/WebView runtime
// limits. It is retained for an explicitly negotiated migration group.
func DefaultRGAOptions() RGAOptions {
	const maxFrameBytes = 1 << 20
	return RGAOptions{
		Text: text.Options{
			MaxNodes:        100_000,
			MaxTombstones:   100_000,
			MaxPendingNodes: 10_000,
			MaxPendingBytes: 512 << 10,
		},
		Decoder: frame.DecoderLimits{
			MaxFrameBytes: maxFrameBytes,
			// Leave ample room for the outer envelope, whose maximum size is
			// intentionally not duplicated from encoding's private constants.
			MaxPayload:     maxFrameBytes - 4096,
			MaxCodecID:     256,
			MaxElements:    100_000,
			MaxTags:        100_000,
			MaxStringBytes: 64 << 10,
		},
		// A 16K-rune edit is below the 1 MiB scalar-v1 frame budget in the
		// measured Unicode workload. Larger editor transactions must be split
		// before the Go RGA constructs per-rune nodes.
		MaxLocalEditRunes: 16 << 10,
		MaxLocalEditBytes: 64 << 10,
		WireFormat:        RGAWireFormatV1,
	}
}

// DefaultRunRGAOptions returns the browser/WebView limits for the compact
// run-v2 protocol used by new Go RGA replication groups. Applications still
// need transport-level request limits before a Uint8Array is allocated.
func DefaultRunRGAOptions() RGAOptions {
	options := DefaultRGAOptions()
	options.WireFormat = RGAWireFormatRunV2
	return options
}

func (o RGAOptions) protocol() RGAProtocol {
	switch o.WireFormat {
	case RGAWireFormatV1:
		return RGAProtocol{StateTypeID: RGAStateTypeID, DeltaTypeID: RGADeltaTypeID, SemanticsVersion: RGASemanticsVersion}
	case RGAWireFormatRunV2:
		return RGAProtocol{StateTypeID: RGARunStateTypeID, DeltaTypeID: RGARunDeltaTypeID, SemanticsVersion: RGARunSemanticsVersion}
	default:
		return RGAProtocol{}
	}
}

func (o RGAOptions) valid() bool {
	return o.Text.MaxNodes > 0 && o.Text.MaxTombstones > 0 &&
		o.Text.MaxPendingNodes > 0 && o.Text.MaxPendingBytes > 0 &&
		o.Decoder.MaxFrameBytes > 0 && o.Decoder.MaxPayload > 0 &&
		o.Decoder.MaxCodecID > 0 && o.Decoder.MaxElements > 0 &&
		o.Decoder.MaxTags > 0 && o.Decoder.MaxStringBytes > 0 &&
		o.Decoder.MaxPayload <= o.Decoder.MaxFrameBytes &&
		o.Decoder.MaxCodecID <= o.Decoder.MaxFrameBytes &&
		o.MaxLocalEditRunes > 0 && o.MaxLocalEditRunes <= o.Text.MaxNodes &&
		o.MaxLocalEditBytes > 0 && o.MaxLocalEditBytes <= o.Decoder.MaxFrameBytes &&
		o.protocol().valid()
}

// RGASnapshot is the complete browser persistence unit. State, Frontier, and
// Clock must be persisted atomically; storing only State can reuse a mutation
// tag when a replica ID is restored after a restart.
type RGASnapshot struct {
	State    []byte
	Frontier map[string]crdt.Tag
	Clock    clock.State
}

// Runtime owns browser-document handles. Individual RGA values remain
// independently concurrency-safe, while the registry itself is protected so
// native tests can exercise it with the race detector.
type Runtime struct {
	mu      sync.RWMutex
	options RGAOptions
	next    uint64
	docs    map[uint64]*text.RGA
}

// NewRuntime creates a bounded RGA client runtime.
func NewRuntime(options RGAOptions) (*Runtime, error) {
	if !options.valid() {
		return nil, ErrInvalidOptions
	}
	return &Runtime{options: options, docs: make(map[uint64]*text.RGA)}, nil
}

// Protocol returns the one RGA frame contract accepted and emitted by this
// runtime. The application must bind this value to its authenticated manifest.
func (r *Runtime) Protocol() RGAProtocol {
	if r == nil {
		return RGAProtocol{}
	}
	return r.options.protocol()
}

// Create allocates an RGA document owned by replicaID and returns an opaque
// non-zero handle.
func (r *Runtime) Create(replicaID string) (uint64, error) {
	if r == nil {
		return 0, ErrUnknownDocument
	}
	if len(replicaID) > r.options.Decoder.MaxStringBytes {
		return 0, frame.ErrFrameLimit
	}
	document, err := text.NewWithOptions(replicaID, r.options.Text)
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next == math.MaxUint64 {
		return 0, ErrHandleExhausted
	}
	r.next++
	r.docs[r.next] = document
	return r.next, nil
}

// Restore validates one atomic persistence unit under the runtime limits,
// restores its clock/frontier, and returns a new opaque document handle.
func (r *Runtime) Restore(saved RGASnapshot) (uint64, error) {
	if r == nil {
		return 0, ErrUnknownDocument
	}
	if err := r.validateSnapshotBounds(saved); err != nil {
		return 0, err
	}
	decoded, err := frame.UnmarshalFrame(saved.State, r.options.Decoder)
	if err != nil {
		return 0, err
	}
	if decoded.TypeID != r.Protocol().StateTypeID {
		return 0, frame.ErrInvalidFrame
	}
	validated, err := snapshot.NewWithClockState(saved.State, saved.Frontier, saved.Clock)
	if err != nil {
		return 0, err
	}
	document, err := text.NewFromSnapshotWithOptions(validated, r.options.Text, r.options.Decoder)
	if err != nil {
		return 0, err
	}
	return r.install(document)
}

func (r *Runtime) install(document *text.RGA) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next == math.MaxUint64 {
		return 0, ErrHandleExhausted
	}
	r.next++
	r.docs[r.next] = document
	return r.next, nil
}

// Drop releases one document handle. It returns whether a document existed.
func (r *Runtime) Drop(handle uint64) bool {
	if r == nil || handle == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.docs[handle]; !ok {
		return false
	}
	delete(r.docs, handle)
	return true
}

// Insert performs one local rune-offset insertion and returns a canonical
// delta frame in the runtime's explicitly selected RGA wire format.
func (r *Runtime) Insert(handle uint64, offset int, value string) ([]byte, error) {
	document, err := r.document(handle)
	if err != nil {
		return nil, err
	}
	if len(value) > r.options.MaxLocalEditBytes || utf8.RuneCountInString(value) > r.options.MaxLocalEditRunes {
		return nil, frame.ErrFrameLimit
	}
	switch r.options.WireFormat {
	case RGAWireFormatV1:
		return document.InsertBinaryWithLimits(offset, value, r.options.Decoder)
	case RGAWireFormatRunV2:
		return document.InsertRunBinaryWithLimits(offset, value, r.options.Decoder)
	default:
		return nil, ErrInvalidOptions
	}
}

// Delete performs one local rune-offset deletion and returns a canonical
// tombstone frame in the runtime's explicitly selected RGA wire format.
func (r *Runtime) Delete(handle uint64, offset, count int) ([]byte, error) {
	document, err := r.document(handle)
	if err != nil {
		return nil, err
	}
	switch r.options.WireFormat {
	case RGAWireFormatV1:
		return document.DeleteBinaryWithLimits(offset, count, r.options.Decoder)
	case RGAWireFormatRunV2:
		return document.DeleteRunBinaryWithLimits(offset, count, r.options.Decoder)
	default:
		return nil, ErrInvalidOptions
	}
}

// Replace performs one atomic local editor replacement and returns one framed
// delta. It preflights the combined insert/tombstone delta before committing so
// a rejected editor transaction cannot leave a half-applied local change.
func (r *Runtime) Replace(handle uint64, offset, count int, value string) ([]byte, error) {
	document, err := r.document(handle)
	if err != nil {
		return nil, err
	}
	if len(value) > r.options.MaxLocalEditBytes || utf8.RuneCountInString(value) > r.options.MaxLocalEditRunes {
		return nil, frame.ErrFrameLimit
	}
	switch r.options.WireFormat {
	case RGAWireFormatV1:
		return document.ReplaceBinaryWithLimits(offset, count, value, r.options.Decoder)
	case RGAWireFormatRunV2:
		return document.ReplaceRunBinaryWithLimits(offset, count, value, r.options.Decoder)
	default:
		return nil, ErrInvalidOptions
	}
}

// ApplyDelta validates and joins one untrusted canonical delta frame for the
// runtime's selected RGA format. A malformed, mismatched, or over-limit frame
// leaves the document unchanged.
func (r *Runtime) ApplyDelta(handle uint64, encoded []byte) error {
	document, err := r.document(handle)
	if err != nil {
		return err
	}
	var delta text.Delta
	switch r.options.WireFormat {
	case RGAWireFormatV1:
		delta, err = text.UnmarshalRGADeltaWithLimits(encoded, r.options.Decoder)
	case RGAWireFormatRunV2:
		delta, err = text.UnmarshalRGARunDeltaWithLimits(encoded, r.options.Decoder)
	default:
		return ErrInvalidOptions
	}
	if err != nil {
		return err
	}
	return document.ApplyDelta(delta)
}

// Text returns the current visible document projection.
func (r *Runtime) Text(handle uint64) (string, error) {
	document, err := r.document(handle)
	if err != nil {
		return "", err
	}
	return document.String(), nil
}

// PendingCount reports accepted out-of-order nodes waiting for a parent.
func (r *Runtime) PendingCount(handle uint64) (int, error) {
	document, err := r.document(handle)
	if err != nil {
		return 0, err
	}
	return document.PendingCount(), nil
}

// MaxFrameBytes reports the largest encoded frame accepted by this runtime.
// The JS boundary checks this before copying an incoming Uint8Array into Go.
func (r *Runtime) MaxFrameBytes() int {
	if r == nil {
		return 0
	}
	return r.options.Decoder.MaxFrameBytes
}

// MaxTags reports the largest retained tag set accepted from one frame. The
// JS snapshot parser uses this to bound its own object traversal before Go
// receives a reconstructed persistence unit.
func (r *Runtime) MaxTags() int {
	if r == nil {
		return 0
	}
	return r.options.Decoder.MaxTags
}

// MaxStringBytes reports the UTF-8 byte budget for a replica ID in a frame or
// persistence unit. The JS boundary uses it before copying untrusted snapshot
// metadata into Go.
func (r *Runtime) MaxStringBytes() int {
	if r == nil {
		return 0
	}
	return r.options.Decoder.MaxStringBytes
}

// MaxLocalEditBytes reports the UTF-8 input budget for one local insertion.
// Editors should split larger transactions before calling Insert.
func (r *Runtime) MaxLocalEditBytes() int {
	if r == nil {
		return 0
	}
	return r.options.MaxLocalEditBytes
}

// MaxLocalEditRunes reports the visible-rune budget for one local insertion.
func (r *Runtime) MaxLocalEditRunes() int {
	if r == nil {
		return 0
	}
	return r.options.MaxLocalEditRunes
}

// Snapshot returns a cloned, complete persistence unit under the runtime's
// output limits. Incomplete out-of-order state is deliberately not persisted.
func (r *Runtime) Snapshot(handle uint64) (RGASnapshot, error) {
	document, err := r.document(handle)
	if err != nil {
		return RGASnapshot{}, err
	}
	var saved snapshot.Snapshot
	switch r.options.WireFormat {
	case RGAWireFormatV1:
		saved, err = document.SnapshotCurrentStateWithLimits(r.options.Decoder)
	case RGAWireFormatRunV2:
		saved, err = document.SnapshotRunCurrentStateWithLimits(r.options.Decoder)
	default:
		return RGASnapshot{}, ErrInvalidOptions
	}
	if err != nil {
		return RGASnapshot{}, err
	}
	clockState, ok := saved.ClockState()
	if !ok {
		return RGASnapshot{}, text.ErrInvalidDelta
	}
	return RGASnapshot{State: saved.Bytes(), Frontier: saved.Frontier(), Clock: clockState}, nil
}

func (r *Runtime) document(handle uint64) (*text.RGA, error) {
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

func (r *Runtime) validateSnapshotBounds(saved RGASnapshot) error {
	if len(saved.State) > r.options.Decoder.MaxFrameBytes ||
		len(saved.Frontier) > r.options.Decoder.MaxTags ||
		len(saved.Clock.ReplicaID) > r.options.Decoder.MaxStringBytes {
		return frame.ErrFrameLimit
	}
	for replicaID, tag := range saved.Frontier {
		if len(replicaID) > r.options.Decoder.MaxStringBytes ||
			len(tag.ReplicaID) > r.options.Decoder.MaxStringBytes {
			return frame.ErrFrameLimit
		}
	}
	return nil
}
