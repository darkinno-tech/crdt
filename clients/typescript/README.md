# TypeScript CRDT client: native shared types, frame decoder, and RGA Wasm

This directory gives browsers and JavaScript/WebView clients two deliberately
separate local-merge paths:

1. `src/native.ts` is a dependency-free TypeScript document with `NativeMap`,
   `NativeArray`, and plain-text `NativeText` shared types. It avoids Wasm download/startup overhead for
   structured application state where all peers select its native TS contract.
2. `src/frame.ts` is a dependency-free TypeScript decoder for the canonical
   v1 outer frame. It validates magic, version, shortest varints, lengths and
   CRC-32C under explicit limits. It returns opaque codec bytes and does not
   treat a checksum as authentication.
3. `cmd/crdt-rga-wasm` compiles the existing Go RGA implementation to Wasm.
   The default artifact uses compact run-v2 frames, matching new Go RGA
   groups. Local edits produce canonical delta frames; incoming frames go
   through the same bounded Go decoder and merge semantics as server-side Go.
4. `../rust` is a complete native Rust run-v2 client. Its owned-buffer C ABI
   is the implementation used by the checked-in Python and Swift bindings.
   It is the native alternative for hosts that cannot use Go/Wasm.

`NativeDocument` is not a substitute implementation of Go RGA run-v2. Its
canonical UTF-8 JSON updates are called **`native-ts-v1`** in this guide, have
no `FrameType`, and must never be passed to `decodeFrame`, a Go frame decoder,
or an existing Go replication group. For a group that must interoperate with
Go, native mobile, or prior RGA data, retain the negotiated Wasm path below.

For native desktop/server/mobile hosts that need the same TypeID `19/20`
semantics without embedding Go, use the [Rust client](../rust/README.md) and
its [multilanguage design](../../docs/design/native-multilanguage-rga.md).

## Native TypeScript shared types

The native layer provides the browser-facing shared-map/shared-array model,
without claiming Yjs API or wire compatibility:

- `document.getMap(name)` returns a LWW `NativeMap`. A set/delete carries an
  immutable `{ actor, counter }` ID; concurrent writes choose the greatest
  counter then raw UTF-8 actor bytes. Deletes are retained tombstones.
- `document.getArray(name)` returns an RGA `NativeArray`. Inserts point to a
  left neighbour, concurrent siblings use the same deterministic ID ordering,
  and deletes retain structural tombstones. Parent-missing inserts wait in a
  bounded queue; delete-before-insert is supported.
- `document.getText(name)` returns a plain-text RGA `NativeText`. Its
  `length`, `insert(index, content)`, and `delete(index, length)` positions are
  UTF-16 code units, matching browser editor and JavaScript string APIs;
  `toString()` returns the current text. Every wire entry is one Unicode scalar
  and the client rejects a position that would split a surrogate pair.
- `document.transact()` emits one local update after a group of mutations.
  `onUpdate()` supplies transport-ready operations and `observe()` on a type
  runs after the transaction has been applied.
- `applyUpdate()` is transport-agnostic and validates the entire update before
  mutation. It accepts reordered and duplicate operations. `encodeNativeUpdate`
  and `decodeNativeUpdate` use canonical JSON for byte transports.
- `getStateVector()` returns a bounded sparse (dotted) state vector, while
  `encodeStateAsUpdates(peerVector)` sends only state the peer vector proves
  it lacks. `encodeNativeStateVector` / `decodeNativeStateVector` provide a
  canonical byte transport for that summary.

```ts
import { decodeNativeUpdate, encodeNativeUpdate, NativeDocument } from "@darkinno/crdt-client/native";

const alice = new NativeDocument("alice-device-7");
const metadata = alice.getMap("metadata");
const cards = alice.getArray("cards");
const body = alice.getText("body");

alice.onUpdate(({ update }) => {
  // Authenticate and authorize this message at the transport boundary first.
  socket.send(encodeNativeUpdate(update));
});

alice.transact(() => {
  metadata.set("title", "Roadmap");
  cards.push([{ id: "card-1", title: "Draft" }]);
  body.insert(0, "Draft notes");
}, "local-editor");

socket.onmessage = ({ data }) => {
  alice.applyUpdate(decodeNativeUpdate(new Uint8Array(data)), "remote-peer");
};
```

