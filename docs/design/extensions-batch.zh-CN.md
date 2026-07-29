# 可选 WebSocket 批量传输设计

[English](extensions-batch.md)

## 状态与范围

WebSocket 批量传输是 live relay 的可选功能。默认关闭，必须依赖既有的 WebSocket 功能，
并通过 `crdt-sync-v2` 子协议扩展传输；不会修改 CRDT frame、Manifest、HTTP 或 SSE 契约。

该扩展仍不是持久化复制服务。它不创建确认回执、重放历史、操作日志、outbox 或原子应用事务。

## 契约

一个 batch 是有边界、有顺序的完整 `crdt-sync-v1` change envelope 列表。每个条目保留自己的
replica dot。支持 v2 的客户端先提供 v2、再提供 v1；v1-only relay 仍可连接，但批量发布会
明确返回不支持错误。

relay 会在第一次 Inbox mutation 前校验完整 batch envelope、每个绑定 Manifest 的 change 以及
每个授权决定，随后在既有的 group 顺序锁内逐项接收。

它刻意不是原子操作。通用的应用 Apply callback 没有回滚接口，因此后续条目可能在此前条目
已被接受后失败。此时 relay 会先转发已接受前缀，再结束发布连接。调用方必须将每个源条目
保留在 durable outbox 中，并逐项重试；已接受 dot 的重试安全且不会再次 fan-out。

## 安全与容量

- feature 开关只在构造 handler 时生效；单独开启 batch 而未开启 WebSocket 会被拒绝。
- batch 同时受总消息字节数和条目数限制。
- 默认上限为 16 项，且不得超过每 peer 的排队消息上限。legacy WebSocket 或 SSE peer
  要么完整排入一个 batch 对应的单条消息，要么在应用层部分入队之前被断开。
- 支持 batch 的 WebSocket peer 只排入一个有界 batch 消息；既有每 peer 字节上限仍然生效。
- HTTP 发布与 SSE 事件保持单个 v1 envelope，不改变既有公共契约或浏览器行为。
- 认证、写授权、读授权、严格 Origin、关闭压缩以及应用负责的持久化恢复边界均不改变。

## 验证矩阵

- wire 往返、数量上限、畸形或截断输入以及 fuzz。
- v2 发布者同时向 v2 和 v1 WebSocket peer 投递。
- 对 v1-only relay 回退并显式拒绝 batch。
- 预校验授权失败时不发生 mutation 或广播。
- 接受前缀后出现动态失败，证明前缀会被转发且重复重试不会再转发。
- 多消息 peer queue 的原子入队、race 检查和以逻辑变更而非原始 batch 归一化的 loopback 基准。
