# Awareness / Presence v1 协议

`awareness` 用于在线状态、昵称、颜色、相对光标等**短生命周期**信息。它不是
CRDT 文档协议：不进入 `ProtocolPolicy`、`replica.Manifest`、`replica.Frontier`、
快照、反熵或 tombstone-GC checkpoint。把它持久化会把“当前在线提示”错误地变成
陈旧业务数据。

Go 实现在 [`awareness`](../../awareness)。参考 WebSocket provider 只有在显式协商
`crdt-sync-v3` 时才启用；既有 v1/v2 CRDT change envelope 完全不变。

## 语义与安全边界

- 每个已认证 actor 独占一个严格递增的无符号 `clock`。
- 一条 update 包含该 actor 的完整 JSON object；更大的 clock 覆盖其全部可见状态，
  而不是逐字段 CRDT merge。
- `remove` 不带 state。store 会在内存保留 actor/clock tombstone，阻止延迟的旧包
  重新显示已离线用户。
- actor、clock、state 全等是幂等重复；同 actor/clock 而 state 不同会拒绝，避免
  到包顺序决定 UI。
- 只有在 TTL 内收到较新的 heartbeat 才算在线（默认 30 秒）。客户端应在 TTL 前
  发出更大 clock 的 heartbeat；正常关闭时应发送 `Remove`。

协议不提供身份、成员资格、权限或保密性。传输层必须先认证连接，再把 update actor
绑定到该 peer 才能转发。JSON 应仅含必要展示信息，绝不能放 token、密钥或敏感资料。

## awareness-v1 二进制布局

无符号整数均使用仓库的最短 uvarint；WebSocket message 提供外部边界。

| 字段 | 编码 | 规则 |
| --- | --- | --- |
| version | 1 byte | `0x01` |
| actor | uvarint 字节长度 + UTF-8 bytes | 非空白且有上限 |
| clock | uvarint | 非零；按 actor 单调递增 |
| status | 1 byte | `0x00` remove；`0x01` online state |
| state | uvarint 字节长度 + bytes | 仅 `0x01`；有上限 JSON object |

online JSON 在保留前会 decode/re-marshal，使 object key 顺序确定。顶层 array、
scalar、`null`、坏 JSON、尾随字节、非规范 varint、未知 status 和任何越界都会拒绝。
默认上限：actor 128 bytes、单 state 16 KiB、单 store 16,384 actors；生产组应按实际
人数和消息预算继续收紧。

## WebSocket provider v3

`crdt-sync-v3` 仅增加一种 binary discriminator，不改变 delta/batch 格式：

```text
0x03 | awareness-v1 update
```

`0x01` change 和 `0x02` change batch 在 v3 中仍保持原 v1/v2 语义。服务端要在
`provider.GroupConfig` 提供 `*awareness.Store`，并在 `Config.AuthorizeAwareness`
实现 actor-to-authenticated-peer 绑定。服务端和客户端只要未实际协商 v3，就会拒绝
awareness 操作。新 v3 peer 完成 manifest handshake 后会收到当前未过期的内存状态；
没有持久化回放。

文本光标建议在 JSON 中传应用自定义的 `text.Anchor` 编码。只有 actor 授权后才能验证
并调用 `text.ResolveAnchor`；遇到未知/已 compact 的 anchor 必须清空光标，不能猜测
绝对 offset。

## 非目标

- 与 Yjs awareness wire 兼容。二者的 provider envelope、认证模型和锚点不同；如需
  转换，应由应用网关单独审查身份与权限。
- last-seen 历史、审计、成员协议。它们需要独立的保留、隐私、授权和时钟策略。
- 自动重连、outbox、可靠投递。presence 本来就是 best-effort，下一次 heartbeat 会
  覆盖丢失状态。
