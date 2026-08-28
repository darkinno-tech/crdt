// Package attachment replicates bounded references to externally stored
// images, audio, video, and arbitrary data. It intentionally never transfers
// media bytes: an application authorizes object access and verifies the
// declared digest after download.
package attachment

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/im10furry/crdt"
	"github.com/im10furry/crdt/clock"
	frame "github.com/im10furry/crdt/encoding"
	"github.com/im10furry/crdt/lww"
	"github.com/im10furry/crdt/snapshot"
)

var (
	ErrNilRegister      = errors.New("attachment: nil register")
	ErrInvalidReference = errors.New("attachment: invalid reference")
	ErrInvalidKey       = errors.New("attachment: invalid key")
	ErrResourceLimit    = errors.New("attachment: resource limit exceeded")
	ErrInvalidDelta     = errors.New("attachment: invalid delta")
	ErrContentMismatch  = errors.New("attachment: content does not match reference")
)

const (
	// SemanticsVersion identifies the immutable attachment-reference schema
	// inside an LWW-Map replication manifest. It is deliberately separate from
	// the outer frame format and the descriptor's inner encoding version.
	SemanticsVersion uint64 = 1

	descriptorVersion = 1
	maxMediaTypeBytes = 127

	// descriptorMaxOverhead covers the four canonical uvarints plus the fixed
	// media-type and SHA-256 fields. Keeping this bound in sync with
	// marshalReference lets the underlying LWW map reject impossible metadata
	// before it allocates and copies it.
	descriptorMaxOverhead = 4*binary.MaxVarintLen64 + maxMediaTypeBytes + sha256.Size
)

// Reference describes immutable content held by an application-owned object
// store. ObjectID is an opaque identifier, never a signed URL or credential.
// Digest is the SHA-256 of the bytes that an authorized downloader must verify
// before decoding or rendering them.
type Reference struct {
	ObjectID  string
	MediaType string
	Size      uint64
	Digest    [sha256.Size]byte
}

// Verify streams one downloaded object and checks its exact byte length and
// SHA-256 digest. It neither buffers the object nor performs I/O beyond the
// supplied reader, so storage selection and authorization remain application
// concerns. A short, oversized, or differently hashed object returns
// ErrContentMismatch.
func (r Reference) Verify(reader io.Reader) error {
	if reader == nil || r.validateSyntax() != nil {
		return ErrInvalidReference
	}
	hash := sha256.New()
	var total uint64
	var buffer [32 << 10]byte
	for {
		count, err := reader.Read(buffer[:])
		if count < 0 || count > len(buffer) {
			return ErrContentMismatch
		}
		if count > 0 {
			if total > r.Size || uint64(count) > r.Size-total {
				return ErrContentMismatch
			}
			total += uint64(count)
			// hash.Hash.Write for SHA-256 is documented to return a nil error.
			_, _ = hash.Write(buffer[:count])
		}
		switch {
		case err == io.EOF:
			if total != r.Size || !bytes.Equal(hash.Sum(nil), r.Digest[:]) {
				return ErrContentMismatch
			}
			return nil
		case err != nil:
			return fmt.Errorf("attachment: read content: %w", err)
		case count == 0:
			return fmt.Errorf("attachment: read content: %w", io.ErrNoProgress)
		}
	}
}

// Options bounds metadata retained by one Register and its underlying LWW-Map.
// MaxObjectBytes bounds the declared external object size to prevent a
// replicated reference from causing an unbounded fetch or allocation in a
// consumer.
type Options struct {
	MaxEntries       int
	MaxKeyBytes      int
	MaxObjectIDBytes int
	MaxObjectBytes   uint64
}

// DefaultOptions returns conservative limits for collaborative documents. A
// deployment with larger galleries should explicitly choose and enforce a
// matching storage and transport budget.
func DefaultOptions() Options {
	return Options{
		MaxEntries:       16 << 10,
		MaxKeyBytes:      512,
		MaxObjectIDBytes: 1024,
		MaxObjectBytes:   1 << 40,
	}
}

