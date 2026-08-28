package set

import "github.com/im10furry/crdt"

// MarshalJSON returns a diagnostic summary for structured logs. It omits set
// elements, tags, clock state, and codec data, and cannot restore the set.
func (s *GSet[T]) MarshalJSON() ([]byte, error) { return crdt.MarshalStateJSON(s) }

// MarshalJSON returns a diagnostic summary for structured logs. It omits set
// elements, tags, clock state, and codec data, and cannot restore the set.
func (s *ORSet[T]) MarshalJSON() ([]byte, error) { return crdt.MarshalStateJSON(s) }

// MarshalJSON returns a diagnostic summary for structured logs. It omits
// elements and codec data and cannot be applied as a delta from JSON.
func (d GSetDelta[T]) MarshalJSON() ([]byte, error) {
	return crdt.MarshalDiagnosticJSON(crdt.StateSnapshot{
		Type:         "gset-delta",
		ElementCount: len(d.elements),
	})
}

// MarshalJSON returns a diagnostic summary for structured logs. It omits
// elements, tags, and clock state and cannot be applied as a delta from JSON.
func (d ORSetDelta[T]) MarshalJSON() ([]byte, error) {
	return crdt.MarshalDiagnosticJSON(crdt.StateSnapshot{
		Type:           "orset-delta",
		ElementCount:   len(d.adds),
		TombstoneCount: len(d.tombstones),
	})
}
