# 受控性能回归 CI

`.github/workflows/test.yml` 中的 `performance` 任务在同一 GitHub runner 上
比较候选提交与 PR 基线（或 `beta` 推送前的提交）。它是回归门禁，不是生产容量
承诺；设定产品限额前，必须在目标 CPU、磁盘、Go 版本、网络和工作负载上重跑聚焦
基准。

## 覆盖的工作负载

| 层级 | 基准 | 单次操作证明的内容 |
| --- | --- | --- |
| CRDT 数据面 | `BenchmarkGCounterApplyDelta` | 一次幂等的内存 delta 应用。 |
| 大文档 | `BenchmarkRGAApplyDeltaLinearChain` | 将有界的 100,000 节点 RGA delta 应用到新状态。 |
| 传输 | `BenchmarkProviderEndToEndRelayFanout` | 一次已认证的 loopback WebSocket 发布被 1、4、16 个接收端解码并安装。 |

每个工作负载以 `GOMAXPROCS=1`、`-cpu=1`、`-benchmem`、`-benchtime=100ms`
运行五次。检查器要求全部五个样本并比较中位数：`ns/op` 增加到两倍以上，或
`B/op`、`allocs/op` 增加超过 5% 时失败。托管 runner 的真实传输场景对调度敏感，
因此耗时门槛刻意保持粗粒度；分配上的余量允许无害的运行时波动，同时会拒绝实质性
的保留工作量增长。

基线和候选必须暴露完全相同的基准名称，避免重命名、跳过或新增子基准时静默削弱
门禁。

## 本地复现

将待对比的提交检出到候选目录旁边，然后运行：

```sh
BENCHMARK_BASE=../crdt-baseline make benchmark-regression
```

原始基线和候选输出保存在 `.tmp/benchmark-results/`。若要形成测量报告而不是运行
门禁，请使用对应集成或运维指南中的聚焦 `go test -bench` 命令，并记录机器和工作
负载。
