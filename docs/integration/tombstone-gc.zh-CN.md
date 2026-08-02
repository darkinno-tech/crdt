# Tombstone GC 模式选择

`tombstonegc.Coordinator` 仍是默认的 tombstone GC。它是复制或持久化 CRDT
数据的唯一适用模式：当前经过认证的成员纪元内，每个活跃成员都必须确认每一个精确
tag；压缩后的状态必须持久化为 checkpoint；旧 delta 必须退役，成员才可以重新加入。

`tombstonegc.SimpleCollector` 是一个刻意限定为“仅本地”的例外，适合可丢弃、单一
权威方的状态，例如推荐缓存，或由服务端当前源数据可重建的默认值。它省去了 receipt
记录、成员流量和逐成员确认状态；它不会放宽复制协议，也不会变更任何 frame。

## 选择模式

| 维度 | `Coordinator`（默认） | `SimpleCollector`（显式 opt-in） |
| --- | --- | --- |
| 适用数据 | 共享、持久化、可离线或业务关键的 CRDT 状态 | 可丢弃的本地缓存、可重建且由服务端单方拥有的派生状态 |
| 回收前证据 | 当前纪元中每个活跃成员对每个精确 tag 的认证确认 | 宿主证明不存在会合并的延迟操作、outbox、备份恢复或重新加入副本 |
| 延迟投递后的正确性 | 延迟 add 不会复活已回收的 delete | 延迟 add 可能复活；因此禁止用于复制状态 |
| 安全边界 | 宿主认证 membership 与 receipt；协调器隔离旧纪元 | 没有成员或 receipt 认证；不代表 peer 可信或拥有授权 |
| 单次回收资源形态 | checkpoint prune 前，精确确认状态随成员/tag 覆盖度增长 | 无确认状态；`MaxBatch` 限制选中的回收 tag，但 target 的枚举成本仍由 target 决定 |
| 结构型 CRDT | 精确证明后调用 eligible compactor | 使用相同的结构检查，pending/非叶子 anchor 仍会保留；这不产生复制安全证明 |

不能因为当前恰好只有一个成员就选择简单模式。只要存在 durable outbox、未来的第二台
设备、可回放日志，或能够恢复旧 CRDT 状态的备份，就必须使用严格模式。

## 仅本地流程

先确认并记录仅本地生命周期：旧操作会和 target 一起丢弃；恢复时从权威源重新构建，
而不是合并历史 CRDT delta。然后选择一个显式且有界的 policy。

```go
collector, err := tombstonegc.NewSimpleCollector(tombstonegc.DefaultSimplePolicy())
if err != nil {
	return err
}

// recommendationCache 是本地、可丢弃状态：它不接收远端或重放的 CRDT
// delta，重启后重新构建而不是 merge。
removed, err := collector.Collect(recommendationCache)
if err != nil {
	return err
}
metrics.RecordTombstonesCollected(removed)
```

`DefaultSimplePolicy` 会保留 256 个 tombstone identity，每次最多选择 64 个。
`MinRetained` 是数量，不是基于删除时间的保留承诺：规范化 CRDT tag 的顺序并不总等于
观察到删除的时间。`MaxBatch` 可取 1 到 8,192；它限制 compaction request，但不能避免
target 自身 `TombstoneTags()` 的枚举成本。

应在有界维护路径中运行回收，并监控 retained tombstone 数、removed 数、调用耗时、错误
及重建次数。若 target 报告 unresolved 或非叶子结构 tombstone，应保留并仅在本地状态
能够安全推进结构时重试。

## 严格流程不变

只要 CRDT 跨越进程、设备、durable log 或备份边界，就使用 `Coordinator`。应用必须认证
membership view 和每个 receipt，在 prune receipt state 前持久化压缩后的 checkpoint，退役
旧 delta，并要求已退役成员从该 checkpoint bootstrap。请以[成员协议](../protocol/membership.md)
作为与传输无关的参考实现。

两种模式都不会认证 CRDT frame、加密数据、建立业务授权，也不会把 checksum、frontier 或
Merkle root 变成删除 receipt。