`NativeText` requires the separately declared `native-ts-text-v1` semantic
capability. It is plain text only: formatting, embeds, rich-text Delta APIs,
and Go/Wasm RGA interoperability are intentionally out of scope. Do not send a
Text-root update to a native peer that has not negotiated this capability; it
will fail closed on the unknown operation. Use the negotiated Go/Wasm rich-text
runtime for document bodies requiring marks, blocks, or embeds.

### Native state-vector anti-entropy

For a **previously authenticated and document/schema-bound** peer, exchange a
state vector before requesting a bounded state repair. The vector is a sparse
set of actor-counter ranges, not a conventional actor-to-maximum map. Native
updates can arrive out of order and a rejected local admission may leave a
counter unused; representing only a maximum could falsely claim a missing
operation was known and make a repair omit it.

```ts
import {
  decodeNativeStateVector,
  encodeNativeStateVector,
} from "@darkinno/crdt-client/native";

const peerVector = decodeNativeStateVector(await receiveAuthenticatedVector());
for (const update of alice.encodeStateAsUpdates(peerVector)) {
  await sendAuthenticatedUpdate(encodeNativeUpdate(update));
}
await sendAuthenticatedVector(encodeNativeStateVector(alice.getStateVector()));
```

The vector is an optimization only: it is neither membership evidence nor a
durable receipt. Array deletes have no separate immutable dot, so state repair
intentionally re-sends retained array tombstones even when a peer knows the
insertion ID. New snapshots and browser metadata store the vector atomically
with roots, updates, and the local counter; legacy snapshots without one are
accepted but conservatively know only IDs present in their retained state.

Values are copied JSON values (`null`, booleans, finite numbers, strings,
arrays, and plain objects). They are intentionally **atomic**: this first
native version does not support nested shared types or merge a mutated nested
object field-by-field. Model independently collaborative structures as named
root `NativeMap`/`NativeArray` values instead. This avoids invisible in-place
mutation and keeps resource accounting explicit.

For a replication group that explicitly negotiates `native-ts-nested-v1`, use
`NativeNestedDocument` from `@darkinno/crdt-client/nested`. It preserves the
atomic-v1 API while allowing a map or array entry to own one independently
merged child `NativeNestedMap`/`NativeNestedArray`:

```ts
import { NativeNestedDocument } from "@darkinno/crdt-client/nested";

const document = new NativeNestedDocument("alice-device-7");
const board = document.getMap("board");
const card = board.createArray("cards").pushMap();
card.set("title", "Draft");
card.createArray("labels").push(["planning"]);
```

The child reference is bound to its immutable parent-operation ID, so it has
one owner and cannot be moved or aliased. Nested updates that arrive before a
parent wait under an explicit limit; snapshots reject while unresolved. This
is a new semantics contract, not Yjs compatibility, a Go frame TypeID, or a
transparent upgrade for a `NativeDocument` peer. See the
[nested-type design](../../docs/design/native-typescript-nested-types.md).

### Native collection bindings

`@darkinno/crdt-client/collections` adds bounded Counter, Set, LWW register,
and Tree views over a private `native-ts-v1` root namespace. Their negotiated
semantic identifier is **`native-ts-collections-v1`**. The transport update is
still canonical `native-ts-v1` JSON, but a peer must declare the same logical
root names and types before applying it through `NativeCollectionsDocument`.
Do not pass these updates to a raw `NativeDocument`, a Go frame decoder, or a
Go Counter/Set/LWW/Tree group: that would bypass the collection semantic
validator and does not establish wire compatibility.

