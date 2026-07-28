// Package counter implements counter CRDT primitives.
package counter

import (
	"encoding/binary"
	"errors"
	"math"
	"sort"
	"sync"

	"github.com/darkinno/crdt"
	frame "github.com/darkinno/crdt/encoding"
)

var (
	// ErrInvalidReplicaID indicates that a counter or delta contains an invalid
	// logical replica identifier.
	ErrInvalidReplicaID = errors.New("counter: invalid replica ID")
	// ErrNilCounter indicates an operation received a nil counter.
	ErrNilCounter = errors.New("counter: nil G-Counter")
	// ErrCounterOverflow indicates an increment or aggregate value overflows.
	ErrCounterOverflow = errors.New("counter: value overflows uint64")
)

// GCounter is a grow-only, state-based counter. Each replica owns one map
// entry, and Merge takes the maximum value for every replica ID.
type GCounter struct {
	mu        sync.RWMutex
	replicaID string
	counts    map[string]uint64
}

// GCounterDelta is a joinable partial GCounter state. Its contents are opaque;
// Counts returns a copy for diagnostics or a future transport encoder.
type GCounterDelta struct {
	counts map[string]uint64
}

var _ crdt.CRDT[*GCounter] = (*GCounter)(nil)
var _ crdt.DeltaCapable[*GCounter, GCounterDelta] = (*GCounter)(nil)

// NewGCounter creates a G-Counter owned by replicaID.
func NewGCounter(replicaID string) (*GCounter, error) {
	if !(crdt.Tag{ReplicaID: replicaID}).Valid() {
		return nil, ErrInvalidReplicaID
	}

	return &GCounter{
		replicaID: replicaID,
		counts:    make(map[string]uint64),
	}, nil
}

