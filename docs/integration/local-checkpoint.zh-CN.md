# 本地 bbolt 检查点参考实现

`persistence` 是本地 CRDT 恢复边界的参考实现。一次 bbolt 事务会保存完整
`snapshot.Snapshot`、该状态对应的 durable-relay 游标，以及应用自有的不透明 outbox。
它补齐 CRDT 状态对象与 [`durable`](durable-provider.zh-CN.md) relay 之间的本地检查点，
但不是多节点数据库。

它适用于一个进程独占一个受保护本地卷、且一个数据库只承载一种具体 CRDT 状态编码的
场景。可运行的 OR-Set 重启流程：

```sh
go run ./examples/persistent-replica
# recovered=true cursor=41 outbox_bytes=24
```

## 恢复边界

```text
本地 mutation -> SnapshotCurrentState -> Save(state, frontier, HLC, cursor, outbox)
                                                       |
                                                       +--> 一次 bbolt commit

重启 -> Load + 具体类型校验 -> NewFromSnapshot -> 重试原始 outbox 字节
```

`Save` 返回 `nil` 只表示 state、frontier、HLC、cursor、outbox 在本地一起提交，并不
表示远端 peer、relay 或其他应用数据库已提交。新建的本地 delta 及其 outbox 表示必须先
跨过你的本地持久化边界，才能按重试策略发布。

在 durable relay 接收回调中，`Cursor` 只能保存为 `Snapshot` 已经包含其效果的最后一个
sequence。较小 cursor 只会安全地重复重放；大于状态的 cursor 会丢失变更。Store 不会
猜测或静默推进这一应用层不变量。

## 配置有类型的 Store

验证器不是装饰：帧校验和不能证明具体 schema/codec，也不能证明安全解码。一个 `Store`
绑定一个具体状态验证函数；不同 CRDT 状态类型或元素 codec 应使用独立 Store，或实施
显式迁移。

```go
store, err := persistence.Open("/var/lib/myapp/tasks.db", persistence.Config{
	MaxRecordBytes:     1 << 20,
	MaxStateBytes:      512 << 10,
	MaxFrontierEntries: 4 << 10,
	MaxReplicaIDBytes:  256,
	MaxOutboxBytes:     64 << 10,
	MaxNameBytes:       128,
	Validate: func(data []byte) error {
		candidate, err := set.NewORSet("validation", taskCodec{})
		if err != nil {
			return err
		}
		limits := frame.DefaultLimits()
		limits.MaxFrameBytes = 512 << 10
		limits.MaxPayload = 512 << 10
		return candidate.UnmarshalBinaryWithLimits(data, limits)
	},
})
if err != nil {
	return err
}
defer store.Close()
```

父目录必须预先存在并由宿主保护，数据库以 `0600` 打开。bbolt 有进程独占锁，同一路径
只能有一个 active process。请按本地 replica/schema 拆分数据库，不能将同一文件挂载给
多个 pod。

## 保存和恢复

```go
saved, err := tasks.SnapshotCurrentState() // OR-Set state、frontier 和 HLC
if err != nil {
	return err
}
if err := store.Save("tasks", persistence.Checkpoint{
	Snapshot: saved,
	Cursor:   durableSequence,
	Outbox:   canonicalPendingPayloads,
}); err != nil {
	return err
}

checkpoint, found, err := store.Load("tasks")
if err != nil || !found {
	return err
}
tasks, err = set.NewORSetFromSnapshot(checkpoint.Snapshot, taskCodec{})
if err != nil {
	return err
}
```

使用 HLC 的协议（OR-Set、LWW、RGA、OR-Tree、list RGA、rich text）不能遗漏 HLC 状态。
`persistence.Save` 会在写入前拒绝这种形状，`Load` 会将其视为损坏，避免重启后复用旧
mutation tag。恢复工厂仍是最终的 type/codec 检查点。

`Outbox` 有意设计为受限的不透明字节，以便应用在同一 bbolt 事务中保存原始 canonical
pending payload，而本包不会擅自定义 transport identity、manifest 或授权语义。发送结果
不明确时必须重试原始字节，不能重新生成 mutation tag。

## 安全与运维

| 边界 | 参考实现行为 | 宿主责任 |
| --- | --- | --- |
| 记录格式 | 版本化确定性记录，SHA-256 发现损坏；畸形、非规范、超限、未知版本均 fail closed。 | 保护数据库和备份不被篡改；摘要不能认证攻击者。 |
| CRDT 状态 | 提交前和加载后运行具体验证器；HLC 协议强制保存时钟状态。 | 使用每个 schema 的 decoder limits，并调用匹配的 `NewFromSnapshot`。 |
| 资源 | 记录、状态、frontier、replica ID、outbox、名称都有显式上限且先检查后分配。 | 按真实文档、actor 数和重试负载选择配额。 |
| 原子性 | 一次 `Update` 同时提交 state、frontier、HLC、cursor、outbox。 | 业务行需进入同一数据库/事务，或采用应用 outbox 协议。 |
| 可用性 | 一个进程、一个本地卷。 | TLS、身份、静态加密、备份、多节点容灾、成员与 tombstone-GC 策略。 |

帧校验和和记录摘要只能发现意外损坏，不能认证恶意 peer。远端输入进入本地状态前必须
通过已认证 manifest 和具体 decoder。检查点也不是 tombstone compaction 的许可：仍要
保留当前 epoch/精确确认、compaction 后的持久化 snapshot 和旧 delta 退役策略。

bbolt 有可串行化 ACID 事务，但只有一个 writer。`Save` 事务应保持短小；不能在事务中
等待网络，也不要期待并发保存提升写吞吐。请备份并恢复测试已关闭的数据库文件，或使用
宿主提供的一致性卷快照。

```sh
go test ./persistence ./examples/persistent-replica
go test -race ./persistence
go test -run='^$' -fuzz=FuzzUnmarshalCheckpoint -fuzztime=20s -parallel=1 ./persistence
go test -run='^$' -bench='BenchmarkStore(Save|Load|SaveParallel)$' -benchmem -benchtime=2s ./persistence
```

这些检查覆盖本地重启、损坏拒绝、并发访问和 fuzz 解码；它们不证明宿主备份、磁盘写满、
TLS、外部身份、多节点存储或生产容量。
