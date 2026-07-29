package set

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
	"github.com/DarkInno/crdt/snapshot"
)

var (
	ErrNilGSet         = errors.New("set: nil G-Set")
	ErrInvalidGSet     = errors.New("set: invalid G-Set state")
	ErrInvalidGSetSnap = errors.New("set: invalid G-Set snapshot")
)

// GSet is a grow-only, state-based set. It is useful when membership never
// needs to be revoked: Add and Merge are set unions, so both are idempotent.
// Elements are identified on the wire by the canonical bytes from codec.
type GSet[T comparable] struct {
	mu        sync.RWMutex
	replicaID string
	codec     boundElementCodec[T]
	elements  map[T]struct{}
}

// GSetDelta is a joinable partial G-Set state.
type GSetDelta[T comparable] struct{ elements map[T]struct{} }

var _ crdt.CRDT[*GSet[string]] = (*GSet[string])(nil)
var _ crdt.DeltaCapable[*GSet[string], GSetDelta[string]] = (*GSet[string])(nil)

// NewGSet creates a G-Set whose framed state uses codec's stable identifier.
// replicaID is diagnostic metadata and must be unique when the caller uses it
// to identify the logical owner of the set.
func NewGSet[T comparable](replicaID string, codec ElementCodec[T]) (*GSet[T], error) {
	if !(crdt.Tag{ReplicaID: replicaID}).Valid() {
		return nil, ErrInvalidReplicaID
	}
	bound, err := bindElementCodec(codec)
	if err != nil {
		return nil, err
	}
	return &GSet[T]{replicaID: replicaID, codec: bound, elements: make(map[T]struct{})}, nil
}

// NewGSetFromSnapshot restores a G-Set from a validated framed snapshot.
func NewGSetFromSnapshot[T comparable](replicaID string, saved snapshot.Snapshot, codec ElementCodec[T]) (*GSet[T], error) {
	if saved.TypeID != crdt.TypeIDGSetState {
		return nil, ErrInvalidGSetSnap
	}
	value, err := NewGSet(replicaID, codec)
	if err != nil {
		return nil, err
	}
	if err := value.UnmarshalBinary(saved.Bytes()); err != nil {
		return nil, err
	}
	return value, nil
}

// Add inserts element and returns the one-element delta. Re-adding an element
// is valid and returns an equivalent idempotent delta.
func (s *GSet[T]) Add(element T) (GSetDelta[T], error) {
	if s == nil {
		return GSetDelta[T]{}, ErrNilGSet
	}
	s.mu.Lock()
	s.elements[element] = struct{}{}
	s.mu.Unlock()
	return GSetDelta[T]{elements: map[T]struct{}{element: {}}}, nil
}

// Contains reports whether element belongs to s.
func (s *GSet[T]) Contains(element T) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	_, ok := s.elements[element]
	s.mu.RUnlock()
	return ok
}

// Elements returns a copy of the set. Its order is unspecified; framed output
// sorts canonical element bytes independently of Go map iteration.
func (s *GSet[T]) Elements() []T {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	result := make([]T, 0, len(s.elements))
	for element := range s.elements {
		result = append(result, element)
	}
	s.mu.RUnlock()
	return result
}

// Merge joins other into s without retaining either caller-owned state.
func (s *GSet[T]) Merge(other *GSet[T]) error {
	if s == nil || other == nil {
		return ErrNilGSet
	}
	if s.codec.id != other.codec.id {
		return ErrCodecMismatch
	}
	if s == other {
		return nil
	}
	other.mu.RLock()
	elements := cloneGSetElements(other.elements)
	other.mu.RUnlock()
	return s.join(elements)
}

// ApplyDelta joins delta into s.
func (s *GSet[T]) ApplyDelta(delta GSetDelta[T]) error {
	if s == nil {
		return ErrNilGSet
	}
	if delta.elements == nil {
		return ErrInvalidGSet
	}
	if s.subsumes(delta.elements) {
		return nil
	}
	return s.join(delta.elements)
}

func (s *GSet[T]) join(elements map[T]struct{}) error {
	if len(elements) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for element := range elements {
		s.elements[element] = struct{}{}
	}
	return nil
}