// Increment adds amount to this counter's local component and returns a delta
// that represents the new component value.
func (c *GCounter) Increment(amount uint64) (GCounterDelta, error) {
	if c == nil {
		return GCounterDelta{}, ErrNilCounter
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	current := c.counts[c.replicaID]
	if math.MaxUint64-current < amount {
		return GCounterDelta{}, ErrCounterOverflow
	}

	next := current + amount
	c.counts[c.replicaID] = next
	return GCounterDelta{counts: map[string]uint64{c.replicaID: next}}, nil
}

// Value returns the sum of all replica components.
func (c *GCounter) Value() (uint64, error) {
	if c == nil {
		return 0, ErrNilCounter
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	var total uint64
	for _, value := range c.counts {
		if math.MaxUint64-total < value {
			return 0, ErrCounterOverflow
		}
		total += value
	}
	return total, nil
}

// Counts returns a copy of the per-replica components.
func (c *GCounter) Counts() map[string]uint64 {
	if c == nil {
		return nil
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneCounts(c.counts)
}

// Merge joins other into c. It copies other before taking c's write lock so
// concurrent cross-merges cannot deadlock.
func (c *GCounter) Merge(other *GCounter) error {
	if c == nil || other == nil {
		return ErrNilCounter
	}

	other.mu.RLock()
	otherCounts := cloneCounts(other.counts)
	other.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	for replicaID, value := range otherCounts {
		if value > c.counts[replicaID] {
			c.counts[replicaID] = value
		}
	}
	return nil
}

// ApplyDelta joins delta into c.
func (c *GCounter) ApplyDelta(delta GCounterDelta) error {
	if c == nil {
		return ErrNilCounter
	}
	if err := validateCounts(delta.counts); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for replicaID, value := range delta.counts {
		if value > c.counts[replicaID] {
			c.counts[replicaID] = value
		}
	}
	return nil
}

// Merge joins other into d and returns a new delta without modifying either
// input delta.
func (d GCounterDelta) Merge(other GCounterDelta) (GCounterDelta, error) {
	if err := validateCounts(d.counts); err != nil {
		return GCounterDelta{}, err
	}
	if err := validateCounts(other.counts); err != nil {
		return GCounterDelta{}, err
	}

	merged := cloneCounts(d.counts)
	for replicaID, value := range other.counts {
		if value > merged[replicaID] {
			merged[replicaID] = value
		}
	}
	return GCounterDelta{counts: merged}, nil
}

// Counts returns a copy of the components represented by d.
func (d GCounterDelta) Counts() map[string]uint64 {
	return cloneCounts(d.counts)
}

// State returns an immutable diagnostic summary.
func (c *GCounter) State() crdt.StateSnapshot {
	if c == nil {
		return crdt.StateSnapshot{Type: "gcounter"}
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	return crdt.StateSnapshot{
		Type:         "gcounter",
		ReplicaID:    c.replicaID,
		ElementCount: len(c.counts),
	}
}

// MarshalBinary returns a deterministic framed representation of c.
func (c *GCounter) MarshalBinary() ([]byte, error) {
	if c == nil {
		return nil, ErrNilCounter
	}
	c.mu.RLock()
	counts := cloneCounts(c.counts)
	c.mu.RUnlock()
	return marshalCounts(crdt.TypeIDGCounterState, counts)
}

// MarshalBinary returns a deterministic framed representation of d.
func (d GCounterDelta) MarshalBinary() ([]byte, error) {
	if err := validateCounts(d.counts); err != nil {
		return nil, err
	}
	return marshalCounts(crdt.TypeIDGCounterDelta, d.counts)
}

// UnmarshalGCounterDelta validates and returns one G-Counter delta frame.
func UnmarshalGCounterDelta(data []byte) (GCounterDelta, error) {
	return UnmarshalGCounterDeltaWithLimits(data, frame.DefaultLimits())
}

// UnmarshalGCounterDeltaWithLimits validates and returns one G-Counter delta
// frame using caller-supplied decoder limits.
func UnmarshalGCounterDeltaWithLimits(data []byte, limits frame.DecoderLimits) (GCounterDelta, error) {
	counts, err := unmarshalCounts(data, crdt.TypeIDGCounterDelta, limits)
	if err != nil {
		return GCounterDelta{}, err
	}
	return GCounterDelta{counts: counts}, nil
}

func marshalCounts(typeID uint64, counts map[string]uint64) ([]byte, error) {
	return marshalCountsWithLimits(typeID, counts, frame.DefaultLimits())
}

func marshalCountsWithLimits(typeID uint64, counts map[string]uint64, limits frame.DecoderLimits) ([]byte, error) {
	if len(counts) > limits.MaxElements {
		return nil, frame.ErrFrameLimit
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	payloadSize := uvarintSize(uint64(len(keys)))
	for _, key := range keys {
		if len(key) > limits.MaxStringBytes {
			return nil, frame.ErrFrameLimit
		}
		additional := uvarintSize(uint64(len(key))) + len(key) + uvarintSize(counts[key])
		if additional > limits.MaxPayload-payloadSize {
			return nil, frame.ErrFrameLimit
		}
		payloadSize += additional
	}
	payload := make([]byte, 0, payloadSize)
	payload = frame.AppendUvarint(payload, uint64(len(keys)))
	for _, key := range keys {
		payload = frame.AppendUvarint(payload, uint64(len(key)))
		payload = append(payload, key...)
		payload = frame.AppendUvarint(payload, counts[key])
	}
	return frame.MarshalFrame(frame.Frame{TypeID: typeID, Payload: payload})
}

func uvarintSize(value uint64) int {
	var encoded [binary.MaxVarintLen64]byte
	return binary.PutUvarint(encoded[:], value)
}

// UnmarshalBinary validates data completely before atomically replacing c's state.
func (c *GCounter) UnmarshalBinary(data []byte) error {
	return c.UnmarshalBinaryWithLimits(data, frame.DefaultLimits())
}

// UnmarshalBinaryWithLimits validates data completely before atomically
// replacing c's state using caller-supplied decoder limits.
func (c *GCounter) UnmarshalBinaryWithLimits(data []byte, limits frame.DecoderLimits) error {
	if c == nil {
		return ErrNilCounter
	}
	counts, err := unmarshalCounts(data, crdt.TypeIDGCounterState, limits)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.counts = counts
	c.mu.Unlock()
	return nil
}

func unmarshalCounts(data []byte, expectedTypeID uint64, limits frame.DecoderLimits) (map[string]uint64, error) {
	decoded, err := frame.UnmarshalFrame(data, limits)
	if err != nil {
		return nil, err
	}
	if decoded.TypeID != expectedTypeID || decoded.CodecID != "" {
		return nil, frame.ErrInvalidFrame
	}
	pos := 0
	count, next, ok := frame.ReadUvarint(decoded.Payload, pos)
	if !ok || count > uint64(limits.MaxElements) {
		return nil, frame.ErrInvalidFrame
	}
	pos = next
	counts := make(map[string]uint64, int(count))
	previous := ""
	for i := uint64(0); i < count; i++ {
		keyBytes, next, ok := frame.ReadBytes(decoded.Payload, pos, limits.MaxStringBytes)
		if !ok {
			return nil, frame.ErrInvalidFrame
		}
		pos = next
		key := string(keyBytes)
		value, next, ok := frame.ReadUvarint(decoded.Payload, pos)
		if !ok || !(crdt.Tag{ReplicaID: key}).Valid() || (i > 0 && key <= previous) {
			return nil, frame.ErrInvalidFrame
		}
		pos = next
		counts[key] = value
		previous = key
	}
	if pos != len(decoded.Payload) {
		return nil, frame.ErrInvalidFrame
	}
	return counts, nil
}

func cloneCounts(counts map[string]uint64) map[string]uint64 {
	clone := make(map[string]uint64, len(counts))
	for replicaID, value := range counts {
		clone[replicaID] = value
	}
	return clone
}

func validateCounts(counts map[string]uint64) error {
	for replicaID := range counts {
		if !(crdt.Tag{ReplicaID: replicaID}).Valid() {
			return ErrInvalidReplicaID
		}
	}
	return nil
}
