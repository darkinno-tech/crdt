package text

import "github.com/im10furry/crdt"

// MarshalJSON returns a diagnostic summary for structured logs. It omits text
// content, positions, tombstone identities, and clock state.
func (r *RGA) MarshalJSON() ([]byte, error) { return crdt.MarshalStateJSON(r) }

// MarshalJSON returns a diagnostic summary for structured logs. It omits text
// content, positions, tombstone identities, and clock state.
func (d Delta) MarshalJSON() ([]byte, error) {
	return crdt.MarshalDiagnosticJSON(crdt.StateSnapshot{
		Type:           "rga-delta",
		ElementCount:   len(d.nodes),
		TombstoneCount: len(d.tombstones),
	})
}