```ts
import {
  encodeNativeUpdate,
  NativeCollectionsDocument,
} from "@darkinno/crdt-client/collections";

const board = new NativeCollectionsDocument("tablet-7");
const inspections = board.getCounter("inspections"); // PN-Counter
const openTasks = board.getORSet<{ id: string }>("open-tasks");
const title = board.getLWWRegister<string>("title");
const outline = board.getORTree<{ kind: string }>("outline");

board.onUpdate(({ update, local }) => {
  if (local) socket.send(encodeNativeUpdate(update));
});

board.transact(() => {
  inspections.increment(1n);
  openTasks.add({ id: "task-42" });
  title.set("Morning inspection");
  const root = outline.add(null, { kind: "report" });
  outline.add(root, { kind: "finding" });
});

socket.onmessage = ({ data }) => {
  board.applyEncodedUpdate(new Uint8Array(data), "authenticated-peer");
};
```

- `NativeCounter` is a PN-Counter. Each actor only advances its own retained
  positive/negative component; reads return `bigint`, and a remote component
  that decreases under a newer tag is rejected before native-map mutation.
- `NativeORSet` has immutable add tags and retained observed-remove
  tombstones. A remove delivered before its add still wins when that add
  arrives. Equal values may have several tags but appear once in `values()`.
- `NativeLWWRegister` is a one-value retained-tombstone register; the existing
  native ID order (counter, then UTF-8 actor bytes) resolves concurrency.
- `NativeORTree` retains immutable parent links plus tombstones. Missing or
  deleted parents hide descendants; moves are remove plus a new add, never a
  parent rewrite. Cycles, excessive depth, pending parents, nodes, and
  tombstones are rejected under explicit limits.

Snapshots include root declarations, current bounded native-map state, and the
local counter. Persist that unit atomically with the authenticated outbox and
delivery frontier before reusing a replica ID. CRC/checksums in a surrounding
transport still do not authenticate a peer or authorize a mutation.

The [collection and rich-text architecture assessment](../../docs/design/typescript-client-types-and-rich-text.md)
records the wire boundary and the next rich-editor implementation gate.
The runnable [structured-editor example](examples/collections-editor/) shows
how to compose the four collection types around an editor without pretending
that a rich-text body is a collection value.

### Native protocol, limits, and persistence

`native-ts-v1` updates are immutable operation sets, not authentication. A
host must still authenticate the sender, authorize the document/group, bind a
schema and deployment limits, cap the HTTP/WebSocket body before allocating a
`Uint8Array`, protect replay/outbox retention, and encrypt in transit/at rest
where required. The decoder checks canonical UTF-8 JSON, exact fields, IDs,
cycles, type conflicts, duplicate-ID payload conflicts, and every limit before
it changes state.

## Browser-native document: persistent local-first use in a few lines

`native-ts-v1` now has a browser facade at
`@darkinno/crdt-client/browser`. `openNativeBrowserDocument` restores an
append-only IndexedDB record before exposing the same named Map/Array/Text API. It
persists a local mutation before an optional transport is allowed to receive
it, and an application can wait for the local recovery boundary with
`flush()`:

```ts
import {
  createBrowserReplicaID,
  openNativeBrowserDocument,
} from "@darkinno/crdt-client/browser";

const board = await openNativeBrowserDocument({
  documentID: "roadmap-2026-q3", // bind this to one product group/schema
  replicaID: createBrowserReplicaID(), // unique for this active tab/Worker
});

const metadata = board.getMap("metadata");
const cards = board.getArray("cards");
board.transact(() => {
  metadata.set("title", "Roadmap");
  cards.push([{ id: "draft", status: "open" }]);
});
await board.flush(); // local IndexedDB recovery record is committed
```

The default browser store is `darkinno-crdt-native`; `documentID` is its local
record key. A recovery record has three parts: copied root declarations plus
the local actor counter and sparse state vector; an optional compacted bounded
state base; and an append-only canonical update log. An append writes its
update and current
metadata in one IndexedDB transaction. This avoids serializing the whole
document on every keystroke while ensuring a same-actor restart never reuses a
counter after `flush()` resolves.

