# 本地检查点基准 — 2026-07-30

这是当前工作版本中 `persistence` 参考实现的受控开发证据。它测量本地 bbolt 文件，
不代表集群数据库、网络、TLS、外部身份或生产容量。

## 方法

- 主机：Apple M4 Pro，`darwin/arm64`，Go 1.26.5。
- 夹具：包含三个短元素、HLC state/frontier、cursor `41` 和 24 字节不透明 outbox 的 OR-Set checkpoint。
- `Save` 在真实临时本地 bbolt 文件中替换一个命名记录；`Load` 读取并运行具体 OR-Set 验证器。
- `SaveParallel` 用 `RunParallel` 写同一记录，刻意展示单 writer 的串行成本，不代表聚合扩展性。
- 每个设置三次样本，`-benchtime=2s`，`-benchmem`。

```sh
GOMAXPROCS=1 go test -run='^$' -bench='BenchmarkStore(Save|Load|SaveParallel)$' -benchmem -benchtime=2s -count=3 ./persistence
GOMAXPROCS=4 go test -run='^$' -bench='BenchmarkStore(Save|Load|SaveParallel)$' -benchmem -benchtime=2s -count=3 ./persistence
```

## 结果

| Benchmark | GOMAXPROCS=1 均值 | GOMAXPROCS=4 均值 | 分配 |
| --- | ---: | ---: | --- |
| `Save` | 6.77 ms/op | 6.65 ms/op | 23.3 KB/op；81 allocs/op |
| `Load` | 2.27 µs/op | 2.00 µs/op | 4,040 B/op；44 allocs/op |
| `SaveParallel` | 6.51 ms/op | 6.68 ms/op | 约 23.3 KB/op；81 allocs/op |

`Save` 和 `SaveParallel` 接近是预期行为：bbolt 同一时刻只允许一个读写事务。`GOMAXPROCS=4`
略微改善串行 `Load` 样本，不会把本地 Store 变成多 writer 系统。设置配额或延迟目标前，
必须在生产磁盘上使用代表性的 state、frontier、outbox、备份与故障注入负载重测。

## 恢复模拟

`TestThreeReplicaCheckpointRecoverySimulation` 使用真实临时 bbolt 文件：一个 mobile OR-Set
replica 在分区时持久化 state、HLC、cursor 和原始 outbox 字节；进程重开后，同一 replica ID
发出新 mutation，随后收到远端 delta，三个 replica 最终收敛。它验证 CRDT 恢复行为，
不证明远端 relay、身份、多卷或备份验收。
