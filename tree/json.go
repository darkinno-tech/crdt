package tree

import "github.com/DarkInno/crdt"

// MarshalJSON returns a diagnostic summary for structured logs. It omits node
// values, identities, tombstone identities, and clock state.
func (t *ORTree) MarshalJSON() ([]byte, error) { return crdt.MarshalStateJSON(t) }

// MarshalJSON returns a diagnostic summary for structured logs. It omits node
// values, identities, tombstone identities, and clock state.
func (d Delta) MarshalJSON() ([]byte, error) {
	return crdt.MarshalDiagnosticJSON(crdt.StateSnapshot{
		Type:           "ortree-delta",
		ElementCount:   len(d.nodes),
		TombstoneCount: len(d.tombstones),
	})
}
