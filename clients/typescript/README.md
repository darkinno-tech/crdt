# TypeScript CRDT client: native shared types, frame decoder, and RGA Wasm

This directory gives browsers and JavaScript/WebView clients two deliberately
separate local-merge paths:

1. `src/native.ts` is a dependency-free TypeScript document with `NativeMap`
   and `NativeArray` shared types. It avoids Wasm download/startup overhead for
   structured application state where all peers select its native TS contract.
2. `src/frame.ts` is a dependency-free TypeScript decoder for the canonical
   v1 outer frame. It validates magic, version, shortest varints, lengths and
   CRC-32C under explicit limits. It returns opaque codec bytes and does not
   treat a checksum as authentication.
3. `cmd/crdt-rga-wasm` compiles the existing Go RGA implementation to Wasm.
   The default artifact uses compact run-v2 frames, matching new Go RGA
   groups. Local edits produce canonical delta frames; incoming frames go
   through the same bounded Go decoder and merge semantics as server-side Go.

`NativeDocument` is not a substitute implementation of Go RGA run-v2. Its
canonical UTF-8 JSON updates are called **`native-ts-v1`** in this guide, have
no `FrameType`, and must never be passed to `decodeFrame`, a Go frame decoder,
or an existing Go replication group. For a group that must interoperate with
Go, native mobile, or prior RGA data, retain the negotiated Wasm path below.

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
- `document.transact()` emits one local update after a group of mutations.
  `onUpdate()` supplies transport-ready operations and `observe()` on a type
  runs after the transaction has been applied.
- `applyUpdate()` is transport-agnostic and validates the entire update before
  mutation. It accepts reordered and duplicate operations. `encodeNativeUpdate`
  and `decodeNativeUpdate` use canonical JSON for byte transports.

```ts
import { decodeNativeUpdate, encodeNativeUpdate, NativeDocument } from "@darkinno/crdt-client/native";

const alice = new NativeDocument("alice-device-7");
const metadata = alice.getMap("metadata");
const cards = alice.getArray("cards");

alice.onUpdate(({ update }) => {
  // Authenticate and authorize this message at the transport boundary first.
  socket.send(encodeNativeUpdate(update));
});

alice.transact(() => {
  metadata.set("title", "Roadmap");
  cards.push([{ id: "card-1", title: "Draft" }]);
}, "local-editor");

socket.onmessage = ({ data }) => {
  alice.applyUpdate(decodeNativeUpdate(new Uint8Array(data)), "remote-peer");
};
```

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
append-only IndexedDB record before exposing the same named Map/Array API. It
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
record key. A recovery record has three parts: copied root declarations and
the local actor counter, an optional compacted bounded state base, and an
append-only canonical update log. An append writes its update and current
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

For a local same-origin multi-tab experience, use two independently created
`BroadcastChannelNativeTransport` instances with the same document-specific
channel name. It is intentionally volatile: BroadcastChannel supplies neither
authentication, durable delivery, history/bootstrap, nor anti-entropy. It is
useful only as an extra live path beside IndexedDB and an authenticated server
transport.

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
depth 32. Pass lower compatible values to `new NativeDocument(replicaID,
options)`. A limit rejection is atomic; it never accepts a partial update.

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

## Build and verify

From the repository root:

```sh
make wasm
make wasm-v1 # optional legacy scalar-v1 artifact in .tmp/crdt-rga-v1-wasm/
make typescript-test
make typescript-native-benchmark
make wasm-test
make wasm-v1-test # verifies the separately built legacy artifact
make typescript-benchmark
make wasm-benchmark
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
提供 `NativeMap`（LWW）与 `NativeArray`（带墓碑、乱序待父节点处理的 RGA），适合所有
参与者明确选择 `native-ts-v1` 的浏览器/WebView 结构化状态同步，因此不需要 Wasm 的下载和
启动成本。它的更新是规范 JSON，和 Go 的 CRDT frame、TypeID、run-v2 完全不同；不能把
native 更新送给 Go decoder，也不能把它伪装成已有复制组的一部分。

`transact()` 将本地多个改动合成一个 update，`onUpdate()` 交给宿主网络层，
`applyUpdate()` 在完整校验后才合并，因此重复与乱序投递可收敛。值为深拷贝 JSON，嵌套
对象是原子 LWW 值；本版本不支持嵌套 shared type。默认上限为 1 MiB/update、10,000
op/update、10,000 map entry、100,000 array node/墓碑、10,000 pending node、64 KiB/value
与深度 32。宿主仍必须完成身份认证、授权、schema/limit 协商、传输 body 上限、重放治理和
加密；canonical JSON 与 CRDT 合并都不提供这些安全能力。快照必须把 root 声明、`updates`
与本地 `counter` 原子持久化，避免重启后复用 ID。

对于必须与 Go、原生移动端或既有 RGA 数据互通的组，仍然使用下面的 Wasm run-v2 路径；它
复用同一份 Go 编辑、乱序、墓碑和 HLC 语义，而不是维护一份隐式兼容的第二实现。

## 浏览器原生文档：几行代码实现本地优先与恢复

`@darkinno/crdt-client/browser` 的 `openNativeBrowserDocument` 在暴露
Map/Array API 前先从 IndexedDB 恢复，并将本地变更写入追加日志。最小使用方式如下：

```ts
import { createBrowserReplicaID, openNativeBrowserDocument } from "@darkinno/crdt-client/browser";

const board = await openNativeBrowserDocument({
  documentID: "roadmap-2026-q3", // 对应一个业务复制组/schema
  replicaID: createBrowserReplicaID(), // 每个同时活跃 tab/Worker 必须不同
});
board.getMap("metadata").set("title", "Roadmap");
board.getArray("cards").push([{ id: "draft", status: "open" }]);
await board.flush(); // 本地恢复记录已提交
```

默认数据库名为 `darkinno-crdt-native`，`documentID` 是本地记录键。记录把根类型声明与本地
counter、可选完整 state base、以及规范 update 追加日志分开保存；每次追加在一个
IndexedDB 事务中同时保存 update 与 metadata。因此不会每次编辑都编码整个文档，并且
`flush()` 成功后以同一 actor 重启不会复用 counter。默认在日志达到 128 条或 1 MiB 时尝试
压缩，但只有没有待确认本地 outbox 且 array 没有待父节点时才可压缩；总上限为 10,000 条或
32 MiB。上限耗尽会由 `flush()` 返回 `persistence_limit`，绝不会静默丢弃变更。

`NativeBrowserTransport` 不内置 URL、WebSocket 鉴权或服务端 envelope。这样不会把
`native-ts-v1` 错称为可直接接入 Go manifest/frame relay 的协议。宿主应在 adapter 中先完成
身份验证、组/schema/version/limits 绑定和入站 body 限制；`send(bytes)` 只有在产品定义的
receipt 到达后才应 resolve。浏览器层在此之前保留 outbox 并在 `connect()` 后重试；普通
WebSocket `send()` 只代表浏览器已入队，并不是远端持久确认。

同源多标签可额外使用相同通道名的 `BroadcastChannelNativeTransport`，但它只是易失的实时路径，
没有认证、持久化、历史/bootstrap 或反熵能力，不能取代经过认证的服务端 transport。更多架构
与实测数据见[浏览器原生客户端设计](../../docs/design/browser-native-client.md)和
[受控性能报告](../../docs/operations/browser-native-client-2026-07-30.md)。

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
