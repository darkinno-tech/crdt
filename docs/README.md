# Documentation

Public library documentation is grouped by purpose. Internal notes belong under
`docs/internal/` or `docs/fault-reviews/`; both paths are ignored and must not
be committed.

## Start here

- [Developer getting-started guide](getting-started.md)
- [开发者入门指南](getting-started.zh-CN.md)
- [Minimal bounded-delivery example](../examples/getting-started)
- [最小有界投递示例](../examples/getting-started)

## Integration

- [End-to-end integration tutorial](integration/overview.md)
- [端到端集成教程](integration/overview.zh-CN.md)
- [Local bbolt checkpoint reference](integration/local-checkpoint.md)
- [本地 bbolt 检查点参考实现](integration/local-checkpoint.zh-CN.md)
- [Durable WebSocket relay reference](integration/durable-provider.md)
- [可持久化 WebSocket relay 参考实现](integration/durable-provider.zh-CN.md)
- [Browser and provider architecture](integration/provider-architecture.md)
- [WebSocket provider reference](integration/websocket-provider.md)
- [WebSocket Provider 参考实现](integration/websocket-provider.zh-CN.md)
- [Attachment reference integration](integration/attachment.md)
- [附件引用集成](integration/attachment.zh-CN.md)
- [Application change observation](integration/observe.md)
- [应用层变更观察](integration/observe.zh-CN.md)

## Design and protocol

- [CRDT extension design](design/crdt-extension.md)
- [Durable transport reference design](design/durable-transport.md)
- [G-Set and MV-Register design](design/gset-mvregister.md)
- [Membership protocol](protocol/membership.md)
- [LWW Set and Map v1 wire protocol](protocol/lww-v1.md)
- [Generic list RGA v1 wire protocol](protocol/list-rga-v1.md)
- [Scalar RGA v1 wire protocol](protocol/rga-scalar-v1.md)
- [RGA run-v2 wire protocol](protocol/rga-run-v2.md)
- [RGA run-v2 线协议](protocol/rga-run-v2.zh-CN.md)
- [Awareness / presence v1](protocol/awareness-v1.md)
- [Awareness / presence v1 协议](protocol/awareness-v1.zh-CN.md)
- [Rich-text v1 wire protocol](protocol/richtext-v1.md)
- [Observed-remove tree v1 wire protocol](protocol/or-tree-v1.md)
- [Bounded rich-text inline-format design](design/rich-text.md)

## Operations and architecture

- [Production configuration, errors, and telemetry](operations/production-readiness.md)
- [Merkle state-repair CLI runbook](operations/merkle-sync-cli.md)
- [Merkle 状态修复 CLI 手册](operations/merkle-sync-cli.zh-CN.md)
- [Cross-host probe deployment runbook](operations/cross-host-probe.md)
- [跨机器同步探针部署手册](operations/cross-host-probe.zh-CN.md)
- [Durable relay benchmark](operations/durable-benchmark-2026-07-29.md)
- [Durable relay 基准测试](operations/durable-benchmark-2026-07-29.zh-CN.md)
- [Local checkpoint benchmark](operations/local-checkpoint-benchmark-2026-07-30.md)
- [本地检查点基准](operations/local-checkpoint-benchmark-2026-07-30.zh-CN.md)
- [Controlled benchmark evidence — 2026-07-29](operations/benchmark-2026-07-29.md)
- [受控压测记录 — 2026-07-29](operations/benchmark-2026-07-29.zh-CN.md)
- [Historical cross-device RGA baseline — 2026-07-29](operations/cross-device-rga-2026-07-29.md)
- [WebSocket batch latency evidence — 2026-07-29](operations/websocket-batch-latency-2026-07-29.md)
- [System context architecture (SVG)](assets/architecture.svg)
- [System context architecture (PNG)](assets/architecture.png)

Repository entry points remain in [README](../README.md),
[中文 README](../README.zh-CN.md), [CHANGELOG](../CHANGELOG.md), and
[CONTRIBUTING](../CONTRIBUTING.md).
