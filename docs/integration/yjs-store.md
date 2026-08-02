# Level 1 Yjs semantic store

`yjsstore/runtime` is the Level 1 companion to the bounded Level 0
`extensions.YJSHandler` relay. It runs the maintained Yjs engine and provides
state-vector diff, V1/V2 update merge, duplicate-safe apply, and durable
snapshot recovery. It does not translate a Yjs document into this repository's
Go RGA, rich-text, manifest, or awareness protocols.

## Boundary and contract

```text
Yjs client / y-websocket
  -> authenticated YJSHandler room
       -> YJSStore Go client (bounded local HTTP)
            -> yjsstore/runtime (pinned Yjs engine)
                 -> 0700 data directory, atomic snapshot record
```

The application creates a `YJSDocument` with a stable `Tenant`, `Room`,
`Epoch`, `Schema`, and format. These five values are persisted with the
snapshot and form its storage key. Incrementing `Epoch` creates a new Yjs
history; it must not be used to reset a document in place.

| Operation | Semantic result | Durable effect |
| --- | --- | --- |
| `Apply(update)` | Applies one format-pinned Yjs update; a retry reports `Applied=false`. | A changed document is materialized and atomically stored before success. |
| `StateVector()` | Returns the current Yjs state vector. | Reads the same persisted snapshot used for recovery. |
| `Diff(remoteVector)` | Returns Yjs's missing update for that vector. | Read-only. |
| `Snapshot()` | Returns a merged recovery update, state vector, and cursor observed together. | Read-only. |
| `Merge(updates)` | Uses Yjs V1 or V2 merge helpers. | Read-only; it does not modify a document. |

The sidecar starts a fresh `Y.Doc` with Yjs GC enabled for each durable
operation and persists the materialized update, rather than retaining an
unbounded update log. That is a recovery snapshot under the selected Yjs
version and GC behavior; it is not permission to mix formats or epochs.

## Start the sidecar

Install exactly the committed lockfile, provide the secret through a secret
manager or service environment, and keep data outside the checkout:

```sh
npm --prefix yjsstore/runtime ci --ignore-scripts
install -d -m 700 /var/lib/example/yjs-store
export YJS_STORE_DATA_DIR=/var/lib/example/yjs-store
export YJS_STORE_TOKEN='at-least-32-bytes-from-your-secret-manager'
export YJS_STORE_HOST=127.0.0.1
export YJS_STORE_PORT=8080
export YJS_STORE_MAX_UPDATE_BYTES=1048576
export YJS_STORE_MAX_STATE_VECTOR_BYTES=65536
export YJS_STORE_MAX_SNAPSHOT_BYTES=16777216
export YJS_STORE_MAX_MERGE_UPDATES=256
export YJS_STORE_MAX_CONCURRENT_REQUESTS=4
export YJS_STORE_REQUEST_TIMEOUT_MS=10000
node --no-experimental-webstorage yjsstore/runtime/server.mjs
```

The data directory must be a non-symlink `0700` directory. Each record is
written as a `0600` temporary file, fsynced, renamed, and followed by a
directory fsync. The runtime checks this directory both while loading its
configuration and immediately before it starts listening, so embedding code
cannot bypass the boundary by constructing a server directly or by changing
permissions between loading and listening. A checksum detects accidental
corruption; it is not an encryption or a defense against an attacker that can
already write the data directory. Use encrypted storage and OS/container
isolation when that threat exists.

The bundled Node runtime is deliberately not a public service: it has one
bearer token, adds no CORS policy, and accepts only the literal loopback
listeners `127.0.0.1` and `::1`. A remote deployment needs a separately
maintained `YJSStore` implementation with TLS/mTLS, network authorization,
rate controls, secret rotation, and an application-owned service boundary. Do
not expose its token to browsers. The Go client also rejects every HTTP
redirect: its configured endpoint is a bearer-token trust boundary, not a
service-discovery URL.

The sidecar admits at most `YJS_STORE_MAX_CONCURRENT_REQUESTS` active HTTP
requests (default `4`, range `1..64`) before application code starts collecting
their bodies. A request beyond that budget receives `503 {"code":"unavailable"}`
and makes no durable change; the caller must use bounded backoff and state-vector
recovery rather than retrying an editor mutation. Set this value to the
intentional maximum concurrent durable workload--for example, a controlled
16-writer test sets it to `16`--and size the Node heap/container for that many
materialized Yjs documents. It is not a substitute for gateway rate limits.

`YJS_STORE_REQUEST_TIMEOUT_MS` defaults to 10 seconds and accepts `1000..120000`.
The server also limits incomplete headers to the smaller of that value and five
seconds, then checks incomplete connections every second. A timed-out partial
body receives Node's `408` response and releases its admission slot without
reaching Yjs or the durable record. Set the timeout only after measuring the
chosen local disk, update size, and Go-client deadline; do not disable it.

