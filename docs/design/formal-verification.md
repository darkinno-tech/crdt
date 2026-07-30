# Formal verification scope and roadmap

DarkInno now carries a checkable Lean model in
[`formal/rga`](../../formal/rga). It starts from the invariant with the highest
correctness risk: a structural RGA tombstone must survive out-of-order delivery
and must not be silently forgotten by a state merge.

The initial model proves idempotent, commutative and associative delta join,
tombstone monotonicity, delete-before-insert non-revival, and a finite
duplicate/reordered convergence theorem. It is intentionally an **abstract
model**, not a proof of the Go implementation. The checked command and exact
scope are documented beside the Lean source.

## Why this boundary first

This repository has stable framed protocols, experimental framed protocols,
and local-only APIs. A proof that ignores those boundaries would give a false
sense of security. The first formal surface is therefore only the stable RGA
state algebra; parser limits, authentication, relay policy, checkpoint
durability and garbage-collection acknowledgement remain explicit external
assumptions.

| Layer | Current evidence | Formal status |
| --- | --- | --- |
| Delta set/tombstone merge | Lean `Delta.lean` | machine-checked abstract proof |
| RGA sibling order and pending parents | unit, fuzz and shuffled simulations | pending graph refinement proof |
| run-v2 frame codec and limits | decoder tests and fuzz | pending parser refinement proof |
| HLC and snapshot recovery | recovery/race tests | pending state-machine proof |
| Structural tombstone compaction | exact-ack/checkpoint tests | pending epoch/receipt proof |
| Provider authorization and awareness | authenticated integration tests | security policy, not CRDT algebra |

`lean-yjs` is valuable precedent: Yjs documents that its model has proofs for
preservation and commutativity, while its community discussion notes the effort
is still moving from algorithmic modeling toward code-adjacent verification.
DarkInno will use the same discipline: publish exactly the completed theorem
and its abstraction boundary, never a blanket “formally verified” claim.

## Gate proposal

The pinned Lean command has been checked locally with a temporary
`leanprover/lean4:v4.31.0` toolchain. It should become a separate CI job after
the first graph refinement is reviewed. It is kept out of the current required
Go gate because an unpinned network-installed CI bootstrap would weaken the
existing supply-chain policy. The project itself already pins the Lean
toolchain, enabling an isolated deterministic job once the repository's
action/toolchain pinning policy is agreed.
