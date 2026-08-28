package register

import (
	"bytes"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/darkinno-tech/crdt"
	frame "github.com/darkinno-tech/crdt/encoding"
	"github.com/darkinno-tech/crdt/snapshot"
)

var (
	ErrNilMVRegister      = errors.New("register: nil MV-Register")
	ErrInvalidMVRegister  = errors.New("register: invalid MV-Register state")
	ErrMVRegisterOverflow = errors.New("register: MV-Register version overflow")
	ErrInvalidMVSnapshot  = errors.New("register: invalid MV-Register snapshot")
)

// MVEntry is one concurrently visible MV-Register value. Value is owned by
// the caller; it never aliases register state.
type MVEntry struct {
	ReplicaID string
	Counter   uint64
	Value     []byte
}

type mvDot struct {
	replicaID string
	counter   uint64
}

// MVRegister is a multi-value register. A Set overwrites every value it has
// observed, while values written concurrently at different replicas remain
// visible together. Causal context is represented by a per-replica version
// vector, not wall-clock time, so clock skew cannot discard a concurrent write.
type MVRegister struct {
	mu        sync.RWMutex
	replicaID string
	context   map[string]uint64
	values    map[mvDot][]byte
}

// MVRegisterDelta is a joinable partial MV-Register state. Set returns a
// delta carrying its new value and the full causal context it observed, which
// is required to remove only causally prior values at another replica.
type MVRegisterDelta struct {
	context map[string]uint64
	values  map[mvDot][]byte
}

var _ crdt.CRDT[*MVRegister] = (*MVRegister)(nil)
var _ crdt.DeltaCapable[*MVRegister, MVRegisterDelta] = (*MVRegister)(nil)

// NewMVRegister creates a causally replicated multi-value register for
// replicaID. Reusing an ID requires restoring its prior state first.
func NewMVRegister(replicaID string) (*MVRegister, error) {
	if !(crdt.Tag{ReplicaID: replicaID}).Valid() {
		return nil, ErrInvalidReplicaID
	}
	return &MVRegister{
		replicaID: replicaID,
		context:   make(map[string]uint64),
		values:    make(map[mvDot][]byte),
	}, nil
}

// NewMVRegisterFromSnapshot restores a register and its causal context. The
// restored state must be persisted atomically before reusing replicaID.
func NewMVRegisterFromSnapshot(replicaID string, saved snapshot.Snapshot) (*MVRegister, error) {
	if saved.TypeID != crdt.TypeIDMVRegisterState {
		return nil, ErrInvalidMVSnapshot
	}
	value, err := NewMVRegister(replicaID)
	if err != nil {
		return nil, err
	}
	if err := value.UnmarshalBinary(saved.Bytes()); err != nil {
		return nil, err
	}
	return value, nil
}