The defaults compact only after 128 retained updates or 1 MiB of retained log,
and only when no local update awaits a transport receipt. They cap the log at
10,000 updates or 32 MiB. A document with an unresolved array parent retains
the log and will not make an incomplete snapshot. If its cap is reached,
`flush()` rejects with `NativeBrowserError("persistence_limit")`; the host must
recover missing parents, reconnect, compact a complete state, or present an
offline-storage error instead of silently dropping a mutation.

### Bring your own authenticated transport

`NativeBrowserTransport` deliberately has no URL, WebSocket handshake, token,
or server envelope built in. That prevents `native-ts-v1` from being presented
as compatible with the manifest-bound Go frame relay. Supply an adapter only
after it authenticates the user and binds this exact `documentID`, schema,
native semantic version, and compatible limits:

```ts
const board = await openNativeBrowserDocument({
  documentID: authenticatedGroupID,
  replicaID: createBrowserReplicaID(),
  transport: authenticatedNativeTransport,
});
```

The adapter's `send(bytes)` must resolve only at the product's receipt
boundary. Until then, the browser client retains the canonical bytes in its
local outbox and retries them after the next `connect()`. A raw WebSocket
`send()` commonly means only that the browser queued bytes; it is not a remote
durable acknowledgement. Received bytes enter `applyEncodedUpdate()` and are
still bounded and canonical-validated before state changes. Cap an HTTP or
WebSocket message before constructing its `Uint8Array`.

`NativeBrowserDocument` exposes the same `getStateVector()` and
`encodeStateAsUpdates(peerVector)` read APIs. Its IndexedDB metadata persists
the sparse vector on both append and compaction, so a restart does not turn a
known repair suffix back into a full transfer.

For a local same-origin multi-tab experience, pass a
`BroadcastChannelNativeTransport` as `liveTransport`; attach an authenticated
receipt transport separately with `connect()` when it is available:

```ts
const live = new BroadcastChannelNativeTransport(JSON.stringify([
  "darkinno-crdt", "native-ts-v1", authenticatedGroupID,
]));
const board = await openNativeBrowserDocument({
  documentID: authenticatedGroupID,
  replicaID: createBrowserReplicaID(),
  liveTransport: live,
});
await board.connect(authenticatedNativeTransport);
```

BroadcastChannel is intentionally volatile: it supplies neither authentication,
durable delivery, history/bootstrap, nor anti-entropy, and its `publish()`
cannot acknowledge the durable outbox. Use it only as an extra live path beside
IndexedDB and an authenticated server transport. The runnable
[multi-tab/offline example](examples/multitab-offline/) also includes a
static-shell-only Service Worker; it never caches API responses or CRDT data.

`flush()` proves that the browser completed the requested IndexedDB work, not
that an operating-system crash, quota eviction, a closed browser process, or a
remote service preserved it forever. Surface `onError`, request persistent
storage where product policy requires it, and treat server-side durable
outbox/replay/checkpoint logic as a separate production requirement. See the
[browser-native architecture decision](../../docs/design/browser-native-client.md)
and [controlled browser benchmarks](../../docs/operations/browser-native-client-2026-07-30.md).

The defaults are deliberately mobile-oriented: 1 MiB encoded update, 10,000
operations/update, 128 roots, 10,000 retained map entries, 100,000 array nodes
and array tombstones, 10,000 unresolved array nodes, 64 KiB/value, and nesting
depth 32. Sparse state vectors are additionally capped at 10,000 actors,
100,000 ranges, and 1 MiB encoded. Pass lower compatible values to
`new NativeDocument(replicaID, options)`. A limit rejection is atomic; it
never accepts a partial update.

Call `document.snapshot()` and atomically persist its **root declarations**,
`updates`, and `counter`; restore with `NativeDocument.restore(replicaID,
snapshot, options)`. The local counter is part of identity safety: persisting
only state risks reuse of an `{ actor, counter }` ID after restart. A document
with unresolved array parents cannot create a snapshot; deliver the missing
parents first.

