package counter

import (
	"errors"
	"math"
	"math/big"
	"sort"
	"sync"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
)

var (
	// ErrNilPNCounter indicates an operation received a nil PN-Counter.
	ErrNilPNCounter = errors.New("counter: nil PN-Counter")
)

// PNCounter is a state-based positive-negative counter. Each component is a
// per-replica G-Counter: positive records increments and negative records
// decrements. Merge takes the maximum component value in both maps.
type PNCounter struct {
	mu        sync.RWMutex
	replicaID string
	positive  map[string]uint64
	negative  map[string]uint64
}

// PNCounterDelta is a joinable partial PN-Counter state. Its contents are
// opaque; the count accessors return copies for diagnostics and transport.
type PNCounterDelta struct {
	positive map[string]uint64
	negative map[string]uint64
}

var _ crdt.CRDT[*PNCounter] = (*PNCounter)(nil)
var _ crdt.DeltaCapable[*PNCounter, PNCounterDelta] = (*PNCounter)(nil)

// NewPNCounter creates a PN-Counter owned by replicaID.
func NewPNCounter(replicaID string) (*PNCounter, error) {
	if !(crdt.Tag{ReplicaID: replicaID}).Valid() {
		return nil, ErrInvalidReplicaID
	}
	return &PNCounter{
		replicaID: replicaID,
		positive:  make(map[string]uint64),
		negative:  make(map[string]uint64),
	}, nil
}

// Increment adds amount to this replica's positive component and returns the
// resulting component as a delta.
func (c *PNCounter) Increment(amount uint64) (PNCounterDelta, error) {
	return c.add(amount, true)
}

// Decrement adds amount to this replica's negative component and returns the
// resulting component as a delta.
func (c *PNCounter) Decrement(amount uint64) (PNCounterDelta, error) {
	return c.add(amount, false)
}