// subsumes reports whether all incoming additions are already represented by
// s. Membership is monotonic, so a true result remains true after releasing
// the read lock and makes duplicate delivery a read-only fast path.
func (s *GSet[T]) subsumes(elements map[T]struct{}) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for element := range elements {
		if _, exists := s.elements[element]; !exists {
			return false
		}
	}
	return true
}

// Merge joins two partial states without modifying either input delta.
func (d GSetDelta[T]) Merge(other GSetDelta[T]) (GSetDelta[T], error) {
	if d.elements == nil || other.elements == nil {
		return GSetDelta[T]{}, ErrInvalidGSet
	}
	merged := cloneGSetElements(d.elements)
	for element := range other.elements {
		merged[element] = struct{}{}
	}
	return GSetDelta[T]{elements: merged}, nil
}

// Elements returns a copy of the elements represented by d.
func (d GSetDelta[T]) Elements() []T {
	result := make([]T, 0, len(d.elements))
	for element := range d.elements {
		result = append(result, element)
	}
	return result
}

// State returns an immutable diagnostic summary.
func (s *GSet[T]) State() crdt.StateSnapshot {
	if s == nil {
		return crdt.StateSnapshot{Type: "gset"}
	}
	s.mu.RLock()
	state := crdt.StateSnapshot{Type: "gset", ReplicaID: s.replicaID, ElementCount: len(s.elements)}
	s.mu.RUnlock()
	return state
}

// MarshalBinary returns the canonical framed G-Set state.
func (s *GSet[T]) MarshalBinary() ([]byte, error) {
	if s == nil {
		return nil, ErrNilGSet
	}
	s.mu.RLock()
	codec, elements := s.codec, cloneGSetElements(s.elements)
	s.mu.RUnlock()
	return marshalGSetWithCodec(crdt.TypeIDGSetState, codec, elements, frame.DefaultLimits())
}

// Snapshot returns an immutable state snapshot. A G-Set has no clock state or
// tombstone frontier, so an empty frontier is sufficient.
func (s *GSet[T]) Snapshot() (snapshot.Snapshot, error) {
	state, err := s.MarshalBinary()
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	return snapshot.New(state, nil)
}

// MarshalBinary returns the canonical framed G-Set delta using codec.
func (d GSetDelta[T]) MarshalBinary(codec ElementCodec[T]) ([]byte, error) {
	if d.elements == nil {
		return nil, ErrInvalidGSet
	}
	return marshalGSet(crdt.TypeIDGSetDelta, codec, d.elements, frame.DefaultLimits())
}

// UnmarshalGSetDelta validates and returns a G-Set delta frame.
func UnmarshalGSetDelta[T comparable](data []byte, codec ElementCodec[T]) (GSetDelta[T], error) {
	return UnmarshalGSetDeltaWithLimits(data, codec, frame.DefaultLimits())
}

// UnmarshalGSetDeltaWithLimits validates a bounded G-Set delta frame.
func UnmarshalGSetDeltaWithLimits[T comparable](data []byte, codec ElementCodec[T], limits frame.DecoderLimits) (GSetDelta[T], error) {
	elements, err := unmarshalGSet(data, crdt.TypeIDGSetDelta, codec, limits)
	if err != nil {
		return GSetDelta[T]{}, err
	}
	return GSetDelta[T]{elements: elements}, nil
}

// UnmarshalBinary atomically replaces s with one canonical G-Set state.
func (s *GSet[T]) UnmarshalBinary(data []byte) error {
	return s.UnmarshalBinaryWithLimits(data, frame.DefaultLimits())
}

// UnmarshalBinaryWithLimits atomically replaces s after complete bounded
// validation. Failed decoding leaves the receiver unchanged.
func (s *GSet[T]) UnmarshalBinaryWithLimits(data []byte, limits frame.DecoderLimits) error {
	if s == nil {
		return ErrNilGSet
	}
	elements, err := unmarshalGSetWithCodec(data, crdt.TypeIDGSetState, s.codec, limits)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.elements = elements
	s.mu.Unlock()
	return nil
}

