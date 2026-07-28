// Package delta provides bounded batching and coalescing for encoded CRDT
// deltas. It does not choose a network transport or retry policy.
package delta

import (
	"errors"
	"sync"

	"github.com/DarkInno/crdt"
	frame "github.com/DarkInno/crdt/encoding"
)

var (
	ErrLimit        = errors.New("delta: batch limit exceeded")
	ErrInvalid      = errors.New("delta: invalid batch")
	ErrTypeMismatch = errors.New("delta: frame type mismatch")
)

const batchMagic = "DBAT"

// Batch contains immutable copies of encoded delta frames.
type Batch struct{ items [][]byte }

// NewBatch deep-copies items and rejects total payload over maxBytes.
func NewBatch(items [][]byte, maxBytes int) (Batch, error) {
	if maxBytes <= 0 {
		return Batch{}, ErrLimit
	}
	batch := Batch{items: make([][]byte, 0, len(items))}
	total := 0
	for _, item := range items {
		if len(item) == 0 || len(item) > maxBytes-total {
			return Batch{}, ErrLimit
		}
		if !isDeltaFrame(item) {
			return Batch{}, ErrInvalid
		}
		batch.items = append(batch.items, append([]byte(nil), item...))
		total += len(item)
	}
	return batch, nil
}

// MarshalBinary returns the canonical batch envelope. Individual items remain
// independent CRDT frames, so callers can diagnose or replay them separately.
func (b Batch) MarshalBinary(maxBytes int) ([]byte, error) {
	validated, err := NewBatch(b.items, maxBytes)
	if err != nil {
		return nil, err
	}
	encoded := make([]byte, 0, 4+len(validated.items)*frameByteOverhead(validated.items))
	encoded = append(encoded, batchMagic...)
	encoded = frame.AppendUvarint(encoded, uint64(len(validated.items)))
	for _, item := range validated.items {
		encoded = frame.AppendUvarint(encoded, uint64(len(item)))
		encoded = append(encoded, item...)
	}
	return encoded, nil
}

// UnmarshalBatch accepts one complete canonical batch envelope and copies its
// items. maxItems and maxBytes bound both allocation and parsing work.
func UnmarshalBatch(data []byte, maxItems, maxBytes int) (Batch, error) {
	if maxItems <= 0 || maxBytes <= 0 || len(data) < len(batchMagic) {
		return Batch{}, ErrLimit
	}
	const maximumInt = int(^uint(0) >> 1)
	if maxItems > (maximumInt-len(batchMagic)-10)/10 || maxBytes > maximumInt-len(batchMagic)-10-maxItems*10 || len(data) > maxBytes+len(batchMagic)+10+maxItems*10 {
		return Batch{}, ErrLimit
	}
	if string(data[:len(batchMagic)]) != batchMagic {
		return Batch{}, ErrInvalid
	}
	pos := len(batchMagic)
	count, next, ok := frame.ReadUvarint(data, pos)
	if !ok || count > uint64(maxItems) {
		return Batch{}, ErrInvalid
	}
	pos = next
	items := make([][]byte, 0, int(count))
	total := 0
	for i := uint64(0); i < count; i++ {
		item, next, ok := frame.ReadBytes(data, pos, maxBytes-total)
		if !ok || len(item) == 0 {
			return Batch{}, ErrInvalid
		}
		if !isDeltaFrame(item) {
			return Batch{}, ErrInvalid
		}
		pos = next
		total += len(item)
		items = append(items, append([]byte(nil), item...))
	}
	if pos != len(data) {
		return Batch{}, ErrInvalid
	}
	return Batch{items: items}, nil
}

// Items returns deep copies of the encoded delta frames.
func (b Batch) Items() [][]byte { return cloneItems(b.items) }

// MergeFunc joins two encoded deltas of the same type. It must return a valid
// canonical delta frame with the same type and codec ID.
type MergeFunc func(left, right []byte) ([]byte, error)

