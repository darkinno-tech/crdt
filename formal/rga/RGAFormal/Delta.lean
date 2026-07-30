import Std

/-!
# DarkInno RGA delta algebra

This is an executable Lean model of the portion of DarkInno RGA state that is
merged as a delta: a finite set of retained positions and a finite set of
structural tombstones. It deliberately abstracts away HLC generation, parent
ordering, bounded decoding, and the Go memory layout; see `README.md` for the
proof-to-implementation boundary.
-/

namespace DarkInno.RGA

abbrev Actor := String

/- A `Position` models a stable per-actor RGA identity. -/
structure Position where
  actor : Actor
  counter : Nat
  deriving DecidableEq, Repr

/- A delta carries independently joinable retained positions and tombstones. -/
structure Delta where
  nodes : Finset Position
  tombstones : Finset Position
  deriving Repr

/- State retains structural tombstones even when a corresponding node has not
yet arrived. That is the property that makes delete-before-insert safe. -/
structure State where
  nodes : Finset Position
  tombstones : Finset Position
  deriving Repr

/- `apply` is the delta join used by the model. It has no destructive delete;
compaction is a separately acknowledged lifecycle operation and is out of scope. -/
def apply (state : State) (delta : Delta) : State :=
  { nodes := state.nodes ∪ delta.nodes
    tombstones := state.tombstones ∪ delta.tombstones }

/- A retained node is visible exactly when it has no retained tombstone. -/
def visible (state : State) : Finset Position := state.nodes \ state.tombstones

/- Applying the same partial state twice changes nothing. -/
theorem apply_idempotent (state : State) (delta : Delta) :
    apply (apply state delta) delta = apply state delta := by
  ext position <;> simp [apply, Finset.union_assoc]

/- Arrival order of two independent delta payloads cannot change model state. -/
theorem apply_commutative (state : State) (left right : Delta) :
    apply (apply state left) right = apply (apply state right) left := by
  ext position <;> simp [apply, or_left_comm, or_comm, or_assoc]

/- Grouping a batched delivery cannot change model state. -/
theorem apply_associative (state : State) (first second third : Delta) :
    apply (apply (apply state first) second) third =
      apply (apply state first) { nodes := second.nodes ∪ third.nodes, tombstones := second.tombstones ∪ third.tombstones } := by
  ext position <;> simp [apply, Finset.union_assoc]

/- Tombstones are monotone until an explicit compaction protocol changes the
epoch. This is why a plain state merge cannot accidentally forget a deletion. -/
theorem tombstones_monotone (state : State) (delta : Delta) :
    state.tombstones ⊆ (apply state delta).tombstones := by
  intro position member
  simp [apply, member]

/- A tombstone already known by a replica wins even if a node for the same
position is delivered later in a different delta. -/
theorem deleted_position_never_revives (state : State) (delta : Delta) (position : Position)
    (deleted : position ∈ state.tombstones) :
    position ∉ visible (apply state delta) := by
  simp [visible, apply, deleted]

/- Duplicate, reordered delivery of a finite two-update batch converges to the
same model state. This is the precise algebraic property exercised by the
Go-level shuffled duplicate simulations. -/
theorem duplicate_reordered_converges (state : State) (first second : Delta) :
    apply (apply (apply state first) second) first =
      apply (apply (apply state second) first) second := by
  rw [apply_idempotent, apply_idempotent, apply_commutative]

end DarkInno.RGA
