import RGAFormal.Delta

/-!
# Deterministic RGA sibling order

`text.childIndex` inserts every child by descending canonical position order.
This model isolates that ordering relation: once a run-v2 decoder has validated
the concrete tag comparator, every pair of distinct siblings has exactly one
legal relative order. Parent reachability and the concrete index refinement
remain separate obligations.
-/

namespace DarkInno.RGA

/- `Nat` stands for a previously validated canonical position rank. The
production comparator orders tags by wall time, logical counter, then actor;
that concrete comparator is outside this small algebraic model. -/
structure OrderedNode where
  position : Nat
  parent : Option Nat
  deriving DecidableEq, Repr

/- `comesBefore` is the descending sibling rule used by `childIndex`. -/
def comesBefore (left right : Nat) : Prop := right < left

/- The order never places one position before itself. -/
theorem sibling_order_irreflexive (position : Nat) : ¬comesBefore position position := by
  exact Nat.lt_irrefl position

/- Two distinct sibling positions cannot precede each other simultaneously. -/
theorem sibling_order_asymmetric {left right : Nat}
    (before : comesBefore left right) : ¬comesBefore right left := by
  exact Nat.lt_asymm before

/- Descending sibling order is transitive. -/
theorem sibling_order_transitive {left middle right : Nat}
    (leftMiddle : comesBefore left middle) (middleRight : comesBefore middle right) :
    comesBefore left right := by
  exact Nat.lt_trans middleRight leftMiddle

/- Every two different validated positions have one and only one order. -/
theorem sibling_order_total {left right : Nat} (different : left ≠ right) :
    comesBefore left right ∨ comesBefore right left := by
  rcases Nat.lt_or_gt_of_ne different with leftBeforeRight | rightBeforeLeft
  · exact Or.inr leftBeforeRight
  · exact Or.inl rightBeforeLeft

/- Restricting the total position order to children of one parent preserves its
deterministic choice. -/
theorem same_parent_siblings_have_unique_order (left right : OrderedNode)
    (_sameParent : left.parent = right.parent) (different : left.position ≠ right.position) :
    comesBefore left.position right.position ∨ comesBefore right.position left.position := by
  exact sibling_order_total different

end DarkInno.RGA