Native arrays optimize for batched local edits and convergent sequence
semantics, not general random-access workloads. A visible-node projection is
retained privately until an insert or tombstone changes structure, making
repeated `length` and `get(index)` reads cheap after the initial O(n) projection.
`insert(index, ...)` still needs that projection to find its left neighbour;
large arrays should be edited in a Worker and mutations should be batched in a
transaction. The included benchmark records append, middle-insert, shuffled
merge, state-update, and cached-read costs on the executing Node version rather
than making a mobile-device capacity claim.

The RGA protocol still requires explicit compatibility admission. Before
loading or applying an RGA frame, authenticate a matching `replica.Manifest`,
group, schema, epoch, codec, semantic version, and `ProtocolPolicy` capability.
The default artifact accepts and emits only run-v2 state/delta IDs 19/20 with
semantics version 2, matching `crdt.DefaultRGAFrameType()`. It deliberately
rejects scalar-v1 IDs 11/12. A manifest represents one concrete protocol, and
the TypeScript loader checks the expected contract before exposing a runtime.
For an explicitly negotiated legacy v1 group, build `make wasm-v1` and pass
`RGA_PROTOCOL_V1`; do not place both formats in one document runtime.

The [RGA run-v2 wire specification](../../docs/protocol/rga-run-v2.md) and its
machine-readable vectors are the contract for a non-Wasm implementation. The
Wasm wrapper remains the recommended path when the host can run it because it
uses the same parser and merge engine as Go.

For text editors, import `@darkinno/crdt-client/bindings`. It provides named
plain-text bindings for Quill, Monaco, CodeMirror 6, Tiptap, Lexical,
ProseMirror, and Slate. Tiptap is accepted only with a canonical unmarked
paragraph/text schema; Lexical, ProseMirror, and Slate require an
application-owned schema-preserving text-leaf port. These adapters are not
Yjs bindings and do not replicate editor formatting, nodes, embeds, selections,
or undo history. See the [editor binding guide](../../docs/integration/rga-editor-bindings.md)
and [2026-07-31 assessment](../../docs/operations/rga-editor-bindings-2026-07-31.md).

For Quill Deltas that must retain approved formatting, use `initRichTextWasm`
and `bindQuillRichText` instead. It is a separate manifest-bound rich-text v1
runtime (state/delta TypeIDs `23/24`), not an extension of the plain RGA
adapter. The application must supply a schema-specific attribute codec; embeds
and unknown attributes are rejected. Quill's required terminal newline belongs
to the document projection, and a joining client must bind after state recovery
with `initialContent: "document"`. See the [rich-text editor binding
guide](../../docs/integration/richtext-editor-bindings.md).

For BlockNote's default text blocks, use `bindBlockNoteRichText` with the
manifest SchemaID `darkinno:blocknote-text-v1`. It preserves the bounded
paragraph/heading/list/quote/code subset, nesting, default block props, and
approved inline styles without adding BlockNote or Yjs to this package's
runtime dependencies. Tables, media, links, custom blocks, and unknown props
are rejected rather than flattened. See the [BlockNote binding guide](../../docs/integration/blocknote-richtext-bindings.md).

## Build and verify

From the repository root:

```sh
make wasm
make wasm-v1 # optional legacy scalar-v1 artifact in .tmp/crdt-rga-v1-wasm/
make typescript-test
make typescript-native-benchmark
make typescript-bindings-benchmark
make typescript-blocknote-benchmark
make wasm-test
make wasm-v1-test # verifies the separately built legacy artifact
make typescript-benchmark
make wasm-benchmark
make wasm-bindings-benchmark
```

`make wasm` writes the matching `crdt-rga.wasm` and Go toolchain's
`wasm_exec.js` below `.tmp/crdt-rga-wasm/`. Do not mix `wasm_exec.js` from a
different Go release with the generated module. The Node test loads the actual
Wasm artifact, checks Go-to-TypeScript frame decoding, and simulates three
replicas with duplicate, reordered delivery and snapshot recovery.

