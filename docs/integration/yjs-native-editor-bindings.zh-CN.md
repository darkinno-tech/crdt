# 原生 Yjs 编辑器绑定

`@darkinno/crdt-client/yjs` 是面向**原生 Yjs 文档**的可选浏览器绑定。它把
`Y.TextEvent.delta` 映射为 CodeMirror 6 的变更集；远端改一个字符只会修改对应区间，
不会把整篇文本重新投影到编辑器。

它与 `bindRGAPlainText` 严格隔离：Go RGA/run-v2 frame、`text.Anchor` 与 Go
`awareness` 不是 Yjs document、relative position 或 y-protocols awareness。
不要把两类协议放进同一个 room，也不要转换在线 mutation。

## 兼容性边界

| 表面 | 约定 |
| --- | --- |
| 文档与 update | 直接使用 `Y.Doc` / `Y.Text`，每个 room 明确固定 V1 或 V2。state vector 和差量仍是原生 Yjs 语义。 |
| 远端编辑 | 将 `Y.TextEvent.delta` 转成一个或多个 CodeMirror UTF-16 change；首次挂接后不再为远端 update 调用 `text.toString()` 做全量投影。 |
| 相对位置 | `createRelativePosition` / `resolveRelativePosition` 提供有界 `Y.RelativePosition`，用于评论、选择和 anchor；awareness 光标只针对同一个本地 `Y.Text` 解析。 |
| 深层观察 | `observeYjsDeep` 仅同步交付受限的 path 与 live target；不保留惰性的 `Y.Event` 或任意用户值。 |
| 撤销/重做 | `createUndoManager` 只跟踪本 binding 的 `applyLocalReplacement` 本地事务。默认最多保留 256 个 stack item；undo/redo 发出补偿性本地 Yjs update，远端编辑不会入栈。 |
| 手动传输 | `onLocalUpdate` / `onLocalAwarenessUpdate` 是同步交给应用自有 outbox 的边界；回调抛错或本地出站字节超限会锁存对应路径并报告稳定错误码。 |
| 手动 V1 sync | `createSyncProtocol` 只读写一条有界、无 y-websocket 外层包装的 y-protocols SyncStep1/2 或 update。V2 继续使用 state-vector/diff API。 |
| Presence | 直接使用 `y-protocols/awareness` 的 encode/apply API。Yjs client ID 仅用于路由，不是已认证用户身份。 |
| 纯文本富格式 | `YjsTextBinding` 故意只支持纯文本；检测到 format 或 embed 就停止投影，绝不静默扁平化。 |
| Quill 2 富文本 | `YjsRichTextBinding` 和 `bindYjsQuillRichText` 直接传递经批准的原生 `Y.Text` Delta。format 和单键 embed 是有界 room schema；远端未知内容会冻结投影。 |

Go 侧的 `extensions.YJSHandler` 已兼容 y-websocket/y-protocols 外层，能够转发
这些原生字节；配置 YJSStore 后可获得持久的 state-vector 恢复。它并不会把 Go
CRDT group 变成 Yjs room。

## 数据流与传输所有权

```text
CodeMirror 本地事务
  -> Y.Text 事务（一帧原生 Yjs update）
  -> y-websocket-compatible provider 或显式 onLocalUpdate 回调
  -> extensions.YJSHandler / YJSStore
  -> 对端 Y.applyUpdate
  -> Y.TextEvent.delta
  -> 精确 CodeMirror change + relative-position 光标解析

Awareness.setLocalStateField(relative cursor)
  -> y-protocols awareness update
  -> relay（仅临时态）
  -> applyAwarenessUpdate
  -> remoteCursors()
```

一个 Y.Doc 只能有一种传输所有者：

- 使用标准 `y-websocket` provider 时，复用同一个 `Y.Doc`/`Awareness`，不要配置
  `onLocalUpdate` 与 `onLocalAwarenessUpdate`。
- 宿主自行管理认证传输时，接收的二进制 payload 分别传给
  `applyRemoteUpdate`/`applyRemoteAwarenessUpdate`，发送端只接收两个 `onLocal*`
  回调。

混用会重复转发幂等数据，并使背压、指标和授权审计变得不确定。

## CodeMirror 6 接入

应用拥有 provider、认证、授权与编辑器生命周期，binding 不拥有 WebSocket 或 Y.Doc：