func marshalGSet[T comparable](typeID uint64, codec ElementCodec[T], elements map[T]struct{}, limits frame.DecoderLimits) ([]byte, error) {
	bound, err := bindElementCodec(codec)
	if err != nil {
		return nil, err
	}
	return marshalGSetWithCodec(typeID, bound, elements, limits)
}

func marshalGSetWithCodec[T comparable](typeID uint64, codec boundElementCodec[T], elements map[T]struct{}, limits frame.DecoderLimits) ([]byte, error) {
	if len(elements) > limits.MaxElements {
		return nil, frame.ErrFrameLimit
	}
	encoded := make([][]byte, 0, len(elements))
	for element := range elements {
		value, err := codec.value.Marshal(element)
		if err != nil {
			return nil, fmt.Errorf("%w: marshal element: %v", ErrInvalidCodec, err)
		}
		if len(value) > limits.MaxStringBytes {
			return nil, frame.ErrFrameLimit
		}
		encoded = append(encoded, value)
	}
	slices.SortFunc(encoded, bytes.Compare)
	payloadSize := frame.UvarintSize(uint64(len(encoded)))
	for index, value := range encoded {
		if index > 0 && bytes.Equal(encoded[index-1], value) {
			return nil, fmt.Errorf("%w: codec produced duplicate element bytes", ErrInvalidCodec)
		}
		additional := frame.UvarintSize(uint64(len(value))) + len(value)
		if additional > limits.MaxPayload-payloadSize {
			return nil, frame.ErrFrameLimit
		}
		payloadSize += additional
	}
	return frame.MarshalFrameWithPayload(typeID, codec.id, payloadSize, func(payload []byte) error {
		payload = frame.AppendUvarint(payload[:0], uint64(len(encoded)))
		for _, value := range encoded {
			payload = appendBytes(payload, value)
		}
		if len(payload) != payloadSize {
			return frame.ErrInvalidFrame
		}
		return nil
	})
}

func unmarshalGSet[T comparable](data []byte, expectedTypeID uint64, codec ElementCodec[T], limits frame.DecoderLimits) (map[T]struct{}, error) {
	bound, err := bindElementCodec(codec)
	if err != nil {
		return nil, err
	}
	return unmarshalGSetWithCodec(data, expectedTypeID, bound, limits)
}

func unmarshalGSetWithCodec[T comparable](data []byte, expectedTypeID uint64, codec boundElementCodec[T], limits frame.DecoderLimits) (map[T]struct{}, error) {
	decoded, err := frame.UnmarshalFrame(data, limits)
	if err != nil {
		return nil, err
	}
	if decoded.TypeID != expectedTypeID || decoded.CodecID != codec.id {
		return nil, ErrCodecMismatch
	}
	count, pos, ok := frame.ReadUvarint(decoded.Payload, 0)
	if !ok || count > uint64(limits.MaxElements) {
		return nil, frame.ErrInvalidFrame
	}
	elements := make(map[T]struct{}, int(count))
	var previous []byte
	for index := uint64(0); index < count; index++ {
		encoded, next, ok := frame.ReadBytes(decoded.Payload, pos, limits.MaxStringBytes)
		if !ok || (index > 0 && bytes.Compare(previous, encoded) >= 0) {
			return nil, frame.ErrInvalidFrame
		}
		pos = next
		element, err := codec.value.Unmarshal(encoded)
		if err != nil {
			return nil, fmt.Errorf("%w: unmarshal element: %v", ErrInvalidCodec, err)
		}
		canonical, err := codec.value.Marshal(element)
		if err != nil || !bytes.Equal(canonical, encoded) {
			return nil, ErrInvalidCodec
		}
		if _, exists := elements[element]; exists {
			return nil, frame.ErrInvalidFrame
		}
		elements[element] = struct{}{}
		previous = encoded
	}
	if pos != len(decoded.Payload) {
		return nil, frame.ErrInvalidFrame
	}
	return elements, nil
}

func cloneGSetElements[T comparable](source map[T]struct{}) map[T]struct{} {
	clone := make(map[T]struct{}, len(source))
	for element := range source {
		clone[element] = struct{}{}
	}
	return clone
}