Run exactly one bundled runtime process for each data directory. Its keyed
lock serializes requests within that process; independent processes sharing a
directory can otherwise each materialize a valid but stale snapshot and lose a
concurrent writer through last-rename-wins replacement. For high availability,
partition each document to one writer or supply a durable store implementation
with cross-process serialization.

## Mount a store-backed room

The limits below must be compatible in all three layers. `MaxSyncBytes` covers
the server's response to an empty client state vector, so it normally equals
the sidecar snapshot limit; it can be larger than one client update.

```go
import "time"

store, err := extensions.NewYJSStore(extensions.YJSStoreConfig{
    Endpoint:            "http://127.0.0.1:8080",
    Token:               sidecarToken, // never log or commit this value
    MaxUpdateBytes:      1 << 20,
    MaxStateVectorBytes: 64 << 10,
    MaxSnapshotBytes:    16 << 20,
    MaxMergeUpdates:     256,
})
if err != nil { return err }

room, err := extensions.NewYJSRoom(extensions.YJSRoomConfig{
    Name:                "notes-tenant-a",
    MaxUpdateBytes:      1 << 20,
    MaxStateVectorBytes: 64 << 10,
    MaxSyncBytes:        16 << 20,
    Store:               store,
    Document: extensions.YJSDocument{
        Tenant: "tenant-a",
        Room:   "notes",
        Epoch:  7,
        Schema: "prosemirror-v1",
        Format: extensions.YJSStoreFormatV1,
    },
})
if err != nil { return err }

handler, err := extensions.NewYJSHandler(extensions.YJSConfig{
    Rooms:                 []*extensions.YJSRoom{room},
    Authenticate:          authenticateRequest,
    AuthorizeSubscription: authorizeRead,
    Authorize:             authorizePublish,
    OriginPatterns:        []string{"app.example.com"},
    MaxMessageBytes:       (16 << 20) + 30, // sync envelope reserve
    MaxQueuedMessages:     64,
    MaxQueuedBytes:        (16 << 20) + 30,
    MaxAwarenessClients:   256,
    StoreTimeout:          5 * time.Second,
})
if err != nil { return err }
```

The handler authenticates and authorizes a browser connection before calling
the sidecar. It sends the durable state vector as sync Step 1 when that
connection starts; on a client Step 1 it sends the sidecar's diff as Step 2.
A client Step 2 or update is durably applied before being fanned out. Awareness
stays in the Go room and remains ephemeral.

One store-backed `YJSDocument` maps to exactly one configured `YJSRoom` in a
handler. Construction rejects a second room with the same tenant, room, epoch,
schema, and format because two live peer sets could otherwise persist to one
document while failing to fan out each other's updates in real time.

`YJSStoreFormatV1` is the compatible choice for the standard y-websocket
provider. V2 needs an explicitly negotiated provider/adapter that emits V2
updates. A store rejects the other encoding instead of guessing based on a
payload that happens to parse.

## Operational limits and recovery

- Limit raw HTTP body, decoded update, state vector, merged snapshot, and
  merge fan-in. The sidecar checks each boundary before base64 decode or Yjs
  invocation, then checks the materialized snapshot before durable replacement.
- Each operation materializes one request-scoped `Y.Doc` from the durable
  snapshot and destroys it on every success or failure path. This prevents
  sidecar-owned Yjs observers or subdocument references accumulating across a
  sustained apply, diff, state-vector, or snapshot workload; it does not make
  an untrusted update's decoded allocation cost safe by itself.
- Set a Node heap/container memory ceiling and ingress rate limit as well.
  A raw byte cap limits but cannot prove a Yjs update's decoded structure has a
  low allocation cost.
- Keep sidecar request admission and receive timeouts bounded. The active
  request limit covers application-level body collection and Yjs
  materialization; excess work receives `unavailable` before its body is
  collected. The HTTP receive deadline releases a partially uploaded request.
  Choose both limits together with the maximum snapshot size and the Go
  client's deadline.
- Treat a sidecar `unavailable` or `corrupt_store` error as a failed durable
  operation. Do not relay that update optimistically or advance an application
  outbox cursor.
- Back up the data directory through a filesystem snapshot or while the
  sidecar is stopped. Restore it to the same Yjs version and document identity,
  then verify `Snapshot`, `StateVector`, and an official client rendering
  before reopening writes.
- Revoke access at the gateway and close active room connections. The sidecar
  token only authenticates the trusted Go-to-sidecar hop; it is not end-user
  authorization.

## Validation and benchmarks

```sh
make yjs-store-test
make yjs-store-benchmark
```

`yjs-store-test` runs direct real-Yjs V1/V2, nested shared-type, state-vector,
merge, restart, duplicate, concurrent-writer, malformed-input, corrupted
record, permission-drift, saturated-request, and partial-body-timeout
scenarios. It then starts the Node sidecar and verifies the Go HTTP client
against it. `yjs-store-benchmark` measures a loopback durable apply,
state-vector diff, and snapshot workload; it is not a TLS, WAN, browser,
authorization, or fan-out capacity claim.