```ts
import * as Y from "yjs";
import { Awareness } from "y-protocols/awareness.js";
import type { ViewUpdate } from "@codemirror/view";
import { bindYjsCodeMirrorPlainText } from "@darkinno/crdt-client/yjs";

const document = new Y.Doc();
const text = document.getText("content");
const awareness = new Awareness(document);
let binding: ReturnType<typeof bindYjsCodeMirrorPlainText> | undefined;

function onCodeMirrorUpdate(update: ViewUpdate) {
  binding?.applyViewUpdate(update);
}

binding = bindYjsCodeMirrorPlainText(document, text, view, {
  updateFormat: "v1", // 必须与 YJSRoom/YJSStore 的 format 一致。
  maxUpdateBytes: 1 << 20,
  maxAwarenessBytes: 64 << 10,
  maxTextUTF16: 1 << 20,
  maxCursorBytes: 256,
}, awareness);

binding.setLocalCursor({ anchor: 12, head: 18 });
renderRemoteCursors(binding.remoteCursors());
```

首次挂接时 editor 与 document 不一致会写入一次完整初始值；之后远端事务全是区间
change。单区间本地 CodeMirror 更新同样增量；旧 adapter 或多区间本地更新会走显式的
原子文本 fallback，而不是发送残缺的 Yjs 事务。

## Quill 2 富文本接入

`@darkinno/crdt-client/yjs-richtext` 是 Quill 原生 Delta 模型的可选绑定。它不会内置
Quill 或 y-quill；应用自己负责 Quill 版本、module、provider、文档生命周期和 schema。
绑定只接受字符串 insert、已批准的标量属性，以及已批准的单键标量 embed；HTML、任意嵌套
对象、custom module state、DOM 引用、cursor 和授权信息都不属于这个 Delta 契约。

```ts
import * as Y from "yjs";
import { bindYjsQuillRichText } from "@darkinno/crdt-client/yjs-richtext";

const document = new Y.Doc();
const text = document.getText("content");
const binding = bindYjsQuillRichText(document, text, quill, {
  updateFormat: "v1", // 必须与 YJSRoom / YJSStore 一致。
  maxUpdateBytes: 1 << 20,
  maxTextUTF16: 1 << 20,
  maxDeltaOperations: 512,
  maxAttributesPerOperation: 8,
  maxAttributeKeyBytes: 64,
  maxAttributeValueBytes: 1024,
  maxEmbedBytes: 4096,
  allowedAttributes: ["bold", "italic", "header", "link"],
  allowedEmbeds: ["image"],
  // 标准 y-websocket-compatible provider 拥有该 Y.Doc 时不要配置此项。
  onLocalUpdate: (update) => durableOutbox.append(update),
});
```

默认 `initialContent: "document"` 会用已验证的 `Y.Text` Delta 替换 Quill 初始内容。
仅在初始化空 `Y.Text` 时使用 `initialContent: "editor"`；非空文档会被拒绝，避免加入中的
编辑器制造并发 seed。一个文档在标准 y-websocket-compatible provider 与同步
`onLocalUpdate` 交接之间只能二选一。

本地 Delta 不符合 schema 时，Quill adapter 会恢复最后一个已验证的 Y.Text 投影；远端 Delta
不符合 schema 时，Y.Doc 保持已合并但富文本投影冻结。应修复已认证的 schema 准入并按 room
既有 state-vector/checkpoint 流程恢复，不能删除 format/embed 后继续展示。

## 手动回调失败与恢复

`onLocal*` 回调只是**同步交接**，不是持久回执，也不能证明对端已应用 update。若产品要求
可靠投递，回调必须先把收到的字节复制到应用自有的 retry/outbox 记录，再进行可能失败的网络
发送；回调返回后的异步发送失败仍由传输所有者负责重试和恢复。

回调抛错，或生成的本地 update 超过对应字节上限时，binding 只锁存该出站路径，并只调用一次
`onError`：

- `applyLocalReplacement`、`undo()`、`redo()` 会返回
  `YjsBindingError("local_update_failed")` 或 `YjsBindingError("resource_limit")`。
  触发它的 Yjs 事务已经提交，无法回滚；后续 binding-owned 文本写入会在创建另一条未交接
  update 前被拒绝。
- `setLocalCursor`、`clearLocalCursor` 对应返回
  `YjsBindingError("local_awareness_failed")` 或 `YjsBindingError("resource_limit")`。
  awareness 仍是临时态，后续 binding-owned cursor 写入会被拒绝。

不要用第二次编辑来“重试”。应暂停受影响 surface 的输入，修复或替换应用 outbox/传输，按 room
既有 state-vector 恢复流程重新对齐，再创建新的 binding。回调收到的字节由调用方负责保存；
binding 不会伪称已持久化。`onError` 自身抛出的异常会被忽略，避免重新进入 Yjs 同步 observer
循环。