func (c *PNCounter) add(amount uint64, positive bool) (PNCounterDelta, error) {
	if c == nil {
		return PNCounterDelta{}, ErrNilPNCounter
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	counts := c.negative
	if positive {
		counts = c.positive
	}
	current := counts[c.replicaID]
	if math.MaxUint64-current < amount {
		return PNCounterDelta{}, ErrCounterOverflow
	}
	next := current + amount
	counts[c.replicaID] = next
	if positive {
		return PNCounterDelta{positive: map[string]uint64{c.replicaID: next}}, nil
	}
	return PNCounterDelta{negative: map[string]uint64{c.replicaID: next}}, nil
}

// Value returns the exact signed counter value as a newly allocated integer.
// It uses big.Int so every valid uint64 component state remains representable.
func (c *PNCounter) Value() (*big.Int, error) {
	if c == nil {
		return nil, ErrNilPNCounter
	}
	c.mu.RLock()
	value := new(big.Int).Sub(sumCounts(c.positive), sumCounts(c.negative))
	c.mu.RUnlock()
	return value, nil
}

// ValueInt64 returns the signed value when it fits in int64.
func (c *PNCounter) ValueInt64() (int64, error) {
	value, err := c.Value()
	if err != nil {
		return 0, err
	}
	if !value.IsInt64() {
		return 0, ErrCounterOverflow
	}
	return value.Int64(), nil
}

// PositiveCounts returns a copy of the per-replica increment components.
func (c *PNCounter) PositiveCounts() map[string]uint64 {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneCounts(c.positive)
}

// NegativeCounts returns a copy of the per-replica decrement components.
func (c *PNCounter) NegativeCounts() map[string]uint64 {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return cloneCounts(c.negative)
}

// Merge joins other into c. It snapshots other before taking c's write lock so
// concurrent cross-merges cannot deadlock.
func (c *PNCounter) Merge(other *PNCounter) error {
	if c == nil || other == nil {
		return ErrNilPNCounter
	}
	other.mu.RLock()
	positive := cloneCounts(other.positive)
	negative := cloneCounts(other.negative)
	other.mu.RUnlock()

	c.mu.Lock()
	joinCounts(c.positive, positive)
	joinCounts(c.negative, negative)
	c.mu.Unlock()
	return nil
}

// ApplyDelta joins delta into c.
func (c *PNCounter) ApplyDelta(delta PNCounterDelta) error {
	_, err := c.ApplyDeltaChanged(delta)
	return err
}

// ApplyDeltaChanged joins delta into c and reports whether it extended either
// retained component map. It makes duplicate remote delivery observable to a
// caller without weakening the counter's idempotent join semantics.
func (c *PNCounter) ApplyDeltaChanged(delta PNCounterDelta) (bool, error) {
	if c == nil {
		return false, ErrNilPNCounter
	}
	if err := validatePNCounts(delta.positive, delta.negative); err != nil {
		return false, err
	}
	c.mu.Lock()
	changed := joinCountsChanged(c.positive, delta.positive)
	changed = joinCountsChanged(c.negative, delta.negative) || changed
	c.mu.Unlock()
	return changed, nil
}

// Merge joins other into d and returns a new delta without modifying either
// input delta.
func (d PNCounterDelta) Merge(other PNCounterDelta) (PNCounterDelta, error) {
	if err := validatePNCounts(d.positive, d.negative); err != nil {
		return PNCounterDelta{}, err
	}
	if err := validatePNCounts(other.positive, other.negative); err != nil {
		return PNCounterDelta{}, err
	}
	positive := cloneCounts(d.positive)
	negative := cloneCounts(d.negative)
	joinCounts(positive, other.positive)
	joinCounts(negative, other.negative)
	return PNCounterDelta{positive: positive, negative: negative}, nil
}

// PositiveCounts returns a copy of the increment components represented by d.
func (d PNCounterDelta) PositiveCounts() map[string]uint64 { return cloneCounts(d.positive) }

// NegativeCounts returns a copy of the decrement components represented by d.
func (d PNCounterDelta) NegativeCounts() map[string]uint64 { return cloneCounts(d.negative) }

// State returns an immutable diagnostic summary.
func (c *PNCounter) State() crdt.StateSnapshot {
	if c == nil {
		return crdt.StateSnapshot{Type: "pncounter"}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return crdt.StateSnapshot{
		Type:         "pncounter",
		ReplicaID:    c.replicaID,
		ElementCount: len(c.positive) + len(c.negative),
	}
}

// MarshalBinary returns a deterministic framed representation of c.
func (c *PNCounter) MarshalBinary() ([]byte, error) {
	if c == nil {
		return nil, ErrNilPNCounter
	}
	c.mu.RLock()
	positive := cloneCounts(c.positive)
	negative := cloneCounts(c.negative)
	c.mu.RUnlock()
	return marshalPNCounts(crdt.TypeIDPNCounterState, positive, negative)
}

// MarshalBinary returns a deterministic framed representation of d.
func (d PNCounterDelta) MarshalBinary() ([]byte, error) {
	if err := validatePNCounts(d.positive, d.negative); err != nil {
		return nil, err
	}
	return marshalPNCounts(crdt.TypeIDPNCounterDelta, d.positive, d.negative)
}

// UnmarshalPNCounterDelta validates and returns one PN-Counter delta frame.
func UnmarshalPNCounterDelta(data []byte) (PNCounterDelta, error) {
	return UnmarshalPNCounterDeltaWithLimits(data, frame.DefaultLimits())
}

// UnmarshalPNCounterDeltaWithLimits validates and returns one PN-Counter
// delta frame using caller-supplied decoder limits.
func UnmarshalPNCounterDeltaWithLimits(data []byte, limits frame.DecoderLimits) (PNCounterDelta, error) {
	positive, negative, err := unmarshalPNCounts(data, crdt.TypeIDPNCounterDelta, limits)
	if err != nil {
		return PNCounterDelta{}, err
	}
	return PNCounterDelta{positive: positive, negative: negative}, nil
}

// UnmarshalBinary validates data completely before atomically replacing c's state.
func (c *PNCounter) UnmarshalBinary(data []byte) error {
	return c.UnmarshalBinaryWithLimits(data, frame.DefaultLimits())
}

// UnmarshalBinaryWithLimits validates data completely before atomically
// replacing c's state using caller-supplied decoder limits.
func (c *PNCounter) UnmarshalBinaryWithLimits(data []byte, limits frame.DecoderLimits) error {
	if c == nil {
		return ErrNilPNCounter
	}
	positive, negative, err := unmarshalPNCounts(data, crdt.TypeIDPNCounterState, limits)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.positive = positive
	c.negative = negative
	c.mu.Unlock()
	return nil
}

func marshalPNCounts(typeID uint64, positive, negative map[string]uint64) ([]byte, error) {
	return marshalPNCountsWithLimits(typeID, positive, negative, frame.DefaultLimits())
}

func marshalPNCountsWithLimits(typeID uint64, positive, negative map[string]uint64, limits frame.DecoderLimits) ([]byte, error) {
	if err := validatePNCounts(positive, negative); err != nil {
		return nil, err
	}
	if len(positive) > limits.MaxElements || len(negative) > limits.MaxElements-len(positive) {
		return nil, frame.ErrFrameLimit
	}
	positiveKeys, positiveSize, err := pnCountsSection(positive, limits)
	if err != nil {
		return nil, err
	}
	negativeKeys, negativeSize, err := pnCountsSection(negative, limits)
	if err != nil || negativeSize > limits.MaxPayload-positiveSize {
		if err != nil {
			return nil, err
		}
		return nil, frame.ErrFrameLimit
	}
	payload := make([]byte, 0, positiveSize+negativeSize)
	payload = appendPNCountsSection(payload, positiveKeys, positive)
	payload = appendPNCountsSection(payload, negativeKeys, negative)
	return frame.MarshalFrame(frame.Frame{TypeID: typeID, Payload: payload})
}

func pnCountsSection(counts map[string]uint64, limits frame.DecoderLimits) ([]string, int, error) {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	size := uvarintSize(uint64(len(keys)))
	for _, key := range keys {
		if len(key) > limits.MaxStringBytes {
			return nil, 0, frame.ErrFrameLimit
		}
		additional := uvarintSize(uint64(len(key))) + len(key) + uvarintSize(counts[key])
		if additional > limits.MaxPayload-size {
			return nil, 0, frame.ErrFrameLimit
		}
		size += additional
	}
	return keys, size, nil
}

func appendPNCountsSection(payload []byte, keys []string, counts map[string]uint64) []byte {
	payload = frame.AppendUvarint(payload, uint64(len(keys)))
	for _, key := range keys {
		payload = frame.AppendUvarint(payload, uint64(len(key)))
		payload = append(payload, key...)
		payload = frame.AppendUvarint(payload, counts[key])
	}
	return payload
}

func unmarshalPNCounts(data []byte, expectedTypeID uint64, limits frame.DecoderLimits) (map[string]uint64, map[string]uint64, error) {
	decoded, err := frame.UnmarshalFrame(data, limits)
	if err != nil {
		return nil, nil, err
	}
	if decoded.TypeID != expectedTypeID || decoded.CodecID != "" {
		return nil, nil, frame.ErrInvalidFrame
	}
	position := 0
	positive, position, err := unmarshalPNCountsSection(decoded.Payload, position, limits.MaxElements, limits)
	if err != nil {
		return nil, nil, err
	}
	negative, position, err := unmarshalPNCountsSection(decoded.Payload, position, limits.MaxElements-len(positive), limits)
	if err != nil || position != len(decoded.Payload) {
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, frame.ErrInvalidFrame
	}
	return positive, negative, nil
}

func unmarshalPNCountsSection(payload []byte, position, maxElements int, limits frame.DecoderLimits) (map[string]uint64, int, error) {
	count, next, ok := frame.ReadUvarint(payload, position)
	// Do not reserve map capacity from an untrusted declaration. The loop
	// validates each entry before adding it and enforces the configured bound.
	if !ok || maxElements < 0 {
		return nil, position, frame.ErrInvalidFrame
	}
	position = next
	counts := make(map[string]uint64)
	previous := ""
	for index := uint64(0); index < count; index++ {
		if len(counts) >= maxElements {
			return nil, position, frame.ErrInvalidFrame
		}
		keyBytes, next, ok := frame.ReadBytes(payload, position, limits.MaxStringBytes)
		if !ok {
			return nil, position, frame.ErrInvalidFrame
		}
		position = next
		key := string(keyBytes)
		value, next, ok := frame.ReadUvarint(payload, position)
		if !ok || !(crdt.Tag{ReplicaID: key}).Valid() || (index > 0 && key <= previous) {
			return nil, position, frame.ErrInvalidFrame
		}
		position = next
		counts[key] = value
		previous = key
	}
	return counts, position, nil
}

func validatePNCounts(positive, negative map[string]uint64) error {
	if err := validateCounts(positive); err != nil {
		return err
	}
	return validateCounts(negative)
}

func joinCounts(target, source map[string]uint64) {
	for replicaID, value := range source {
		if value > target[replicaID] {
			target[replicaID] = value
		}
	}
}

func joinCountsChanged(target, source map[string]uint64) bool {
	changed := false
	for replicaID, value := range source {
		if value > target[replicaID] {
			target[replicaID] = value
			changed = true
		}
	}
	return changed
}

func sumCounts(counts map[string]uint64) *big.Int {
	total := new(big.Int)
	var component big.Int
	for _, value := range counts {
		component.SetUint64(value)
		total.Add(total, &component)
	}
	return total
}
