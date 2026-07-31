package documenttree

import "github.com/DarkInno/crdt"

// MarshalJSON returns a payload-free diagnostic summary.
func (d *Document) MarshalJSON() ([]byte, error) { return crdt.MarshalStateJSON(d) }

// MarshalJSON returns a payload-free diagnostic summary for a delta.
func (d Delta) MarshalJSON() ([]byte, error) {
	return crdt.MarshalDiagnosticJSON(crdt.StateSnapshot{
		Type:           "document-tree-delta",
		ElementCount:   len(d.state.objects),
		TombstoneCount: countTombstones(d.state),
	})
}