// Set stores value, causally overwriting all values currently observed by r,
// and returns a delta suitable for duplicate and out-of-order delivery.
func (r *MVRegister) Set(value []byte) (MVRegisterDelta, error) {
	if r == nil {
		return MVRegisterDelta{}, ErrNilMVRegister
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.context[r.replicaID]
	if current == math.MaxUint64 {
		return MVRegisterDelta{}, ErrMVRegisterOverflow
	}
	next := current + 1
	r.context[r.replicaID] = next
	dot := mvDot{replicaID: r.replicaID, counter: next}
	r.values = map[mvDot][]byte{dot: append([]byte(nil), value...)}
	return MVRegisterDelta{
		context: cloneMVContext(r.context),
		values:  cloneMVValues(r.values),
	}, nil
}

// Values returns all concurrently visible values in canonical dot order.
func (r *MVRegister) Values() []MVEntry {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	entries := mvEntries(r.values)
	r.mu.RUnlock()
	return entries
}

// Value returns the sole value when no write is concurrent with it.
func (r *MVRegister) Value() ([]byte, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	if len(r.values) != 1 {
		r.mu.RUnlock()
		return nil, false
	}
	for _, value := range r.values {
		result := append([]byte(nil), value...)
		r.mu.RUnlock()
		return result, true
	}
	r.mu.RUnlock()
	return nil, false
}

// Merge joins other into r. The receiver changes only after both states have
// been validated and a same-dot value conflict has been ruled out.
func (r *MVRegister) Merge(other *MVRegister) error {
	if r == nil || other == nil {
		return ErrNilMVRegister
	}
	if r == other {
		return nil
	}
	other.mu.RLock()
	context, values := cloneMVContext(other.context), cloneMVValues(other.values)
	other.mu.RUnlock()
	if err := validateMVState(context, values); err != nil {
		return err
	}
	return r.join(context, values)
}

// ApplyDelta joins delta into r.
func (r *MVRegister) ApplyDelta(delta MVRegisterDelta) error {
	if r == nil {
		return ErrNilMVRegister
	}
	if err := validateMVState(delta.context, delta.values); err != nil {
		return err
	}
	if covered, err := r.subsumes(delta.context, delta.values); err != nil || covered {
		return err
	}
	return r.join(delta.context, delta.values)
}

// subsumes reports whether context is already known by r. Same-dot values are
// checked while read-locked so a malformed conflicting delta cannot be hidden
// by an otherwise dominating causal context.
func (r *MVRegister) subsumes(context map[string]uint64, values map[mvDot][]byte) (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for dot, incoming := range values {
		if current, exists := r.values[dot]; exists && !bytes.Equal(current, incoming) {
			return false, ErrTagConflict
		}
	}
	for replicaID, counter := range context {
		if r.context[replicaID] < counter {
			return false, nil
		}
	}
	return true, nil
}

func (r *MVRegister) join(context map[string]uint64, values map[mvDot][]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := validateMVState(r.context, r.values); err != nil {
		return err
	}
	mergedContext, mergedValues, err := joinMVState(r.context, r.values, context, values)
	if err != nil {
		return err
	}
	r.context, r.values = mergedContext, mergedValues
	return nil
}

// Merge joins partial states without modifying either input delta.
func (d MVRegisterDelta) Merge(other MVRegisterDelta) (MVRegisterDelta, error) {
	if err := validateMVState(d.context, d.values); err != nil {
		return MVRegisterDelta{}, err
	}
	if err := validateMVState(other.context, other.values); err != nil {
		return MVRegisterDelta{}, err
	}
	context, values, err := joinMVState(d.context, d.values, other.context, other.values)
	if err != nil {
		return MVRegisterDelta{}, err
	}
	return MVRegisterDelta{context: context, values: values}, nil
}

// Values returns copies of the values represented by d in canonical order.
func (d MVRegisterDelta) Values() []MVEntry { return mvEntries(d.values) }

// State returns an immutable diagnostic summary.
func (r *MVRegister) State() crdt.StateSnapshot {
	if r == nil {
		return crdt.StateSnapshot{Type: "mv-register"}
	}
	r.mu.RLock()
	state := crdt.StateSnapshot{Type: "mv-register", ReplicaID: r.replicaID, ElementCount: len(r.values)}
	r.mu.RUnlock()
	return state
}

// MarshalBinary returns the canonical framed MV-Register state.
func (r *MVRegister) MarshalBinary() ([]byte, error) {
	if r == nil {
		return nil, ErrNilMVRegister
	}
	r.mu.RLock()
	context, values := cloneMVContext(r.context), cloneMVValues(r.values)
	r.mu.RUnlock()
	return marshalMVRegister(crdt.TypeIDMVRegisterState, context, values, frame.DefaultLimits())
}

// Snapshot returns an immutable state snapshot containing the causal context
// required to resume writes safely with the same replica ID.
func (r *MVRegister) Snapshot() (snapshot.Snapshot, error) {
	state, err := r.MarshalBinary()
	if err != nil {
		return snapshot.Snapshot{}, err
	}
	return snapshot.New(state, nil)
}

// MarshalBinary returns the canonical framed MV-Register delta.
func (d MVRegisterDelta) MarshalBinary() ([]byte, error) {
	return marshalMVRegister(crdt.TypeIDMVRegisterDelta, d.context, d.values, frame.DefaultLimits())
}

// UnmarshalMVRegisterDelta validates and returns one MV-Register delta frame.
func UnmarshalMVRegisterDelta(data []byte) (MVRegisterDelta, error) {
	return UnmarshalMVRegisterDeltaWithLimits(data, frame.DefaultLimits())
}

// UnmarshalMVRegisterDeltaWithLimits validates a bounded delta frame.
func UnmarshalMVRegisterDeltaWithLimits(data []byte, limits frame.DecoderLimits) (MVRegisterDelta, error) {
	context, values, err := unmarshalMVRegister(data, crdt.TypeIDMVRegisterDelta, limits)
	if err != nil {
		return MVRegisterDelta{}, err
	}
	return MVRegisterDelta{context: context, values: values}, nil
}

// UnmarshalBinary atomically replaces r with one canonical state frame.
func (r *MVRegister) UnmarshalBinary(data []byte) error {
	return r.UnmarshalBinaryWithLimits(data, frame.DefaultLimits())
}

// UnmarshalBinaryWithLimits validates all state before replacing r.
func (r *MVRegister) UnmarshalBinaryWithLimits(data []byte, limits frame.DecoderLimits) error {
	if r == nil {
		return ErrNilMVRegister
	}
	context, values, err := unmarshalMVRegister(data, crdt.TypeIDMVRegisterState, limits)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.context, r.values = context, values
	r.mu.Unlock()
	return nil
}

func joinMVState(leftContext map[string]uint64, leftValues map[mvDot][]byte, rightContext map[string]uint64, rightValues map[mvDot][]byte) (map[string]uint64, map[mvDot][]byte, error) {
	mergedValues := make(map[mvDot][]byte, len(leftValues)+len(rightValues))
	for dot, left := range leftValues {
		if right, shared := rightValues[dot]; shared {
			if !bytes.Equal(left, right) {
				return nil, nil, ErrTagConflict
			}
			mergedValues[dot] = append([]byte(nil), left...)
			continue
		}
		if !mvContextCovers(rightContext, dot) {
			mergedValues[dot] = append([]byte(nil), left...)
		}
	}
	for dot, right := range rightValues {
		if _, shared := leftValues[dot]; shared {
			continue
		}
		if !mvContextCovers(leftContext, dot) {
			mergedValues[dot] = append([]byte(nil), right...)
		}
	}
	mergedContext := cloneMVContext(leftContext)
	for replicaID, counter := range rightContext {
		if counter > mergedContext[replicaID] {
			mergedContext[replicaID] = counter
		}
	}
	return mergedContext, mergedValues, nil
}

func marshalMVRegister(typeID uint64, context map[string]uint64, values map[mvDot][]byte, limits frame.DecoderLimits) ([]byte, error) {
	if err := validateMVState(context, values); err != nil {
		return nil, err
	}
	if len(context) > limits.MaxElements || len(values) > limits.MaxElements-len(context) {
		return nil, frame.ErrFrameLimit
	}
	replicas := make([]string, 0, len(context))
	for replicaID := range context {
		replicas = append(replicas, replicaID)
	}
	sort.Strings(replicas)
	dots := sortedMVDots(values)
	payloadSize := frame.UvarintSize(uint64(len(replicas)))
	for _, replicaID := range replicas {
		if len(replicaID) > limits.MaxStringBytes {
			return nil, frame.ErrFrameLimit
		}
		additional := frame.UvarintSize(uint64(len(replicaID))) + len(replicaID) + frame.UvarintSize(context[replicaID])
		if additional > limits.MaxPayload-payloadSize {
			return nil, frame.ErrFrameLimit
		}
		payloadSize += additional
	}
	additional := frame.UvarintSize(uint64(len(dots)))
	if additional > limits.MaxPayload-payloadSize {
		return nil, frame.ErrFrameLimit
	}
	payloadSize += additional
	for _, dot := range dots {
		value := values[dot]
		if len(dot.replicaID) > limits.MaxStringBytes || len(value) > limits.MaxStringBytes {
			return nil, frame.ErrFrameLimit
		}
		additional = frame.UvarintSize(uint64(len(dot.replicaID))) + len(dot.replicaID) + frame.UvarintSize(dot.counter) + frame.UvarintSize(uint64(len(value))) + len(value)
		if additional > limits.MaxPayload-payloadSize {
			return nil, frame.ErrFrameLimit
		}
		payloadSize += additional
	}
	return frame.MarshalFrameWithPayload(typeID, "", payloadSize, func(payload []byte) error {
		payload = frame.AppendUvarint(payload[:0], uint64(len(replicas)))
		for _, replicaID := range replicas {
			payload = appendMVBytes(payload, []byte(replicaID))
			payload = frame.AppendUvarint(payload, context[replicaID])
		}
		payload = frame.AppendUvarint(payload, uint64(len(dots)))
		for _, dot := range dots {
			payload = appendMVBytes(payload, []byte(dot.replicaID))
			payload = frame.AppendUvarint(payload, dot.counter)
			payload = appendMVBytes(payload, values[dot])
		}
		if len(payload) != payloadSize {
			return frame.ErrInvalidFrame
		}
		return nil
	})
}

func unmarshalMVRegister(data []byte, expectedTypeID uint64, limits frame.DecoderLimits) (map[string]uint64, map[mvDot][]byte, error) {
	decoded, err := frame.UnmarshalFrame(data, limits)
	if err != nil {
		return nil, nil, err
	}
	if decoded.TypeID != expectedTypeID || decoded.CodecID != "" {
		return nil, nil, frame.ErrInvalidFrame
	}
	contextCount, pos, ok := frame.ReadUvarint(decoded.Payload, 0)
	if !ok || contextCount > uint64(limits.MaxElements) {
		return nil, nil, frame.ErrInvalidFrame
	}
	context := make(map[string]uint64, int(contextCount))
	previousReplica := ""
	for index := uint64(0); index < contextCount; index++ {
		replicaBytes, next, ok := frame.ReadBytes(decoded.Payload, pos, limits.MaxStringBytes)
		if !ok {
			return nil, nil, frame.ErrInvalidFrame
		}
		pos = next
		counter, next, ok := frame.ReadUvarint(decoded.Payload, pos)
		if !ok {
			return nil, nil, frame.ErrInvalidFrame
		}
		pos = next
		replicaID := string(replicaBytes)
		if !(crdt.Tag{ReplicaID: replicaID}).Valid() || counter == 0 || (index > 0 && replicaID <= previousReplica) {
			return nil, nil, frame.ErrInvalidFrame
		}
		context[replicaID] = counter
		previousReplica = replicaID
	}
	valueCount, next, ok := frame.ReadUvarint(decoded.Payload, pos)
	if !ok || valueCount > uint64(limits.MaxElements)-contextCount {
		return nil, nil, frame.ErrInvalidFrame
	}
	pos = next
	values := make(map[mvDot][]byte, int(valueCount))
	var previousDot mvDot
	for index := uint64(0); index < valueCount; index++ {
		replicaBytes, next, ok := frame.ReadBytes(decoded.Payload, pos, limits.MaxStringBytes)
		if !ok {
			return nil, nil, frame.ErrInvalidFrame
		}
		pos = next
		counter, next, ok := frame.ReadUvarint(decoded.Payload, pos)
		if !ok {
			return nil, nil, frame.ErrInvalidFrame
		}
		pos = next
		value, next, ok := frame.ReadBytes(decoded.Payload, pos, limits.MaxStringBytes)
		if !ok {
			return nil, nil, frame.ErrInvalidFrame
		}
		pos = next
		dot := mvDot{replicaID: string(replicaBytes), counter: counter}
		if !validMVDot(dot) || !mvContextCovers(context, dot) || (index > 0 && compareMVDot(previousDot, dot) >= 0) {
			return nil, nil, frame.ErrInvalidFrame
		}
		values[dot] = append([]byte(nil), value...)
		previousDot = dot
	}
	if pos != len(decoded.Payload) {
		return nil, nil, frame.ErrInvalidFrame
	}
	return context, values, nil
}

func validateMVState(context map[string]uint64, values map[mvDot][]byte) error {
	if context == nil || values == nil {
		return ErrInvalidMVRegister
	}
	for replicaID, counter := range context {
		if !(crdt.Tag{ReplicaID: replicaID}).Valid() || counter == 0 {
			return ErrInvalidMVRegister
		}
	}
	for dot := range values {
		if !validMVDot(dot) || !mvContextCovers(context, dot) {
			return ErrInvalidMVRegister
		}
	}
	return nil
}

func validMVDot(dot mvDot) bool {
	return strings.TrimSpace(dot.replicaID) != "" && dot.counter != 0
}

func mvContextCovers(context map[string]uint64, dot mvDot) bool {
	return context[dot.replicaID] >= dot.counter
}

func compareMVDot(left, right mvDot) int {
	if left.replicaID < right.replicaID {
		return -1
	}
	if left.replicaID > right.replicaID {
		return 1
	}
	if left.counter < right.counter {
		return -1
	}
	if left.counter > right.counter {
		return 1
	}
	return 0
}

func sortedMVDots(values map[mvDot][]byte) []mvDot {
	dots := make([]mvDot, 0, len(values))
	for dot := range values {
		dots = append(dots, dot)
	}
	sort.Slice(dots, func(i, j int) bool { return compareMVDot(dots[i], dots[j]) < 0 })
	return dots
}

func cloneMVContext(context map[string]uint64) map[string]uint64 {
	clone := make(map[string]uint64, len(context))
	for replicaID, counter := range context {
		clone[replicaID] = counter
	}
	return clone
}

func cloneMVValues(values map[mvDot][]byte) map[mvDot][]byte {
	clone := make(map[mvDot][]byte, len(values))
	for dot, value := range values {
		clone[dot] = append([]byte(nil), value...)
	}
	return clone
}

func mvEntries(values map[mvDot][]byte) []MVEntry {
	dots := sortedMVDots(values)
	entries := make([]MVEntry, 0, len(dots))
	for _, dot := range dots {
		entries = append(entries, MVEntry{ReplicaID: dot.replicaID, Counter: dot.counter, Value: append([]byte(nil), values[dot]...)})
	}
	return entries
}

func appendMVBytes(dst, value []byte) []byte {
	dst = frame.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}
