import Std

/-!
# DarkInno RGA delta algebra

This is an executable Lean model of the portion of DarkInno RGA state that is
merged as a delta: retained positions and structural tombstones. It
deliberately abstracts away HLC generation, parent ordering, bounded decoding,
and the Go memory layout; see `README.md` for the proof-to-implementation
boundary.

Lean 4.31 no longer ships `Finset` in `Std`, so this model uses extensional
sets. That strengthens the algebraic join statements while intentionally
abstracting the production finite-resource limits to Go validation and fuzzing.
-/

namespace DarkInno.RGA

abbrev Actor := String

/- A `Position` models a stable per-actor RGA identity. -/
structure Position where
  actor : Actor
  counter : Nat
  deriving DecidableEq, Repr

/- An extensional set avoids an unpinned third-party proof dependency. -/
abbrev PositionSet := Position → Prop

def union (left right : PositionSet) : PositionSet := fun position => left position ∨ right position

/- A delta carries independently joinable retained positions and tombstones. -/
structure Delta where
  nodes : PositionSet
  tombstones : PositionSet

/- State retains structural tombstones even when a corresponding node has not
yet arrived. That is the property that makes delete-before-insert safe. -/
structure State where
  nodes : PositionSet
  tombstones : PositionSet

/- `apply` is the delta join used by the model. It has no destructive delete;
compaction is a separately acknowledged lifecycle operation and is out of scope. -/
def apply (state : State) (delta : Delta) : State :=
  { nodes := union state.nodes delta.nodes
    tombstones := union state.tombstones delta.tombstones }

/- A retained node is visible exactly when it has no retained tombstone. -/
def visible (state : State) (position : Position) : Prop :=
  state.nodes position ∧ ¬state.tombstones position

theorem state_ext (left right : State)
    (nodes : ∀ position, left.nodes position ↔ right.nodes position)
    (tombstones : ∀ position, left.tombstones position ↔ right.tombstones position) : left = right := by
  cases left
  cases right
  congr
  · funext position
    apply propext
    exact nodes position
  · funext position
    apply propext
    exact tombstones position

/- Applying the same partial state twice changes nothing. -/
theorem apply_idempotent (state : State) (delta : Delta) :
    apply (apply state delta) delta = apply state delta := by
  apply state_ext <;> intro position <;> simp [apply, union]

/- Arrival order of two independent delta payloads cannot change model state. -/
theorem apply_commutative (state : State) (left right : Delta) :
    apply (apply state left) right = apply (apply state right) left := by
  apply state_ext <;> intro position <;> simp [apply, union, or_left_comm, or_comm, or_assoc]

/- Grouping a batched delivery cannot change model state. -/
theorem apply_associative (state : State) (first second third : Delta) :
    apply (apply (apply state first) second) third =
      apply (apply state first) { nodes := union second.nodes third.nodes, tombstones := union second.tombstones third.tombstones } := by
  apply state_ext <;> intro position <;> simp [apply, union, or_assoc]

/- Tombstones are monotone until an explicit compaction protocol changes the
epoch. This is why a plain state merge cannot accidentally forget a deletion. -/
theorem tombstones_monotone (state : State) (delta : Delta) (position : Position)
    (member : state.tombstones position) :
    (apply state delta).tombstones position := by
  exact Or.inl member

/- A tombstone already known by a replica wins even if a node for the same
position is delivered later in a different delta. -/
theorem deleted_position_never_revives (state : State) (delta : Delta) (position : Position)
    (deleted : state.tombstones position) :
    ¬visible (apply state delta) position := by
  simp [visible, apply, union, deleted]

/- Duplicate, reordered delivery of a finite two-update batch converges to the
same model state. This is the precise algebraic property exercised by the
Go-level shuffled duplicate simulations. -/
theorem duplicate_reordered_converges (state : State) (first second : Delta) :
    apply (apply (apply state first) second) first =
      apply (apply (apply state second) first) second := by
  apply state_ext <;> intro position <;> simp [apply, union, or_left_comm, or_comm, or_assoc]

end DarkInno.RGA