func (o Options) lwwOptions() (lww.MapOptions, bool) {
	if o.MaxEntries <= 0 || o.MaxKeyBytes <= 0 || o.MaxObjectIDBytes <= 0 || o.MaxObjectBytes == 0 {
		return lww.MapOptions{}, false
	}
	maxInt := int(^uint(0) >> 1)
	if o.MaxObjectIDBytes > maxInt-descriptorMaxOverhead {
		return lww.MapOptions{}, false
	}
	return lww.MapOptions{
		MaxEntries:    o.MaxEntries,
		MaxKeyBytes:   o.MaxKeyBytes,
		MaxValueBytes: o.MaxObjectIDBytes + descriptorMaxOverhead,
	}, true
}

// Delta is an opaque, joinable attachment-reference change. Its LWW-Map frame
// remains TypeIDLWWMapDelta, so peers must use the attachment schema ID and
// SemanticsVersion in their authenticated replication manifest.
type Delta struct{ value lww.MapDelta }

// Register resolves concurrent changes to the same key by the canonical HLC
// order used by lww.Map. Deletes retain metadata until the embedding
// application has met the LWW tombstone lifecycle requirements.
type Register struct {
	mu      sync.Mutex
	values  *lww.Map
	options Options
}

var _ crdt.CRDT[*Register] = (*Register)(nil)
var _ crdt.DeltaCapable[*Register, Delta] = (*Register)(nil)

// New creates a register with DefaultOptions.
func New(replicaID string) (*Register, error) { return NewWithOptions(replicaID, DefaultOptions()) }

// NewWithOptions creates a register with explicit metadata and object-size
// limits. It never stores the referenced media bytes.
func NewWithOptions(replicaID string, options Options) (*Register, error) {
	return NewFromClockWithOptions(clock.State{ReplicaID: replicaID}, options)
}

// NewFromClockWithOptions restores a replica clock with explicit limits.
func NewFromClockWithOptions(state clock.State, options Options) (*Register, error) {
	mapOptions, ok := options.lwwOptions()
	if !ok {
		return nil, ErrResourceLimit
	}
	values, err := lww.NewMapFromClockWithOptions(state, mapOptions)
	if err != nil {
		return nil, normalizeMapError(err)
	}
	return &Register{values: values, options: options}, nil
}

// ClockState returns the HLC state that must be saved atomically with a
// complete snapshot before the replica ID is reused.
func (r *Register) ClockState() clock.State {
	if r == nil {
		return clock.State{}
	}
	r.mu.Lock()
	state := r.values.ClockState()
	r.mu.Unlock()
	return state
}

