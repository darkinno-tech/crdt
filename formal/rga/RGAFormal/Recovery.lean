import RGAFormal.Delta

/-!
# HLC and snapshot recovery state machine

The model treats one durable recovery record as an atomic unit: RGA state,
the local HLC state, and the application delivery frontier.  It does not model
filesystem ordering, provider transactions, wall-clock APIs, or Go integer
overflow; those are runtime and integration obligations.
-/

namespace DarkInno.RGA.Recovery

structure Clock where
  wallTime : Nat
  logical : Nat
  deriving DecidableEq, Repr

/- `tick` models a local HLC event after the caller has sampled physical time.
When physical time is not ahead, the logical component advances. -/
def tick (physical : Nat) (clock : Clock) : Clock :=
  if clock.wallTime < physical then
    { wallTime := physical, logical := 0 }
  else
    { wallTime := clock.wallTime, logical := clock.logical + 1 }

/- A recovery record has no independently durable state, clock, or frontier
field.  This is the modelled atomic-persistence boundary. -/
structure Snapshot where
  state : State
  clock : Clock
  frontier : PositionSet

/- The live machine can diverge from its last durable record while accepting
updates.  Only `commit` advances the crash-recovery point. -/
structure Machine where
  live : Snapshot
  durable : Snapshot

def evolve (machine : Machine) (next : Snapshot) : Machine :=
  { machine with live := next }

def commit (machine : Machine) : Machine :=
  { machine with durable := machine.live }

def recover (machine : Machine) : Machine :=
  { live := machine.durable, durable := machine.durable }

/- A crash before an atomic commit recovers the complete previous record; it
cannot combine a new RGA state with an old HLC/frontier in this model. -/
theorem recover_before_commit (machine : Machine) (next : Snapshot) :
    (recover (evolve machine next)).live = machine.durable := by
  rfl

/- A completed commit recovers the complete new record, including its HLC and
frontier. -/
theorem recover_after_commit (machine : Machine) (next : Snapshot) :
    (recover (commit (evolve machine next))).live = next := by
  rfl

/- Recovery preserves every component of one committed record together. -/
theorem recover_preserves_clock (machine : Machine) :
    (recover machine).live.clock = machine.durable.clock := by
  rfl

theorem recover_preserves_state (machine : Machine) :
    (recover machine).live.state = machine.durable.state := by
  rfl

theorem recover_preserves_frontier (machine : Machine) :
    (recover machine).live.frontier = machine.durable.frontier := by
  rfl

/- A local tick never moves the wall-time component backwards. -/
theorem tick_wall_monotone (physical : Nat) (clock : Clock) :
    clock.wallTime ≤ (tick physical clock).wallTime := by
  by_cases advances : clock.wallTime < physical
  · have preserved : clock.wallTime ≤ physical := Nat.le_of_lt advances
    simpa [tick, advances] using preserved
  · simp [tick, advances]

/- When physical time does not advance, the logical component advances by one.
This is the portion of HLC freshness that snapshot recovery must retain. -/
theorem tick_logical_when_physical_stale (physical : Nat) (clock : Clock)
    (stale : physical ≤ clock.wallTime) :
    (tick physical clock).wallTime = clock.wallTime ∧
      (tick physical clock).logical = clock.logical + 1 := by
  have notLess : ¬ clock.wallTime < physical := Nat.not_lt_of_ge stale
  unfold tick
  simp [notLess]

end DarkInno.RGA.Recovery
