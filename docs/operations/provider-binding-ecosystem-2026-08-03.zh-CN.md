# Provider 与绑定生态决策 — 2026-08-03

## 结论

不能为了追求 provider 数量，把每个 Yjs 包转换成 Go CRDT 后端。正确方向是：在需要互操作的高价值链路上完善原生 Yjs 能力，Go CRDT provider 继续作为独立协议族，并且只在存在真实编辑器契约的地方增加有界绑定。

本次补充原生 `Y.Text` / Quill Delta 富文本绑定：它保留已批准的格式和单键嵌入，可通过既有的 y-websocket 兼容 `extensions.YJSHandler` 或应用自有的原生 Yjs 传输工作。它不会让 Go RGA、`richtext` v1、`native-ts-v1`、Redis/SQL 日志或 WebRTC bridge 变成 Yjs 线协议。

## 事实与范围

[Yjs 官方概览](https://github.com/yjs/yjs#overview) 将核心共享类型与网络、编辑器模块明确分离。2026-08-03 核对时，官方 provider 列表本身声明并不完整，仍列出至少 22 种连接方案、6 项持久化条目和 20 行编辑器/状态绑定。这是发现索引，不是兼容性或安全认证。

| 面向 | 本仓库现有能力 | Yjs 生态类别 | 采用边界 |
| --- | --- | --- | --- |
| 实时 WebSocket | 有界 Go live relay，以及兼容 y-websocket/y-protocols 的 `extensions.YJSHandler` | y-websocket、Hocuspocus、y-sweet、托管后端 | Yjs 文档走原生 envelope；第三方 provider 直接使用 `Y.Doc`，不经过 RGA frame。 |
| P2P | 有序可靠的 Go CRDT DataChannel bridge | y-webrtc、libp2p、Matrix、Nostr、ATProto、Dat | bridge 继续是 Go 协议专用；身份、信令、重放、持久 receipt、NAT 由宿主负责。 |
| 服务端持久化 | bbolt 参考，以及 Redis/PostgreSQL/MySQL/SQLite `durable.Log` | y-indexeddb、y-leveldb、Mongo/PostgreSQL/Firestore、y-sweet | Go durable log 只保存 Go envelope；Yjs 语义持久化走绑定 tenant/room/epoch/schema/format 的 `YJSStore`。 |
| 离线本地缓存 | Go/Wasm checkpoint 与原生 TS store | y-indexeddb、React Native SQLite | 存储格式不能混用：Yjs 用原生 state vector，RGA 组原子保存 state/frontier/HLC。 |
| 纯文本编辑器 | Quill、CodeMirror、Monaco、ProseMirror/Tiptap port、Slate、Lexical 的 RGA adapter | y-quill、y-codemirror、y-monaco、y-prosemirror 等 | RGA adapter 保持 profile 边界；原生 Yjs 已有 CodeMirror 纯文本，本次增加 Quill Delta。 |
| 富文本编辑器 | Quill、Tiptap/ProseMirror、BlockNote 的 Go/Wasm richtext profile | y-quill、y-prosemirror、原生编辑器整合 | 仅在可逆编辑器契约上加 adapter；任意 schema、HTML/DOM 和厂商扩展一律拒绝。 |

## 架构、正确性与安全

```text
Quill 2 Delta（已批准 schema）
        <-> YjsQuillRichTextBinding
        <-> Y.Text / Y.Doc（每个 room 固定 V1 或 V2）
        <-> 只选一个 transport owner
             -> y-websocket-compatible provider -> extensions.YJSHandler -> 可选 YJSStore
             -> 应用 outbox / 已认证 native Yjs transport

Go RGA / richtext / native-ts group
        <-> 各自 Manifest、state/frontier/HLC 与 durable.Log
```

两条路径不会在编辑器以下汇合。WebSocket callback、DataChannel `Send`、Redis 脚本、SQL commit 或 Yjs update callback 都不是客户端 durable receipt，也不证明其他副本已渲染。

`YjsRichTextBinding` 在本地变更前、远端投影前都校验 Delta：每项只允许一个动作；操作数、文本、属性和嵌入均受限；格式和单键嵌入必须白名单；V1/V2 不猜测；本地 outbox callback 失败后闭锁后续写入；远端未知 schema 或编辑器 callback 失败时冻结投影、不会篡改内容或回滚共享 Y.Doc。

Go relay/store 仍负责连接认证、独立读写授权、origin、队列、文档身份、update/body 上限以及持久化后再 fan-out。Yjs client ID 仅是路由值，不能当作用户身份。

## 多维度判断

| 维度 | 判断 | 行动 |
| --- | --- | --- |
| 实现 | 大多数 Yjs provider 是替代传输或托管产品，不存在共同 provider ABI。 | 只有可测试的 wire、身份与恢复契约才增加 adapter，拒绝空 Go wrapper。 |
| 设计 | Go CRDT 与 Yjs 的状态、恢复和编辑器语义不同。 | Yjs room 保持 native；RGA/richtext/collections 保持 Manifest 协商协议。 |
| 正确性 | 全文字符串转换会丢格式、嵌入、位置和事务意图。 | 直接传递批准的 Quill Delta；不支持内容必须拒绝，绝不 flatten。 |
| 安全 | provider 目录不说明认证、租户隔离、放大攻击、加密或保留策略。 | 必须持续执行 room 授权、pre-decode 限制、schema policy 与 store 准入。 |
| 性能 | 全文替换和 Delta 增量写入成本不同。 | 必须在目标编辑器和浏览器测量，不把本地基准映射为 WAN/TLS/持久化/绘制容量。 |
| 运维 | 托管、联邦和 P2P provider 会改变故障域与支持边界。 | 选型前审查发布节奏、数据地域、备份恢复、限流、webhook 幂等、观测与退出路径。 |

## 推荐顺序

1. 需要 Yjs 互操作时，先用现有 y-websocket 兼容 relay 与 YJSStore 构建有界原生 room。
2. Quill 2 使用 `bindYjsQuillRichText`，把文档 schema 的格式和 embed 精确写入白名单；标准 provider 与手动 outbox 二选一。
3. 第三方 provider 只有在身份、数据地域、持久化、恢复和运维契约被接受后才引入；能传字节不等于受支持。
4. 新增其他编辑器 adapter 前，必须用真实 schema 覆盖本地编辑、远端合并、重复/乱序/重连恢复、未知内容、callback 失败、资源耗尽和浏览器/设备行为。

实现入口见[原生 Yjs 编辑器绑定](../integration/yjs-native-editor-bindings.md)。
