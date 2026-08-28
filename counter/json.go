package counter

import "github.com/darkinno-tech/crdt"

// MarshalJSON returns a diagnostic summary for structured logs. It omits
// per-replica components and cannot be used to restore counter state.
func (c *GCounter) MarshalJSON() ([]byte, error) { return crdt.MarshalStateJSON(c) }

// MarshalJSON returns a diagnostic summary for structured logs. It omits
// per-replica components and cannot be used to restore counter state.
func (c *PNCounter) MarshalJSON() ([]byte, error) { return crdt.MarshalStateJSON(c) }

// MarshalJSON returns a diagnostic summary for structured logs. It omits
// per-replica components and cannot be applied as a delta from JSON.
func (d GCounterDelta) MarshalJSON() ([]byte, error) {
	return crdt.MarshalDiagnosticJSON(crdt.StateSnapshot{
		Type:         "gcounter-delta",
		ElementCount: len(d.counts),
	})
}

// MarshalJSON returns a diagnostic summary for structured logs. It omits
// per-replica components and cannot be applied as a delta from JSON.
func (d PNCounterDelta) MarshalJSON() ([]byte, error) {
	return crdt.MarshalDiagnosticJSON(crdt.StateSnapshot{
		Type:         "pncounter-delta",
		ElementCount: len(d.positive) + len(d.negative),
	})
}
