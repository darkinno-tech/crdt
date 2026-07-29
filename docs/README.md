# Documentation

Public library documentation is grouped by purpose. Internal notes belong under
`docs/internal/` or `docs/fault-reviews/`; both paths are ignored and must not
be committed.

## Integration

- [End-to-end integration tutorial](integration/overview.md)
- [端到端集成教程](integration/overview.zh-CN.md)
- [Durable WebSocket relay reference](integration/durable-provider.md)
- [可持久化 WebSocket relay 参考实现](integration/durable-provider.zh-CN.md)
- [Attachment reference integration](integration/attachment.md)
- [附件引用集成](integration/attachment.zh-CN.md)

## Design and protocol

- [CRDT extension design](design/crdt-extension.md)
- [Durable transport reference design](design/durable-transport.md)
- [G-Set and MV-Register design](design/gset-mvregister.md)
- [Membership protocol](protocol/membership.md)

## Operations and architecture

- [Cross-host probe deployment runbook](operations/cross-host-probe.md)
- [跨机器同步探针部署手册](operations/cross-host-probe.zh-CN.md)
- [System context architecture (SVG)](assets/architecture.svg)
- [System context architecture (PNG)](assets/architecture.png)

Repository entry points remain in [README](../README.md),
[中文 README](../README.zh-CN.md), [CHANGELOG](../CHANGELOG.md), and
[CONTRIBUTING](../CONTRIBUTING.md).