The native benchmark reports append, middle insert, shuffled replication,
state-update, and cached visible-read samples; the existing targets report raw
decoder throughput and actual Node-to-Go-Wasm insert/apply latency. Treat all
of them as controlled baselines for the local machine and Node/Go versions, not
a mobile-device SLA.

The current controlled local sample, including all five values and frame sizes,
is recorded in the [2026-07-29 benchmark report](../../docs/operations/benchmark-2026-07-29.md).

For a browser deployment, copy both generated files into your asset pipeline,
serve `.wasm` as `application/wasm`, and allow Wasm compilation in your CSP.
Use a Web Worker for long documents so local merges do not block the UI thread.

## Browser use

Load the generated Go support file before importing the TypeScript wrapper:

```html
<script src="/assets/wasm_exec.js"></script>
<script type="module">
  import {
    decodeFrame, FrameType, initRGAWasm, RGA_PROTOCOL_RUN_V2,
  } from "/assets/crdt-client/index.js";

  // Bind this expected protocol to the authenticated manifest for the group.
  const runtime = await initRGAWasm({
    wasmURL: "/assets/crdt-rga.wasm",
    expectedProtocol: RGA_PROTOCOL_RUN_V2,
  });
  const document = runtime.create("browser-replica-7");

  // Apply locally first, then place these same bytes in an authenticated outbox.
  const delta = document.insert(0, "local collaborative edit");
  if (decodeFrame(delta).typeID !== FrameType.RGARunDelta) throw new Error("wrong frame");
  await sendAuthenticatedDelta(delta);

  // On receipt, authenticate and verify the manifest before calling applyDelta.
  document.applyDelta(receivedDelta);
</script>
```

`decodeFrame` checks only the common envelope. The Wasm call validates the
type-specific RGA payload and resource limits again. Neither layer provides
TLS, signatures, authorization, replay prevention, transport retries, or
server-side policy enforcement.

## Persistence and limits

`document.snapshot()` returns `{ state, clock, frontier }`. Persist and restore
all three fields atomically via `runtime.restore(snapshot)`. Saving only the
state frame risks reusing an HLC mutation tag after a process restart.

### Offline-first Wasm RGA facade

`openRGAWasmBrowserDocument` provides the same atomic recovery boundary for a
manifest-selected Go/Wasm RGA actor without making every application rebuild
an IndexedDB log and outbox:

```ts
import {
  BroadcastChannelNativeTransport,
  createBrowserReplicaID,
  initRGAWasm,
  openRGAWasmBrowserDocument,
} from "@darkinno/crdt-client";

const runtime = await initRGAWasm({ wasmURL: "/assets/crdt-rga.wasm" });
const replicaID = createBrowserReplicaID("tab"); // one concurrently active actor
const live = new BroadcastChannelNativeTransport(JSON.stringify([
  "darkinno-crdt", "rga-run-v2", authenticatedGroupID,
]));
const document = await openRGAWasmBrowserDocument({
  documentID: authenticatedGroupID,
  replicaID,
  runtime,
  liveTransport: live,
  transport: authenticatedRGAReceiptTransport,
});
document.insert(0, "offline draft");
await document.flush();
```

The facade uses separate `rga-documents` / `rga-updates` stores; it never mixes
Go RGA frames with `native-ts-v1` records. Its key contains both document and
replica IDs, so a live second tab must create a fresh actor rather than reuse
the first tab's HLC. `liveTransport` is only post-append local delivery and
never acknowledges the outbox; `transport` resolves only at the
application-defined durable receipt, and a raw WebSocket enqueue is not
sufficient.

`anchorAt(runeOffset)` and `resolveAnchor(anchor)` expose local cursor
boundaries as RGA Position/Tags instead of emulating a Yjs relative position.
The plain-text binding can capture/restore supported editor selections through
remote merges. Anchors are ephemeral application/presence metadata: never put
them in RGA frames, snapshots, or the durable outbox. See the
[offline RGA architecture](../../docs/design/browser-wasm-rga-offline-client.md),
[editor binding guide](../../docs/integration/rga-editor-bindings.md), and
[controlled performance evidence](../../docs/operations/browser-wasm-rga-offline-2026-07-31.md).

