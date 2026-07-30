# TypeScript frame decoder and RGA Wasm client

This directory gives browser and JavaScript/WebView clients a local RGA merge
path without reimplementing the Go RGA algorithm. It has two intentionally
separate layers:

1. `src/frame.ts` is a dependency-free TypeScript decoder for the canonical
   v1 outer frame. It validates magic, version, shortest varints, lengths and
   CRC-32C under explicit limits. It returns opaque codec bytes and does not
   treat a checksum as authentication.
2. `cmd/crdt-rga-wasm` compiles the existing Go RGA implementation to Wasm.
   The default artifact uses compact run-v2 frames, matching new Go RGA
   groups. Local edits produce canonical delta frames; incoming frames go
   through the same bounded Go decoder and merge semantics as server-side Go.

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

The two benchmark targets report raw five-sample decoder throughput and actual
Node-to-Go-Wasm insert/apply latency. Treat them as a baseline for the local
machine and Go/Node versions, not a mobile-device SLA.

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

该目录为浏览器和 JavaScript/WebView 客户端提供本地 RGA 合并能力，而不是把编辑
退化为服务端仲裁。TypeScript 只负责有界的通用 frame 外层解码；默认 Wasm 产物复用
同一份 Go 代码的 run-v2 编辑、乱序处理、墓碑和 HLC 语义，因此不会维护一份容易漂移的
第二实现。

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