// Put writes ref at key and returns the delta for replication.
func (r *Register) Put(key string, ref Reference) (Delta, error) {
	if r == nil {
		return Delta{}, ErrNilRegister
	}
	if err := validateKey(key, r.options); err != nil {
		return Delta{}, err
	}
	if err := ref.validate(r.options); err != nil {
		return Delta{}, err
	}
	encoded, err := marshalReference(ref, r.options)
	if err != nil {
		return Delta{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.values.HasEntry(key) && r.values.EntryCount() >= r.options.MaxEntries {
		return Delta{}, ErrResourceLimit
	}
	change, err := r.values.SetWithDelta(key, encoded)
	if err != nil {
		return Delta{}, normalizeMapError(err)
	}
	return Delta{value: change}, nil
}

// Delete removes key and returns a tombstone delta. Replaying a delete is
// idempotent and remains permitted at the entry limit.
func (r *Register) Delete(key string) (Delta, error) {
	if r == nil {
		return Delta{}, ErrNilRegister
	}
	if err := validateKey(key, r.options); err != nil {
		return Delta{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.values.HasEntry(key) && r.values.EntryCount() >= r.options.MaxEntries {
		return Delta{}, ErrResourceLimit
	}
	change, err := r.values.DeleteWithDelta(key)
	if err != nil {
		return Delta{}, normalizeMapError(err)
	}
	return Delta{value: change}, nil
}

// Get returns a validated copy of the current reference at key.
func (r *Register) Get(key string) (Reference, bool) {
	if r == nil {
		return Reference{}, false
	}
	r.mu.Lock()
	value, ok := r.values.Get(key)
	options := r.options
	r.mu.Unlock()
	if !ok {
		return Reference{}, false
	}
	ref, err := unmarshalReference(value, options)
	if err != nil {
		return Reference{}, false
	}
	return ref, true
}

// Keys returns visible attachment keys in lexical order.
func (r *Register) Keys() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	keys := r.values.Keys()
	r.mu.Unlock()
	return keys
}

// ApplyDelta validates descriptor schema and resource limits before joining a
// change. Invalid input leaves both the register and its HLC unchanged.
func (r *Register) ApplyDelta(change Delta) error {
	if r == nil {
		return ErrNilRegister
	}
	if err := change.validate(r.options); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wouldExceed(change.value.Keys()) {
		return ErrResourceLimit
	}
	return normalizeMapError(r.values.ApplyDelta(change.value))
}

// Merge joins another validated register without retaining caller-owned data.
func (r *Register) Merge(other *Register) error {
	if r == nil || other == nil {
		return ErrNilRegister
	}
	if r == other {
		return nil
	}
	other.mu.Lock()
	encoded, err := other.values.MarshalBinary()
	other.mu.Unlock()
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	mapOptions, ok := r.options.lwwOptions()
	if !ok {
		return ErrResourceLimit
	}
	incoming, err := lww.NewMapFromClockWithOptions(r.values.ClockState(), mapOptions)
	if err != nil {
		return normalizeMapError(err)
	}
	if err := incoming.UnmarshalBinary(encoded); err != nil {
		return normalizeMapError(err)
	}
	if err := validateMap(incoming, r.options); err != nil {
		return err
	}
	if r.wouldExceed(incoming.EntryKeys()) {
		return ErrResourceLimit
	}
	return normalizeMapError(r.values.Merge(incoming))
}

// State reports attachment metadata counts without exposing references.
func (r *Register) State() crdt.StateSnapshot {
	if r == nil {
		return crdt.StateSnapshot{Type: "attachment-register"}
	}
	r.mu.Lock()
	state := r.values.State()
	r.mu.Unlock()
	state.Type = "attachment-register"
	return state
}

// Frontier returns the greatest retained LWW tag for each replica.
func (r *Register) Frontier() map[string]crdt.Tag {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	frontier := r.values.Frontier()
	r.mu.Unlock()
	return frontier
}

// TombstoneTags returns retained delete tags in canonical order. The tags are
// evidence to report to a tombstonegc.Coordinator, not proof that a caller may
// remove metadata by itself.
func (r *Register) TombstoneTags() []crdt.Tag {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	tags := r.values.TombstoneTags()
	r.mu.Unlock()
	return tags
}

// CompactTombstones removes only requested delete metadata. For replicated
// state, invoke it only after every active member has acknowledged the exact
// tags in one authenticated membership epoch, a post-compaction snapshot is
// durable, and obsolete deltas are retired. tombstonegc.SimpleCollector may
// call it only for its documented local-only lifecycle. Unknown tags are
// ignored; invalid or live tags leave the register unchanged.
func (r *Register) CompactTombstones(tags []crdt.Tag) (int, error) {
	if r == nil {
		return 0, ErrNilRegister
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	removed, err := r.values.CompactTombstones(tags)
	return removed, normalizeMapError(err)
}

func (r *Register) wouldExceed(keys []string) bool {
	newEntries := 0
	for _, key := range keys {
		if !r.values.HasEntry(key) {
			newEntries++
		}
	}
	return newEntries > r.options.MaxEntries-r.values.EntryCount()
}

// MarshalBinary returns the canonical LWW-Map state frame. The frame stores
// only attachment descriptors, never image, video, audio, or data bytes.
func (r *Register) MarshalBinary() ([]byte, error) {
	if r == nil {
		return nil, ErrNilRegister
	}
	r.mu.Lock()
	encoded, err := r.values.MarshalBinary()
	r.mu.Unlock()
	return encoded, err
}

// UnmarshalBinaryWithLimits atomically replaces r from a canonical LWW-Map
// state after validating every descriptor against r's configured limits.
func (r *Register) UnmarshalBinaryWithLimits(data []byte, limits frame.Limits) error {
	if r == nil {
		return ErrNilRegister
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	mapOptions, ok := r.options.lwwOptions()
	if !ok {
		return ErrResourceLimit
	}
	candidate, err := lww.NewMapFromClockWithOptions(r.values.ClockState(), mapOptions)
	if err != nil {
		return normalizeMapError(err)
	}
	if err := candidate.UnmarshalBinaryWithLimits(data, limits); err != nil {
		return normalizeMapError(err)
	}
	if candidate.EntryCount() > r.options.MaxEntries {
		return ErrResourceLimit
	}
	if err := validateMap(candidate, r.options); err != nil {
		return err
	}
	return normalizeMapError(r.values.UnmarshalBinaryWithLimits(data, limits))
}

// UnmarshalBinary uses the library's default outer-frame bounds.
func (r *Register) UnmarshalBinary(data []byte) error {
	return r.UnmarshalBinaryWithLimits(data, frame.DefaultLimits())
}

// MarshalBinary serializes one attachment delta using TypeIDLWWMapDelta.
func (d Delta) MarshalBinary() ([]byte, error) { return d.value.MarshalBinary() }

// Merge joins two attachment deltas without modifying either input.
func (d Delta) Merge(other Delta) (Delta, error) {
	merged, err := d.value.Merge(other.value)
	if err != nil {
		return Delta{}, err
	}
	return Delta{value: merged}, nil
}

// UnmarshalDeltaWithLimits decodes a bounded LWW-Map delta and validates the
// immutable attachment descriptor schema before returning it to a caller.
func UnmarshalDeltaWithLimits(data []byte, limits frame.Limits, options Options) (Delta, error) {
	mapOptions, ok := options.lwwOptions()
	if !ok {
		return Delta{}, ErrResourceLimit
	}
	change, err := lww.UnmarshalMapDeltaWithOptions(data, limits, mapOptions)
	if err != nil {
		return Delta{}, normalizeMapError(err)
	}
	delta := Delta{value: change}
	if err := delta.validate(options); err != nil {
		return Delta{}, err
	}
	return delta, nil
}

// UnmarshalDelta uses DefaultOptions and the library's default frame limits.
func UnmarshalDelta(data []byte) (Delta, error) {
	return UnmarshalDeltaWithLimits(data, frame.DefaultLimits(), DefaultOptions())
}

// Snapshot delegates to the underlying LWW-Map, preserving its HLC state.
func (r *Register) Snapshot(frontier map[string]crdt.Tag) (snapshot.Snapshot, error) {
	if r == nil {
		return snapshot.Snapshot{}, ErrNilRegister
	}
	r.mu.Lock()
	saved, err := r.values.Snapshot(frontier)
	r.mu.Unlock()
	return saved, err
}

// SnapshotCurrentState captures complete attachment metadata, frontier, and
// HLC state for atomic same-replica recovery.
func (r *Register) SnapshotCurrentState() (snapshot.Snapshot, error) {
	if r == nil {
		return snapshot.Snapshot{}, ErrNilRegister
	}
	r.mu.Lock()
	saved, err := r.values.SnapshotCurrentState()
	r.mu.Unlock()
	return saved, err
}

// NewFromSnapshotWithOptions restores a complete attachment register. The
// caller must use the same limits used by its replication group.
func NewFromSnapshotWithOptions(saved snapshot.Snapshot, options Options) (*Register, error) {
	mapOptions, ok := options.lwwOptions()
	if !ok {
		return nil, ErrResourceLimit
	}
	values, err := lww.NewMapFromSnapshotWithOptions(saved, mapOptions)
	if err != nil {
		return nil, normalizeMapError(err)
	}
	if values.EntryCount() > options.MaxEntries {
		return nil, ErrResourceLimit
	}
	if err := validateMap(values, options); err != nil {
		return nil, err
	}
	return &Register{values: values, options: options}, nil
}

// NewFromSnapshot restores using DefaultOptions.
func NewFromSnapshot(saved snapshot.Snapshot) (*Register, error) {
	return NewFromSnapshotWithOptions(saved, DefaultOptions())
}

func (d Delta) validate(options Options) error {
	if len(d.value.Keys()) > options.MaxEntries {
		return ErrResourceLimit
	}
	return d.value.ValidateValues(func(key string, value []byte) error {
		if err := validateKey(key, options); err != nil {
			return err
		}
		_, err := unmarshalReference(value, options)
		return err
	})
}

func validateMap(values *lww.Map, options Options) error {
	for _, key := range values.EntryKeys() {
		if err := validateKey(key, options); err != nil {
			return err
		}
	}
	return values.ValidateValues(func(key string, value []byte) error {
		_, err := unmarshalReference(value, options)
		return err
	})
}

func normalizeMapError(err error) error {
	if errors.Is(err, lww.ErrResourceLimit) {
		return ErrResourceLimit
	}
	return err
}

func validateKey(key string, options Options) error {
	if key == "" || strings.TrimSpace(key) != key || !utf8.ValidString(key) || len(key) > options.MaxKeyBytes {
		return ErrInvalidKey
	}
	for _, value := range key {
		if unicode.IsControl(value) {
			return ErrInvalidKey
		}
	}
	return nil
}

func (r Reference) validate(options Options) error {
	if err := r.validateSyntax(); err != nil {
		return err
	}
	if len(r.ObjectID) > options.MaxObjectIDBytes || r.Size > options.MaxObjectBytes {
		return ErrInvalidReference
	}
	return nil
}

func (r Reference) validateSyntax() error {
	if r.ObjectID == "" || strings.TrimSpace(r.ObjectID) != r.ObjectID || !utf8.ValidString(r.ObjectID) {
		return ErrInvalidReference
	}
	for _, value := range r.ObjectID {
		if unicode.IsControl(value) {
			return ErrInvalidReference
		}
	}
	mediaType, params, err := mime.ParseMediaType(r.MediaType)
	if err != nil || len(params) != 0 || mediaType != r.MediaType || len(r.MediaType) == 0 || len(r.MediaType) > maxMediaTypeBytes {
		return ErrInvalidReference
	}
	var empty [sha256.Size]byte
	if r.Digest == empty {
		return ErrInvalidReference
	}
	return nil
}

func marshalReference(ref Reference, options Options) ([]byte, error) {
	if err := ref.validate(options); err != nil {
		return nil, err
	}
	encoded := frame.AppendUvarint(nil, descriptorVersion)
	encoded = appendBytes(encoded, ref.ObjectID)
	encoded = appendBytes(encoded, ref.MediaType)
	encoded = frame.AppendUvarint(encoded, ref.Size)
	encoded = append(encoded, ref.Digest[:]...)
	return encoded, nil
}

func unmarshalReference(data []byte, options Options) (Reference, error) {
	position := 0
	version, next, ok := frame.ReadUvarint(data, position)
	if !ok || version != descriptorVersion {
		return Reference{}, ErrInvalidReference
	}
	position = next
	objectID, next, ok := frame.ReadBytes(data, position, options.MaxObjectIDBytes)
	if !ok {
		return Reference{}, ErrInvalidReference
	}
	position = next
	mediaType, next, ok := frame.ReadBytes(data, position, maxMediaTypeBytes)
	if !ok {
		return Reference{}, ErrInvalidReference
	}
	position = next
	size, next, ok := frame.ReadUvarint(data, position)
	if !ok || len(data)-next != sha256.Size {
		return Reference{}, ErrInvalidReference
	}
	ref := Reference{ObjectID: string(objectID), MediaType: string(mediaType), Size: size}
	copy(ref.Digest[:], data[next:])
	if err := ref.validate(options); err != nil {
		return Reference{}, err
	}
	return ref, nil
}

func appendBytes(dst []byte, value string) []byte {
	dst = frame.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}