The default client budget is intentionally conservative: 1 MiB total frame,
100,000 nodes/tags, 10,000 unresolved nodes, 512 KiB pending metadata, and
64 KiB replica-ID strings. A single local insert is further capped at 64 KiB
and 16,384 runes before the RGA constructs per-rune nodes; split a larger editor
transaction into ordered chunks. The transport must cap request/body size before
it allocates a `Uint8Array`; the Wasm boundary checks the byte length again
before copying it into Go.

This module is appropriate for browsers and WebViews with a compatible
WebAssembly runtime. Native mobile apps without such a runtime need a separate
Go Mobile binding or a separately specified, cross-language-tested semantic
implementation; a TypeScript envelope decoder alone is not a native merge
engine.

## 中文说明

该目录同时提供纯 TypeScript 的结构化 CRDT 和 Go RGA 的 Wasm 路径。`NativeDocument`
提供 `NativeMap`（LWW）、`NativeArray`（带墓碑、乱序待父节点处理的 RGA）和纯文本
`NativeText`（每个 Unicode 标量一个 RGA 节点），适合所有参与者明确选择 `native-ts-v1`
的浏览器/WebView 结构化状态同步，因此不需要 Wasm 的下载和启动成本。Text 的
`length`、`insert`、`delete` 使用浏览器字符串相同的 UTF-16 code-unit 偏移，拒绝将代理对
拆开。它的更新是规范 JSON，和 Go 的 CRDT frame、TypeID、run-v2 完全不同；不能把 native
更新送给 Go decoder，也不能把它伪装成已有复制组的一部分。

`transact()` 将本地多个改动合成一个 update，`onUpdate()` 交给宿主网络层，
`applyUpdate()` 在完整校验后才合并，因此重复与乱序投递可收敛。值为深拷贝 JSON，嵌套
对象是原子 LWW 值；本版本不支持嵌套 shared type。默认上限为 1 MiB/update、10,000
op/update、10,000 map entry、100,000 array node/墓碑、100,000 text node/墓碑、10,000
pending node、64 KiB/value 与深度 32。Text 必须协商 `native-ts-text-v1` 能力；未协商旧端
应 fail-closed，不能按 Array、Yjs 或 Go/Wasm rich text 解释。宿主仍必须完成身份认证、授权、
schema/limit 协商、传输 body 上限、重放治理和加密；canonical JSON 与 CRDT 合并都不提供这些
安全能力。快照必须把 root 声明、`updates` 与本地 `counter` 原子持久化，避免重启后复用 ID。

### 原生 State Vector 与反熵

在已经完成**身份认证且绑定同一 document/schema**的对端之间，可用
`getStateVector()` 交换摘要，再调用 `encodeStateAsUpdates(peerVector)` 仅导出对端尚未证明
已拥有的状态；`encodeNativeStateVector` 和 `decodeNativeStateVector` 提供该摘要的规范字节表示。
这里采用按 actor 保存精确 counter 区间的稀疏（dotted）State Vector，而不是
`actor -> 最大 counter`：native 更新允许乱序抵达，且被拒绝的本地操作可能留下未使用的
counter；仅保存最大值会把缺失操作错误地声明为已知，反熵时可能漏传状态。

State Vector 仅用于优化传输，不是成员资格、授权或持久 receipt 的证据。Array/Text 删除没有
独立 dot，因此即使对端已有插入 ID，状态修复也会保留并重传 tombstone，以保证 delete-before-
insert 收敛。新快照及浏览器 IndexedDB metadata 会将 vector 与 root、updates、本地 counter
原子保存；旧快照没有 vector 时仍可恢复，但只会保守地识别其保留状态中出现过的 ID。

对于必须与 Go、原生移动端或既有 RGA 数据互通的组，仍然使用下面的 Wasm run-v2 路径；它
复用同一份 Go 编辑、乱序、墓碑和 HLC 语义，而不是维护一份隐式兼容的第二实现。

## 浏览器原生文档：几行代码实现本地优先与恢复