## 相对位置、深层视图与本地撤销

`createRelativePosition` / `resolveRelativePosition` 只能针对当前绑定的
`Y.Text` 使用；外来、畸形或失效的位置会失败关闭。`observeYjsDeep` 需要显式配置
每个事务的事件数与路径深度上限；上限溢出或应用回调抛错后会自行卸载，避免继续声称
视图仍然同步。

```ts
const undo = binding.createUndoManager({
  captureTimeout: 500,
  maxStackItems: 256,
});
```

undo history 只是本地 UI 状态，不能持久化、不能授权，也不能撤回服务端日志。成功的
`undo()` / `redo()` 是新的正常 Yjs update，仍需经过同一认证传输、持久回执与重试。
`maxStackItems` 默认是 256；binding 在下一次本地编辑将超上限前，调用 Yjs 自己的完整
history 释放路径清除 undo/redo 栈，再记录本次编辑。不能仅删除内部栈的旧元素：那会遗留
被保留的 deleted struct 或破坏 Yjs GC bookkeeping。

## 安全和资源边界

- `maxUpdateBytes` 与 `maxAwarenessBytes` 在进入对应 JS decoder 前拒绝入站字节，取值
  不得超过 relay/store 的同类限制。
- `maxTextUTF16` 只在本地编辑前限制 UI 增长；它无法让已经解码的恶意 Yjs update 不分配
  内存，公开入口仍必须依赖带认证的 server/store 限制。
- `maxCursorBytes` 限制 relative-position payload；畸形、过期、属于其他 shared type 的
  awareness 光标会被忽略。
- `maxStackItems` 限制本地 undo/redo 保留量；它只是 UI 内存边界，不是持久历史、授权
  记录或远端操作限制。
- 手动 `onLocalUpdate` / `onLocalAwarenessUpdate` 抛错，或生成的本地出站 update 超过相应
  上限，会锁存该路径。触发事务可能已提交，必须恢复应用自有 outbox 并重新同步后再挂接新
  binding；回调绝不是持久回执。
- `observeYjsDeep` 的事件数和路径深度分别有上限；溢出或应用回调失败后会卸载该观察者，
  而不是发送部分或过期的视图。
- awareness 是临时态：不能写入 YJSStore snapshot、Go CRDT frame、审计日志，也不能参与
  授权。服务端必须像 `YJSHandler` 一样把 client ID 绑定到已认证连接。
- 收到 `YjsBindingError("unsupported_text")` 表示渲染边界，底层 Yjs 文档仍有效。应卸载
  plain-text view 并改用带 schema 的富文本 surface，不能删除 format/embed 后继续显示。
- `YjsRichTextBinding` 独立限制 Delta 操作数、文本、每个 format key/value 和每个 embed；
  `allowedAttributes` / `allowedEmbeds` 是编辑器 schema 白名单，不是授权策略。远端 schema
  违例产生 `YjsBindingError("unsupported_rich_text")` 并冻结富文本视图，绝不把富内容降级为纯文本。
- Quill selection、cursor、comment、clipboard sanitisation、图片上传、链接请求、custom module
  state 和本地 undo grouping 仍由应用负责。presence payload 必须绑定已认证 room，且不能进入
  YJSStore snapshot 或授权决策。

## 验证与性能范围

```sh
make typescript-test
node --test clients/typescript/test/yjs.test.mjs
make typescript-yjs-core-benchmark
make typescript-yjs-bindings-benchmark
make typescript-yjs-richtext-benchmark
```

重点测试使用 JSDOM 下的真实 CodeMirror 6 view，覆盖远端区间更新、V1/V2、state-vector、
相对位置/awareness、格式拒绝、有界 undo history、深层观察、V1 SyncStep1/2、手动回调失败
锁存以及三副本延迟/重复/乱序模拟。富文本测试使用真实 Yjs 文档与确定性的 Quill-shaped Delta
port，覆盖批准 format/embed 收敛、本地恢复、远端投影冻结、本地交接锁存、Delta source-cursor
语义和 256 个畸形 Delta 的原子拒绝。该端口是 Quill 契约模拟，不是特定 Quill 浏览器构建的
验收声明。性能脚本只记录本地进程工作量和 editor write 形状，不能当作浏览器渲染、WebSocket、
TLS、WAN、持久化或服务容量结论；记录见[性能基线](../operations/yjs-native-editor-bindings-2026-08-01.md)
与 [Quill Delta 基线](../operations/yjs-richtext-binding-benchmark-2026-08-03.md)。
