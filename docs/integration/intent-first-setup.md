# Intent-first CRDT setup

[English](intent-first-setup.md) | [简体中文](intent-first-setup.zh-CN.md)

A CRDT is a declared merge rule, not a replacement for product authority. This
guide starts with the fact the product needs to share, then makes the chosen
rule inspectable by both a developer and a tool before any TypeID is copied
into a manifest.

## 1. Decide whether the fact belongs in a CRDT

Use an authoritative service instead of a CRDT when accepting concurrent
offline changes could violate an invariant. Typical examples are balances,
inventory reservations, exclusive bookings, workflow transitions, access
control, and identity decisions.

For an eventually consistent fact, choose the smallest rule that matches the
business meaning:

| Question | Profile to inspect | Concurrent outcome |
| --- | --- | --- |
| Can every replica only increase its own contribution? | `counter/grow-only` | Increments accumulate. |
| Can an offline member be independently added or removed? | `set/add-wins` | A concurrent add remains present. |
| Must concurrent field writes stay visible for product review? | `register/multi-value` | All causally concurrent values remain. |
| Is this a new plain collaborative text document? | `text/run-v2` | Inserts order deterministically and deletes retain anchors. |
| Does the document need a fixed declared hierarchy of child CRDTs? | `document/tree-v1` | Declared child operations merge under its own protocol. |

The complete list also includes LWW, legacy text, ordered-list, move-list,
observed-remove tree, and rich-text profiles. A profile makes the conflict rule
and unsafe use cases visible; it does not make a security or capacity decision.

## 2. Ask the profile catalog instead of guessing protocol IDs

The root package exposes immutable copies of the profiles. Their IDs are
case-sensitive so a typo never silently selects a different merge rule.

```go
profile, ok := crdt.ReplicationProfileFor("text/run-v2")
if !ok {
	return errors.New("unknown CRDT profile")
}

for _, requirement := range profile.HostRequirements {
	log.Println(requirement)
}
```

`profile.FrameType` is the canonical state ID, delta ID, semantics version,
and HLC flag from this release's closed protocol registry. It is metadata only:
do not accept a frame because a profile mentions it.

For a human-readable terminal view, run:

```sh
go run ./cmd/crdt-profile -id text/run-v2
```

For a deterministic machine-readable handoff to a code generator, review bot,
or AI-assisted configuration flow, run:

```sh
go run ./cmd/crdt-profile -id text/run-v2 -format json
```

The JSON describes merge semantics and remaining host responsibilities. It
contains no credentials, transport endpoint, production limit, or permission
grant. Treat it as an input to design review, not a deployable configuration.

## 3. Build a manifest without hand-copying protocol fields

After the product and security review has selected the profile, convert its
canonical `FrameType` through the replica helper. The helper rejects a partial
or altered type pair, so an old delta ID cannot accidentally be combined with
a new semantics version.

```go
profile, ok := crdt.ReplicationProfileFor("text/run-v2")
if !ok {
	return errors.New("unknown CRDT profile")
}

builder, err := replica.NewSessionBuilderForFrameType(
	"notes-42",                         // application group ID
	"example.com/notes/plain-text/v1",  // application schema ID
	1,                                  // membership/contract epoch
	profile.FrameType,
	"", // codec ID; use a deterministic, versioned ID when profile.RequiresCodecID
	crdt.ProtocolPolicy{},
)
if err != nil {
	return fmt.Errorf("create manifest: %w", err)
}
manifest := builder.Manifest()
```

This is equivalent to creating `replica.Protocol` and calling
`replica.NewSessionBuilder`, but avoids repeating three protocol fields. It
does **not** authenticate `manifest`; complete the authenticated exact-manifest
comparison before a sender can publish or subscribe.

Run the full local example, which selects `counter/grow-only`, constructs its
manifest from the profile, validates a manifest-bound delta, and performs a
bounded duplicate delivery:

```sh
(cd examples && go run ./intent-first-setup)
# profile=counter/grow-only
# state_type=1
# delta_type=3
# value=3
```

## 4. Keep the missing boundaries explicit

Every profile leaves these responsibilities with the integrating service:

1. Authenticate the peer and compare one exact manifest before accepting a
   frame. A checksum, TypeID, profile, or successful gRPC call is not peer
   identity or authorization.
2. Apply transport body limits before decoding, then use the concrete
   `Unmarshal*WithLimits` decoder before mutation. Profile metadata has no
   default byte, element, tag, queue, or retention budget.
3. Authorize both the operation and its values. A convergent merge rule does
   not authorize an increment, grant a role, or reserve inventory.
4. Persist the profile's HLC or causal recovery state atomically with the
   concrete CRDT state, delivery frontier, and outbox before reusing an ID.
5. Treat retention and tombstone compaction as an authenticated membership
   protocol, never as a local cleanup optimization.

The profile command and example are intentionally offline and deterministic.
For a real HTTP duplicate-delivery exercise, recovery, and anti-entropy
acceptance criteria, continue with the [end-to-end integration tutorial](overview.md).
