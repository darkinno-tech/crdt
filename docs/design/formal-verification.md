# Formal verification scope and roadmap

DarkInno now carries a checkable Lean model in
[`formal/rga`](../../formal/rga). It starts from the invariant with the highest
correctness risk: a structural RGA tombstone must survive out-of-order delivery
and must not be silently forgotten by a state merge.

The initial model proves idempotent, commutative and associative delta join,
tombstone monotonicity, delete-before-insert non-revival, and a finite
duplicate/reordered convergence theorem. The current extension also checks a
strict sibling-order relation, a parsed run-v2 envelope refinement, and an
atomic HLC/snapshot recovery state machine. They are intentionally **abstract
models**, not proofs of the Go implementation. The checked command and exact
scope are documented beside the Lean source.

## Why this boundary first

This repository has stable framed protocols and local-only APIs. A proof that
ignores those boundaries would give a false
sense of security. The first formal surface is therefore only the stable RGA
state algebra; parser limits, authentication, relay policy, checkpoint
durability and garbage-collection acknowledgement remain explicit external
assumptions.

| Layer | Current evidence | Formal status |
| --- | --- | --- |
| Delta set/tombstone merge | Lean `Delta.lean` | machine-checked abstract proof |
| RGA sibling order | unit, fuzz and shuffled simulations | Lean strict-order proof; parent-index and pending-parent refinement remain pending |
| run-v2 frame codec and limits | decoder tests and fuzz | Lean parsed-envelope refinement; byte parser, CRC and limits remain pending |
| HLC and snapshot recovery | recovery/race tests | Lean atomic-record state machine; Go/provider durability refinement remains pending |
| Structural tombstone compaction | exact-ack/checkpoint tests | pending epoch/receipt proof |
| Provider authorization and awareness | authenticated integration tests | security policy, not CRDT algebra |

`lean-yjs` is valuable precedent: Yjs documents that its model has proofs for
preservation and commutativity, while its community discussion notes the effort
is still moving from algorithmic modeling toward code-adjacent verification.
DarkInno will use the same discipline: publish exactly the completed theorem
and its abstraction boundary, never a blanket “formally verified” claim.

## Checked command and gate proposal

Run `make formal-rga` to check the pinned Lean model files. It intentionally
does not run from `make verify`: installing a new toolchain on a required Go
gate would weaken the existing supply-chain boundary.

The pinned Lean command should become a separate CI job only after its
toolchain bootstrap is itself pinned and reviewed. The project already pins
the Lean version, enabling an isolated deterministic job once the repository's
action/toolchain pinning policy is agreed.
