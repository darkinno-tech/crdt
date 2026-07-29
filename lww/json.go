package lww

import "github.com/DarkInno/crdt"

// MarshalJSON returns a diagnostic summary for structured logs. It omits
// elements, keys, values, tags, and clock state, and cannot restore the set.
func (s *Set[T]) MarshalJSON() ([]byte, error) { return crdt.MarshalStateJSON(s) }

// MarshalJSON returns a diagnostic summary for structured logs. It omits set
// elements and tags and cannot be applied as a delta from JSON.
func (d SetDelta[T]) MarshalJSON() ([]byte, error) {
	present := 0
	for _, entry := range d.entries {
		if entry.present {
			present++
		}
	}
	return crdt.MarshalDiagnosticJSON(crdt.StateSnapshot{
		Type:           "lww-set-delta",
		ElementCount:   present,
		TombstoneCount: len(d.entries) - present,
	})
}

// MarshalJSON returns a diagnostic summary for structured logs. It omits map
// keys, values, tags, and clock state, and cannot restore the map.
func (m *Map) MarshalJSON() ([]byte, error) { return crdt.MarshalStateJSON(m) }

// MarshalJSON returns a diagnostic summary for structured logs. It omits map
// keys, values, tags, and clock state and cannot be applied as a delta from JSON.
func (d MapDelta) MarshalJSON() ([]byte, error) {
	present := 0
	for _, entry := range d.entries {
		if entry.present {
			present++
		}
	}
	return crdt.MarshalDiagnosticJSON(crdt.StateSnapshot{
		Type:           "lww-map-delta",
		ElementCount:   present,
		TombstoneCount: len(d.entries) - present,
	})
}
