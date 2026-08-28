package register

import "github.com/darkinno-tech/crdt"

// MarshalJSON returns a diagnostic summary for structured logs. It omits the
// opaque value, HLC tag, and clock state, and cannot restore the register.
func (r *LWW) MarshalJSON() ([]byte, error) { return crdt.MarshalStateJSON(r) }

// MarshalJSON returns a diagnostic summary for structured logs. It omits the
// register value and cannot restore the register.
func (r *Max) MarshalJSON() ([]byte, error) { return crdt.MarshalStateJSON(r) }

// MarshalJSON returns a diagnostic summary for structured logs. It omits
// values, causal context, and dots, and cannot restore the register.
func (r *MVRegister) MarshalJSON() ([]byte, error) { return crdt.MarshalStateJSON(r) }

// MarshalJSON returns a diagnostic summary for structured logs. It omits
// values, causal context, and dots and cannot be applied as a delta from JSON.
func (d MVRegisterDelta) MarshalJSON() ([]byte, error) {
	return crdt.MarshalDiagnosticJSON(crdt.StateSnapshot{
		Type:         "mv-register-delta",
		ElementCount: len(d.values),
	})
}
