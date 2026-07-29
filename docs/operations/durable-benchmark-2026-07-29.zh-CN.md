# Durable relay 基准测试 — 2026-07-29

这是 `durable` 参考实现的受控本地证据，不是 SLA，也不能直接推导生产容量。

## 环境

| 项目 | 值 |
| --- | --- |
| 日期 | 2026-07-29 |
| 主机 | Apple M4 Pro，darwin/arm64 |
| Go | go1.26.5 |
| 存储 | bbolt v1.3.10，默认同步事务设置 |
| WebSocket | github.com/coder/websocket v1.8.13 |
| 命令 | `go test -run='^$' -bench='Benchmark(DurableAppend|DurableReplay|ReconnectHandshakeLoopback)$' -benchtime=1s -benchmem ./durable` |

## 结果

| 基准 | 工作负载 | 结果 | 分配 |
| --- | --- | ---: | ---: |
| `BenchmarkDurableAppend` | 一个规范 G-Counter delta、bbolt append 事务与 sync | 8,064,225 ns/op | 57,277 B/op；133 allocs/op |
| `BenchmarkDurableReplay` | 从本地存储重放 256 个持久化规范事件 | 38,759 ns/op | 99,856 B/op；1,052 allocs/op |
| `BenchmarkReconnectHandshakeLoopback` | high-water 处真实本地 WebSocket hello/resume 握手 | 162,732 ns/op | 43,270 B/op；334 allocs/op |

重放基准会为全部 256 个事件构造独占的 `replica.Change`；分配数刻意包含安全字节所有权与 Manifest 校验，不能据此声称存储层零拷贝。append 基准包含了该参考实现“一事件一事务”的持久化取舍。

## 解读与边界

- bbolt 参考实现只有一个 writer。这些数字不代表多个进程能够安全共享同一个文件，也不代表写入吞吐可水平扩展。
- 测试未包含 TLS、ingress/WAF、token 校验、真实网络、具体 CRDT Apply、客户端 checkpoint 事务、磁盘竞争、备份流量和多租户配额。
- 留存、重放、队列与输入上限仍是硬边界。只有在目标存储和部署平台上复现完整工作负载与故障测试后，才可以提高上限。
- 生产发布仍需 restore drill、存储延迟/错误指标、队列/重放拒绝指标，以及对超出重放窗口客户端的已认证 checkpoint bootstrap 路径。
