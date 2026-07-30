# 受控压测记录 — 2026-07-29

本文是可复现的开发验证证据，不是生产延迟、吞吐或容量承诺；不记录主机地址、凭据、令牌或业务数据。

## 方法

- RGA 与 `replica.Inbox` 使用 detached 提交 `152838ea30d1b65cef87b248b21ddbf0b1714550`。
- WebSocket provider 使用 SHA-256 为 `8ff2686f3bc431564027c342214220409da32370f433bab4348359a8a13efe59` 的测试二进制；它只新增 `examples/websocket-provider/provider/provider_benchmark_test.go` 中的测试夹具。
- 两台 Debian 13 `linux/amd64` 主机均为 4 个 Intel Xeon Platinum 8272CL vCPU、3.8 GiB 内存。测试二进制由本机 Go 1.26.5 交叉编译，上传至权限为 0700 的临时目录并核验 SHA-256。
- 每个远端单元均为三次 `-benchtime=2s` 的算术均值，分别在指定 `GOMAXPROCS` 下测量。

RGA 夹具为 100,000 rune 的线性文档。`snapshot` 从原始 snapshot bytes 计算 delta，`cached_base` 从已校验的 `SnapshotBase` 计算。

| 工作负载 | 主机 A，G=1 | 主机 A，G=4 | 主机 B，G=1 | 主机 B，G=4 | 每操作分配 |
| --- | ---: | ---: | ---: | ---: | --- |
| RGA state v1 编码 | 125.2 ms | 100.0 ms | 123.6 ms | 101.7 ms | 16,061,696 B；263 allocs |
| RGA state run-v2 编码 | 128.4 ms | 98.6 ms | 125.2 ms | 100.3 ms | 21,386,544 B；266 allocs |
| 从原始 snapshot 生成 RGA delta | 37.9 ms | 29.2 ms | 37.6 ms | 29.6 ms | 约 11.8 MiB；356-371 allocs |
| 从 cached base 生成 RGA delta | 3.44 ms | 3.40 ms | 3.48 ms | 3.43 ms | 110,296 B；25 allocs |
| Inbox 已安装的重复投递 | 246.3 ns | 249.5 ns | 249.5 ns | 249.6 ns | 8 B；1 alloc |

RGA 与 Inbox 循环均为串行循环，故 `GOMAXPROCS=4` 仅是运行时设置样本，不是四核汇总吞吐。本夹具中 cached-base 路径比重新扫描原始 snapshot 快约 11 倍，且没有放宽校验或兼容性要求。

## WebSocket provider 参考实现

`Group` 重复投递基准使用 `RunParallel`，测量单个有界准入锁的竞争。扇出基准只有一个发布者，并等待所有 loopback 观察者完成解码和安装。下表合并两台主机（每格 6 次采样）。

| 工作负载 | G=1 | G=4 | 每操作分配 |
| --- | ---: | ---: | --- |
| 已安装重复 Dot 的并行准入 | 745.4 ns | 783.0 ns | 224 B；7 allocs |
| 端到端中继，1 个观察者 | 52.7 µs | 70.0 µs | 5,607 B；77-78 allocs |
| 端到端中继，4 个观察者 | 102.1 µs | 96.7 µs | 11,007 B；150-151 allocs |
| 端到端中继，16 个观察者 | 311.0 µs | 182.9 µs | 32,505 B；439 allocs |

未开放公网端口：provider handler 和 client 均在关闭压缩的 `httptest` loopback 中运行。因此这些数据不包括 WAN、TLS、外部认证、持久化 outbox/存储、重连、snapshot、反熵或浏览器成本。

## 本机 Node 与 Go-Wasm 样本

本机为 Apple M4 Pro、12 个逻辑 CPU、24 GiB 内存、Go 1.26.5、Node v26.5.0；每项 JavaScript 数据含 5 个样本。

| 工作负载 | 结果 |
| --- | --- |
| TypeScript 解码 512 KiB frame | 492.06-508.43 MiB/s；均值 502.59 MiB/s |
| Node 到 Go-Wasm：单 rune 插入 + apply，38 B frame | 0.090-0.131 ms；均值 0.109 ms |
| Node 到 Go-Wasm：4,096 rune 插入 + apply，187,900 B frame | 60.707-62.125 ms；均值 61.523 ms |

同一提交的本机 Go RGA 采样期间有另一项 fuzz 任务启动，方差明显增大，因此不发布这轮本机 Go 数据；应在空闲机器重新采样后再同两台 Linux 主机比较。

## 复现

```sh
go test -count=1 ./text ./replica ./examples/websocket-provider/provider
go test -race -count=1 ./examples/websocket-provider/provider

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -o text.test ./text
GOMAXPROCS=4 ./text.test -test.run=NO_SUCH_TEST \
  -test.bench='BenchmarkRGAMarshal' -test.benchmem -test.benchtime=2s -test.count=3

make typescript-benchmark
make wasm-benchmark
```