`@darkinno/crdt-client/browser` 的 `openNativeBrowserDocument` 在暴露
Map/Array/Text API 前先从 IndexedDB 恢复，并将本地变更写入追加日志。最小使用方式如下：

```ts
import { createBrowserReplicaID, openNativeBrowserDocument } from "@darkinno/crdt-client/browser";

const board = await openNativeBrowserDocument({
  documentID: "roadmap-2026-q3", // 对应一个业务复制组/schema
  replicaID: createBrowserReplicaID(), // 每个同时活跃 tab/Worker 必须不同
});
board.getMap("metadata").set("title", "Roadmap");
board.getArray("cards").push([{ id: "draft", status: "open" }]);
board.getText("body").insert(0, "纯文本草稿");
await board.flush(); // 本地恢复记录已提交
```

默认数据库名为 `darkinno-crdt-native`，`documentID` 是本地记录键。记录把根类型声明与本地
counter、稀疏 State Vector、可选完整 state base、以及规范 update 追加日志分开保存；每次追加在一个
IndexedDB 事务中同时保存 update 与 metadata。因此不会每次编辑都编码整个文档，并且
`flush()` 成功后以同一 actor 重启不会复用 counter。默认在日志达到 128 条或 1 MiB 时尝试
压缩，但只有没有待确认本地 outbox 且 array 没有待父节点时才可压缩；总上限为 10,000 条或
32 MiB。上限耗尽会由 `flush()` 返回 `persistence_limit`，绝不会静默丢弃变更。

`NativeBrowserTransport` 不内置 URL、WebSocket 鉴权或服务端 envelope。这样不会把
`native-ts-v1` 错称为可直接接入 Go manifest/frame relay 的协议。宿主应在 adapter 中先完成
身份验证、组/schema/version/limits 绑定和入站 body 限制；`send(bytes)` 只有在产品定义的
receipt 到达后才应 resolve。浏览器层在此之前保留 outbox 并在 `connect()` 后重试；普通
WebSocket `send()` 只代表浏览器已入队，并不是远端持久确认。

`NativeBrowserDocument` 同样暴露 `getStateVector()` 与
`encodeStateAsUpdates(peerVector)` 的只读接口。它在追加或压缩时同步保存稀疏 vector，避免
重启后把已完成的反熵后缀再次退化为全量状态传输。

同源多标签将 `BroadcastChannelNativeTransport` 作为 `liveTransport` 传入；认证服务端 receipt
adapter 仍通过 `connect()` 单独连接。前者只是易失的实时路径：没有认证、持久化、
历史/bootstrap 或反熵能力，`publish()` 也绝不会确认 outbox；不能取代经过认证的服务端
transport。更多架构、示例和实测数据见[多标签与离线壳](../../docs/integration/browser-multitab-offline.md)、
[浏览器原生客户端设计](../../docs/design/browser-native-client.md)和
[受控性能报告](../../docs/operations/browser-multitab-offline-2026-08-01.md)。

RGA 仍需要显式协商：先认证 `replica.Manifest`（group、schema、epoch、codec、语义
版本和能力），再接收 frame；校验和不等于身份验证。默认 Wasm 产物只接收/发出
TypeID 19/20、语义版本 2 的 run-v2，和新建 Go RGA 组的
`crdt.DefaultRGAFrameType()` 一致；TypeScript loader 也会校验期望协议。`snapshot()` 的
`state`、`clock`、`frontier` 必须原子持久化。默认限制为 1 MiB frame、10 万
node/tag、1 万个待父节点和 512 KiB 待处理元数据；单次本地插入额外限制为 64 KiB 且
16,384 rune，超出时应按顺序拆分编辑事务。旧 v1 组只能显式执行 `make wasm-v1` 并传入
`RGA_PROTOCOL_V1`；不要在同一 document runtime 混用两种格式。没有 Wasm 运行时的原生
移动端仍需 Go Mobile 绑定或经过独立跨语言验证的语义实现，不能把 frame 解码器误当成本地
合并器。