// Coalescer stores a bounded stream of same-type encoded deltas. With merge
// set, Add replaces the tail with its join, avoiding a scan of earlier items.
// With nil merge it is a bounded FIFO batch builder.
type Coalescer struct {
	mu         sync.Mutex
	maxItems   int
	maxBytes   int
	merge      MergeFunc
	typeID     uint64
	codecID    string
	items      [][]byte
	total      int
	generation uint64
}

// NewCoalescer creates an empty bounded coalescer.
func NewCoalescer(maxItems, maxBytes int, merge MergeFunc) (*Coalescer, error) {
	if maxItems <= 0 || maxBytes <= 0 {
		return nil, ErrLimit
	}
	return &Coalescer{maxItems: maxItems, maxBytes: maxBytes, merge: merge}, nil
}

// Add validates item as a CRDT frame and appends or joins it. On error the
// coalescer is unchanged.
func (c *Coalescer) Add(item []byte) error {
	if c == nil {
		return ErrInvalid
	}
	if len(item) == 0 || len(item) > c.maxBytes {
		return ErrLimit
	}
	decoded, err := frame.UnmarshalFrame(item, frame.DefaultLimits())
	if err != nil {
		return err
	}
	if !isDeltaType(decoded.TypeID) {
		return ErrInvalid
	}
	copyItem := append([]byte(nil), item...)

	for {
		c.mu.Lock()
		if len(c.items) == 0 {
			c.typeID, c.codecID = decoded.TypeID, decoded.CodecID
			c.items = append(c.items, copyItem)
			c.total = len(copyItem)
			c.generation++
			c.mu.Unlock()
			return nil
		}
		if decoded.TypeID != c.typeID || decoded.CodecID != c.codecID {
			c.mu.Unlock()
			return ErrTypeMismatch
		}
		if c.merge == nil {
			if len(c.items) == c.maxItems || len(copyItem) > c.maxBytes-c.total {
				c.mu.Unlock()
				return ErrLimit
			}
			c.items = append(c.items, copyItem)
			c.total += len(copyItem)
			c.generation++
			c.mu.Unlock()
			return nil
		}
		tail := append([]byte(nil), c.items[len(c.items)-1]...)
		generation := c.generation
		typeID, codecID := c.typeID, c.codecID
		c.mu.Unlock()

		merged, err := c.merge(tail, copyItem)
		if err != nil {
			return err
		}
		mergedFrame, err := frame.UnmarshalFrame(merged, frame.DefaultLimits())
		if err != nil {
			return err
		}
		if mergedFrame.TypeID != typeID || mergedFrame.CodecID != codecID {
			return ErrTypeMismatch
		}

		c.mu.Lock()
		if c.generation != generation {
			c.mu.Unlock()
			continue
		}
		if len(merged) > c.maxBytes-(c.total-len(c.items[len(c.items)-1])) {
			c.mu.Unlock()
			return ErrLimit
		}
		c.total += len(merged) - len(c.items[len(c.items)-1])
		c.items[len(c.items)-1] = append([]byte(nil), merged...)
		c.generation++
		c.mu.Unlock()
		return nil
	}
}

// Drain returns the accumulated batch and resets c. The result owns its item
// bytes and can be safely retained by the caller.
func (c *Coalescer) Drain() Batch {
	if c == nil {
		return Batch{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	batch := Batch{items: cloneItems(c.items)}
	c.items = nil
	c.total = 0
	c.typeID = 0
	c.codecID = ""
	c.generation++
	return batch
}

// Len reports the queued item count and total item bytes.
func (c *Coalescer) Len() (items, bytes int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items), c.total
}

func cloneItems(source [][]byte) [][]byte {
	clone := make([][]byte, len(source))
	for i, item := range source {
		clone[i] = append([]byte(nil), item...)
	}
	return clone
}

func frameByteOverhead(items [][]byte) int {
	total := 0
	for _, item := range items {
		total += len(item) + 10
	}
	return total
}

func isDeltaFrame(item []byte) bool {
	decoded, err := frame.UnmarshalFrame(item, frame.DefaultLimits())
	return err == nil && isDeltaType(decoded.TypeID)
}

func isDeltaType(typeID uint64) bool {
	_, ok := crdt.FrameTypeForDelta(typeID)
	return ok
}
