# 本地 checkpoint Store 参考实现

`persistence` 是本地 CRDT 恢复边界的参考实现。其 `Store` 契约将完整
`snapshot.Snapshot`、该状态对应的 durable-relay 游标，以及应用自有的不透明 outbox
作为一个持久化边界保存。`BoltStore` 使用 bbolt 事务，`FileStore` 使用私有文件原子替换。
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
                                                       +--> 一次 Store 持久化边界

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
config := persistence.Config{
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
}
var store persistence.Store
store, err := persistence.Open("/var/lib/myapp/tasks.db", config) // bbolt
if err != nil {
	return err
}
defer store.Close()
```

### 显式加载有界配置

宿主应用可以通过不可变、分层的 `config.Loader` 构造同一份强类型配置；构造函数自身仍不会
隐式读取环境变量。所有容量仍必须显式提供；bbolt 锁超时和格式策略才有文档化默认值。若环境
source 使用 `CRDT_` 前缀，则必须提供 `CRDT_PERSISTENCE_MAX_RECORD_BYTES`、
`CRDT_PERSISTENCE_MAX_STATE_BYTES`、`CRDT_PERSISTENCE_MAX_FRONTIER_ENTRIES`、
`CRDT_PERSISTENCE_MAX_REPLICA_ID_BYTES`、`CRDT_PERSISTENCE_MAX_OUTBOX_BYTES` 与
`CRDT_PERSISTENCE_MAX_NAME_BYTES`。

```go
environment, err := config.NewEnvironment("CRDT_")
if err != nil {
	return err
}
loader, err := config.New(environment)
if err != nil {
	return err
}
config, err := persistence.ConfigFrom(loader, validateTasks)
if err != nil {
	return err
}
store, err := persistence.Open("/var/lib/myapp/tasks.db", config)
```

`FileStore` 还必须提供 `CRDT_PERSISTENCE_MAX_STORE_BYTES` 并调用
`persistence.FileConfigFrom`。`PERSISTENCE_FORMAT_VERSION`、
`PERSISTENCE_FORMAT_COMPATIBILITY`（`current` 或 `current-and-previous`）以及
`PERSISTENCE_MIGRATE_ON_LOAD` 可省略。validator 和可执行 migration 仍应作为
`ConfigFrom`/`FileConfigFrom` 的代码参数；绝不能从环境配置中读取或编码它们。

## 记录格式兼容与迁移

`Config.Format` 是本地 checkpoint 外层记录格式的唯一策略入口。新 Store 默认写入
`RecordFormatV2`，也默认可读取紧邻的 v1 记录。这个兼容性只覆盖本地 checkpoint 记录；
它不使 CRDT frame 版本、TypeID、codec 或 Manifest 变得可互换。

对于只变更外层格式的升级，请在已验证备份后启用事务性重写。v1 和 v2 checkpoint 的
payload 语义相同，因此不需要自定义 transform：

```go
config.Format = persistence.FormatConfig{
	Version:       persistence.RecordFormatV2,
	Compatibility: persistence.CompatibilityCurrentAndPrevious,
	MigrateOnLoad: true,
}
```

`Load` 会先验证记录摘要、所有已配置的字节/数量上限、source validator 和 CRDT frame，
才调用迁移。随后它使用 `Config.Validate` 验证替换后的 checkpoint，并在同一个 bbolt 写事务
或一次原子文件替换中重写。transform 失败会返回 `persistence.ErrMigration`，原始记录保持
不变。回滚窗口关闭后可设为 `CompatibilityCurrentOnly`，以拒绝旧文件。

若 CRDT schema 或 codec 也发生变化，请为旧记录版本提供一个 `Migration`。其可选
`Validate` 校验 source format，`Transform` 必须返回能通过 `Config.Validate` 的完整目标
checkpoint。transform 必须有界且确定；不得据此推断身份、授权输入或改写在线远程 CRDT 流量。


若要使用无额外依赖的文件参考实现，必须设置完整文件的显式上限。它会在打开和每次读取时
验证每一条记录，写入 `0600` 临时文件并 `sync`，原子 `rename` 后再 `sync` 父目录：

```go
store, err = persistence.OpenFile("/var/lib/myapp/tasks.store", persistence.FileConfig{
	Config:        config,
	MaxStoreBytes: 4 << 20,
})
```

父目录必须预先存在并由宿主保护。两种后端都要求同一路径只能有一个 active process。
bbolt 额外持有操作系统文件锁；文件参考实现只使用进程内 mutex，无法检测第二个进程。
请按本地 replica/schema 拆分存储，不能将同一文件挂载给多个 pod。

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

## 退役本地 checkpoint

`Delete` 是幂等的：如果指定 checkpoint 已不存在，返回 `found=false, nil`。它只会原子
删除这一个本地恢复边界；只能在宿主的保留/重新加入策略确认该 replica 不再需要它时调用。
它**不会**确认 peer、退役 durable relay event，也不会让 CRDT tombstone 获得 GC 资格。

```go
deleted, err := store.Delete("retired-device")
if err != nil {
	return err
}
if deleted {
	// 应用此时只能删除自己对应的本地元数据。
}
```

使用 HLC 的协议（OR-Set、LWW、RGA、OR-Tree、list RGA、rich text）不能遗漏 HLC 状态。
`persistence.Save` 会在写入前拒绝这种形状，`Load` 会将其视为损坏，避免重启后复用旧
mutation tag。恢复工厂仍是最终的 type/codec 检查点。

`Outbox` 有意设计为受限的不透明字节，以便应用在同一 Store 持久化边界中保存原始 canonical
pending payload，而本包不会擅自定义 transport identity、manifest 或授权语义。发送结果
不明确时必须重试原始字节，不能重新生成 mutation tag。

## 安全与运维

| 边界 | 参考实现行为 | 宿主责任 |
| --- | --- | --- |
| 记录格式 | 版本化确定性记录，SHA-256 发现损坏；畸形、非规范、超限、未知版本均 fail closed。 | 保护数据库和备份不被篡改；摘要不能认证攻击者。 |
| CRDT 状态 | 提交前和加载后运行具体验证器；HLC 协议强制保存时钟状态。 | 使用每个 schema 的 decoder limits，并调用匹配的 `NewFromSnapshot`。 |
| 资源 | 记录、状态、frontier、replica ID、outbox、名称都有显式上限且先检查后分配。 | 按真实文档、actor 数和重试负载选择配额。 |
| 原子性 | `BoltStore` 的一次 bbolt `Update`，或 `FileStore` 写入/同步/替换完整私有文件；两者都一起提交 state、frontier、HLC、cursor 和 outbox。 | 业务行需进入同一数据库/事务，或采用应用 outbox 协议。 |
| 可用性 | 一个进程和一个本地卷；文件存储没有进程间锁。 | TLS、身份、静态加密、备份、多节点容灾、成员与 tombstone-GC 策略。 |

帧校验和和记录摘要只能发现意外损坏，不能认证恶意 peer。远端输入进入本地状态前必须
通过已认证 manifest 和具体 decoder。检查点也不是 tombstone compaction 的许可：仍要
保留当前 epoch/精确确认、compaction 后的持久化 snapshot 和旧 delta 退役策略。

bbolt 有可串行化 ACID 事务，但只有一个 writer。`FileStore` 每次保存都会重写完整的有界
文件，因此较大的 checkpoint 集或高写入速率应优先使用 bbolt。保存边界应保持短小；不能在
其中等待网络，也不要期待并发保存提升写吞吐。请备份并恢复测试已关闭的数据库文件，或使用
宿主提供的一致性卷快照。

```sh
go test ./persistence ./examples/persistent-replica
go test -race ./persistence
go test -run='^$' -fuzz=FuzzUnmarshalCheckpoint -fuzztime=250000x -parallel=1 ./persistence
go test -run='^$' -fuzz=FuzzUnmarshalFileRecords -fuzztime=250000x -parallel=1 ./persistence
go test -run='^$' -bench='Benchmark((File)?Store(Save|Load|SaveParallel|Delete|LoadLegacyMigration)|(File)?ConfigFromLoader)$' -benchmem -benchtime=2s ./persistence
```

这些检查覆盖本地重启、损坏拒绝、并发访问和 fuzz 解码；它们不证明宿主备份、磁盘写满、
TLS、外部身份、多节点存储或生产容量。
