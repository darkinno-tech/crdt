# Server-side AI agents as Yjs peers

`YJSAgentPeer` lets a trusted service-side agent participate in one configured
Level 1 Yjs document through the same durable update path as browser clients.
It is not an LLM integration, a text-patch API, or a Yjs-to-Go conversion
layer. The agent's tool runtime must use a maintained Yjs engine and publish a
normal, format-pinned Yjs update after it has decided what to change.

The pattern is compatible with the collaborative-agent shape demonstrated by
[Electric's collaborative AI editor](https://github.com/electric-sql/collaborative-ai-editor): a server-side agent works through document tools and
writes its result back into the shared Yjs document. This package deliberately
keeps model streaming, chat history, tool policy, and Yjs document durability
as separate concerns.

```text
browser Yjs peers                     trusted agent task
  y-websocket provider                   service identity + policy
          |                                           |
          +-------- authenticated YJSHandler --------+
                            |               ^
                  state-vector/diff         | durable Apply, then fan-out
                            v               |
                    YJSStore sidecar <------+
                    (pinned Yjs engine)
```

## Preconditions and invariants

`OpenYJSAgentPeer` accepts only a configured store-backed room. Its immutable
`Tenant`, `Room`, `Epoch`, `Schema`, and V1/V2 `Format` are the document
identity. An opaque Level 0 relay has bounded live history only, so it cannot
bootstrap a service process with a complete semantic document and is rejected.

The host must provide a service `Peer` identity, for example
`agent:copy-editor:run-7`, after authenticating its task runner. It must select
the room from trusted route/product configuration. Do not derive either value
from a Yjs client ID, a prompt, tool arguments, or document content. A Yjs
client ID is CRDT metadata, not an authenticated actor.

On open and on every `Snapshot` or `Diff`, the handler invokes
`AuthorizeSubscription`. On every `Publish`, it invokes `Authorize` with
`YJSUpdate`. A revoked service account therefore cannot keep using an old
handle. The host's callbacks can audit the service identity, room, operation,
cursor, and allow/deny result, but must not log document bytes or prompts.

`Publish` has one ordering guarantee:

```text
authorize write -> bound update -> YJSStore.Apply -> durable success -> live fan-out
```

A store failure produces no fan-out. `Applied=false` is an idempotent replay
of an already durable Yjs update; it is not a browser receipt, an approval, or
proof that any human has seen the result. An agent peer exposes no awareness
method: y-protocols awareness is connection-owned ephemeral state, not a
durable agent status channel.

## Trusted tool-runtime flow

The model never receives permission to choose a room or submit arbitrary HTTP
requests to the Yjs store. A task runner owns a short-lived, bounded local
`Y.Doc`, invokes narrow product tools such as `propose_rewrite` or
`append_citation`, validates their inputs and scope, and then gives the encoded
update to the application server.

```go
agent, err := handler.OpenYJSAgentPeer(
    extensions.Peer{ID: "agent:copy-editor:run-7"},
    "notes-tenant-a", // selected by trusted application routing
)
if err != nil {
    return err
}

snapshot, err := agent.Snapshot(ctx) // task runner applies this with its pinned Yjs engine
if err != nil {
    return err
}
_ = snapshot // do not place document bytes in logs or an unbounded prompt history

// The trusted Yjs tool runtime applies a bounded, reviewed tool action to its
// local Y.Doc and produces one V1/V2 update matching the configured room.
result, err := agent.Publish(ctx, encodedYjsUpdate)
if err != nil {
    return err // preserve/recover the task outbox; do not invent a receipt
}
if !result.Applied {
    // The exact update was already durable; recover from the returned state vector.
}
```

A fresh task normally begins with `Snapshot`. A bounded long-lived task runner
that retains its local Yjs state should send its state vector to `Diff` instead;
this avoids rereading a full document per model turn. The runner must use the
matching V1 or V2 Yjs API and discard/recover its local document after a tool
or outbound-delivery failure. The library intentionally does not turn model
text into Yjs operations, choose editor schemas, or retain an agent's local
document.

Do not call `YJSStore` directly from the agent service: that would bypass the
room's user-facing authorization and live fan-out. The sidecar bearer token is
only for the trusted Go-to-sidecar hop and must never reach browser code, model
tools, prompts, or logs.

## Product and security boundaries

CRDT convergence resolves concurrent Yjs updates; it does not establish that a
rewrite is desirable, preserves a legal meaning, or has human approval. For
high-impact actions, make the agent create a suggestion/branch, use stable
Yjs-relative positions for the intended range, show the author and scope in
the UI, and require an application-level acceptance action before `Publish`.
For auto-apply actions, give each tool a narrow scope, cap changed bytes and
calls per task, rate-limit service identities, and store an application audit
record outside the document.

Keep chat transcripts, model tokens, tool traces, and agent status outside the
Yjs document unless the product explicitly models them as shared document
state with retention and access rules. A presence label such as "agent is
writing" belongs in a separately authorized, bounded ephemeral channel. It
must not clear an outbox or be treated as durable document delivery.

The existing Yjs limits remain mandatory: raw message/update, state-vector,
sync/snapshot, sidecar request concurrency, timeout, queue, and slow-peer
limits. The model context budget is a separate cap: never equate the sidecar's
maximum snapshot size with an acceptable prompt size. Use `Diff`, chunked
task-local summaries, and product-defined scope before increasing either cap.

## Validation plan

| Scenario | Evidence required |
| --- | --- |
| Unit/mock | Anonymous or opaque-room opens fail; read/write revocation is rechecked; over-limit work never reaches the store; duplicate publish is not re-fanned-out. |
| Real protocol | A real `yjs@13.6.31` sidecar stores an agent update; a fresh standard `y-websocket` client with BroadcastChannel disabled recovers it through the relay's state-vector handshake. |
| Fault/recovery | Sidecar unavailable, invalid/wrong-format update, process restart, cancellation, duplicate retry, and a stale agent vector leave the last durable snapshot recoverable. |
| Controlled performance | Measure durable apply/diff/snapshot at 1/4/16/64 receivers and report p50/p95/p99, diff bytes, CPU, heap, and queue drops. These local measurements do not establish WAN, TLS, model, authorization-database, or production fan-out capacity. |
| Live acceptance | Use target document shapes, authenticated gateway, TLS, actual browser/editor bindings, slow receivers, task retry/outbox recovery, authorization revocation, and a model/tool evaluation set before enabling auto-apply. |

See [the Level 1 store guide](yjs-store.md) for sidecar deployment and
[the Yjs interoperability decision](../design/yjs-deeper-interoperability.md)
for the non-negotiable Yjs/Go protocol boundary.
