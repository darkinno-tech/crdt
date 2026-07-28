# crdt

[English](README.md) | [简体中文](README.zh-CN.md)

`crdt` 是一个小巧、无第三方依赖、可组合的 Go 状态型 CRDT 库。
它提供确定性的二进制状态帧与增量帧，使副本能在重复投递、乱序和暂时
网络分区的情况下收敛。

> 状态：首个公开模块版本为 `v1.0.0`；API 遵循语义化版本规范。

## 特性

- 基于状态的 **G-Counter**，具有可合并且类型隔离的增量。
- 使用调用方自定义元素编解码器、带可合并增量的仅增长 **G-Set**。
- 支持调用方自定义元素编解码器的加法胜出（add-wins）观察移除 **OR-Set**。
- 因果复制的 **MV-Register**：保留并发的不透明字节写入，而非用墙上时钟裁决。
- 处于实验阶段、支持 Delta 复制的 **LWW-Map**，使用不透明字节值和确定性的 HLC 冲突决议。
- 混合逻辑时钟（HLC）标签和可持久化的时钟状态，支持副本重启。
- 规范化、带校验和的二进制帧；解码有边界且编码确定。
- 增量批处理/合并、版本化快照与用于反熵的 Merkle 摘要。
- 带成员纪元的可选精确确认墓碑回收。
- 所提供 CRDT 实现均支持安全的并发访问。
- 实验性 LWW-Map、RGA 文本与 OR-Tree 集合；仅能通过每个复制组的显式协议策略启用。

## 范围

本库提供 CRDT 数据类型和线协议基础组件。它不负责选择网络传输、成员协议、
认证、存储后端或重试策略。只有当应用提供权威且经过认证的活跃成员视图后，
`tombstonegc.Coordinator` 才会安全地执行自动回收；它不发现、认证或持久化该
成员视图。校验和只能检测意外的帧损坏，不能提供真实性校验或加密。

## 实验性 LWW-Map、RGA 与 OR-Tree 协议

LWW-Map（`lww.Map`）、RGA 文本（`text`）和 OR-Tree（`tree`）已具备确定性的
状态/增量帧、有边界的解码，以及带 HLC 的快照恢复；但在稳定发布前仍属于实验性
能力，其 API 和墓碑生命周期仍可能调整。复制组必须显式选择并在交换这些帧前通告
协议集合：

```go
policy := crdt.ProtocolPolicy{AllowExperimental: true}
for _, kind := range policy.FrameTypes() {
	// 在经过认证的连接握手中包含 StateID 和 DeltaID。
	_ = kind
}
```

零值策略仅通告稳定的 G-Counter、G-Set、OR-Set、MV-Register 和 PN-Counter 协议。该策略既不是全局
开关，也不是插件注册机制：未知或仅预留的帧类型仍不受支持。LWW-Map、RGA 和
OR-Tree 的实验使用者必须原子持久化 HLC 状态和快照，并保留墓碑；这些类型的精确
确认式墓碑回收尚未实现。

## 要求

- Go 1.21 或更高版本

## 安装

首个公开发布标签可用后：

```sh
go get github.com/DarkInno/crdt@v1.0.0
```

在此之前，请使用本地检出进行开发：

```sh
git clone https://github.com/DarkInno/crdt.git
cd crdt
go test ./...
```

## 快速开始

### G-Counter

每个副本仅递增自己的分量。`Merge` 取每个副本分量的最大值，因此满足
可交换性、结合性和幂等性。

```go
package main

import (
	"fmt"
	"log"

	"github.com/DarkInno/crdt/counter"
)

func main() {
	left, err := counter.NewGCounter("left")
	if err != nil {
		log.Fatal(err)
	}
	right, err := counter.NewGCounter("right")
	if err != nil {
		log.Fatal(err)
	}

	if _, err := left.Increment(2); err != nil {
		log.Fatal(err)
	}
	if _, err := right.Increment(3); err != nil {
		log.Fatal(err)
	}
	if err := left.Merge(right); err != nil { // 投递顺序不影响结果。
		log.Fatal(err)
	}

	value, err := left.Value()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(value)
	// Output: 5
}
```

### PN-Counter

PN-Counter 支持独立递增与递减。它将每个副本的正、负分量分别保存为
G-Counter，因此合并仍满足可交换性、结合性和幂等性。`Value` 返回精确的
`*big.Int`；需要有界机器整数时使用 `ValueInt64`。

```go
counter, err := counter.NewPNCounter("cart")
if err != nil {
	log.Fatal(err)
}
if _, err := counter.Increment(7); err != nil {
	log.Fatal(err)
}
if _, err := counter.Decrement(2); err != nil {
	log.Fatal(err)
}
value, err := counter.Value()
if err != nil {
	log.Fatal(err)
}
fmt.Println(value)
// Output: 5
```

### OR-Set 增量复制

OR-Set 使用稳定的编解码器 ID 和稳定的元素编码字节，以便在不同副本间识别
同一元素类型。

```go
package main

import (
	"fmt"
	"log"

	"github.com/DarkInno/crdt/set"
)

type stringCodec struct{}

func (stringCodec) ID() string                            { return "example.com/string/v1" }
func (stringCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (stringCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }

func main() {
	codec := stringCodec{}
	left, err := set.NewORSet("left", codec)
	if err != nil {
		log.Fatal(err)
	}
	right, err := set.NewORSet("right", codec)
	if err != nil {
		log.Fatal(err)
	}

	delta, err := left.Add("item")
	if err != nil {
		log.Fatal(err)
	}
	if err := right.ApplyDelta(delta); err != nil {
		log.Fatal(err)
	}

	fmt.Println(right.Contains("item"))
	// Output: true
}
```

要使移除操作被其他副本观察到，应发送返回的移除增量或合并状态。未观察到某个
标签的移除与该标签的新增并发时，元素仍然存在（加法胜出语义）。

## 端到端集成

可复现的本地 HTTP 投递演练、生产集成检查清单、快照/重启指引，以及应当采集的
收敛证据，见[端到端集成教程](INTEGRATION.zh-CN.md)。
[可运行的协作任务看板示例](examples/collaborative-board)演示重复投递、网络分区期间
的 add/remove 冲突，以及从 OR-Set 快照恢复：

```sh
go run ./examples/collaborative-board
```

英文版本见 [integration tutorial](INTEGRATION.md)。

## 在分布式系统中的正确使用方式

- 为每个存活逻辑副本提供全局唯一且非空的副本 ID。
- 原子地持久化 OR-Set 快照及其 HLC 状态。当集合自身 frontier 已足够时使用
  `ORSet.SnapshotCurrentState()`；当复制层具有更广泛的确认 frontier 时使用
  `ORSet.Snapshot(frontier)`；通过 `NewORSetFromSnapshot` 恢复。不要仅从字节
  恢复一个使用相同 ID 的 OR-Set。
- 若要自动回收墓碑，请使用稳定的复制组 ID 创建协调器。每个活跃成员都必须在
  该 ID 与当前 `tombstonegc.Coordinator` 成员纪元下报告精确的
  `ORSet.TombstoneTags()`；对每个收到的报告，将两个值传给
  `AcknowledgeAndCompact`。当增量可能乱序投递时，不要从 `Frontier()` 推导
  确认：最大标签并不能证明更早墓碑已经收到。移除成员前必须让其退出复制；重新
  加入的成员必须从回收后的快照启动。
- `ORSet.Compact` 仅适用于传输层能够独立证明所提供 frontier 对每个副本都是
  无缺口因果前缀的场景。
- 在复用 MV-Register 的副本 ID 前持久化其状态快照。其版本向量而非墙上时钟可证明
  后续 `Set` 已观察哪些写入；使用 `register.NewMVRegisterFromSnapshot` 恢复。
- 将 `ProtocolPolicy.FrameTypes()` 作为经过认证的连接/建链能力通告。只有两端
  都选择实验协议时才能发送 LWW-Map、RGA 或 OR-Tree 帧；应原子持久化其带 HLC
  的快照，且暂时不要回收其墓碑。
- 保持 `ElementCodec.ID`、`Marshal` 与 `Unmarshal` 确定性，并确保它们可安全
  并发调用。编码值必须以规范形式往返。
- 将收到的字节视为不可信数据。请根据传输环境使用带合适限制的
  `UnmarshalBinaryWithLimits` 与 `Unmarshal*DeltaWithLimits`。
- 在外围应用中完成消息认证、授权、加密、重试和持久化。CRDT 收敛本身不提供
  这些保证。

## JSON 诊断输出

具体 CRDT 状态和 delta 对象实现了 `json.Marshaler`，可用于结构化日志和人工查看。例如：

```json
{"type":"gcounter","replica_id":"left","element_count":2,"tombstone_count":0}
```

该输出刻意不包含应用值、元素键、标签、时钟状态或二进制帧。JSON 诊断结果不能恢复
或应用 CRDT 状态/delta，也不是复制格式；复制和持久化仍应使用有界、规范的二进制编码。

## 包

| 包 | 作用 |
| --- | --- |
| `crdt` | 通用契约、状态摘要与变更标签。 |
| `clock` | 混合逻辑时钟和持久化 HLC 状态。 |
| `counter` | G-Counter、PN-Counter 及其增量编解码器。 |
| `set` | G-Set、加法胜出 OR-Set 与元素编解码器契约。 |
| `lww` | 内存 LWW-Set 与实验性的、带帧 LWW-Map。 |
| `text` | 实验性、带帧的 RGA 协作文本。 |
| `tree` | 实验性、带帧的观察移除树。 |
| `register` | 内存内 LWW/max register，以及带帧的因果 MV-Register。 |
| `encoding` | 带边界的版本化二进制帧。 |
| `delta` | 带边界的增量批处理和合并器。 |
| `snapshot` | 不可变状态快照与恢复计划。 |
| `merkle` | 用于反熵的确定性摘要。 |
| `tombstonegc` | 精确墓碑确认和纪元范围的 GC 协调。 |

## 开发与验证

```sh
go test ./...
go test -race ./...
go vet ./...
make coverage
```

`make verify` 还会运行 fuzz、`staticcheck` 和 `golangci-lint`。这两个工具
必须已安装在本地 `PATH` 中；GitHub Actions 会安装固定版本。要在 Docker 中
复现覆盖率门禁：

```sh
make docker-test
```

### 诊断与同步探针

`crdt-analyze` 在输出 JSON 元数据（类型、编解码器、负载大小和 SHA-256 指纹）
前，会先校验一个有边界的帧：

```sh
go run ./cmd/crdt-analyze -file ./state.frame
```

`crdt-sync-probe` 是用于跨主机验证重复增量投递的短生命周期 HTTP 测试工具，
不是生产复制服务。其默认监听地址仅为回环地址；每个端点都要求非空令牌。优先
使用 `-token-file`（权限 `0600`）而非 `-token`，且仅在受控测试窗口内绑定
公网地址。

```sh
# 在每个接收端执行。
go run ./cmd/crdt-sync-probe -mode serve -replica receiver -token-file ./probe.token

# 生成一个增量，并将完全相同的字节序列发送至每个目标。
go run ./cmd/crdt-sync-probe -mode send \
  -target http://receiver-a:49511,http://receiver-b:49511 \
  -replica sender -token-file ./probe.token -duplicates 3
```

使用 `make test-unit` 分别运行各包；使用 `make test-integration` 运行三副本、
恢复、批处理、编码和反熵流程。

CI 工作流会强制执行格式化、单元测试、竞态检测、vet、解码器 fuzz、静态分析、
每个包至少 90% 的覆盖率，以及 Go 1.26 容器验证。

### 质量与性能快照 — 2026-07-28

这份预发布快照在 Go 1.26.5 上采集。它是该修订的历史记录，而不是适用于所有
工作负载的延迟或吞吐保证；本次文档更新没有重新执行以下检查。

- 记录中的 `make verify` 已通过：格式化、独立包测试、集成与极限场景、竞态检测、vet、
  四个各 10 秒的解码器 fuzz、`staticcheck`、`golangci-lint`，以及每包至少
  90% 的覆盖率门禁。
- 记录中的 `make docker-test` 已在 Go 1.26 上通过；`govulncheck ./...` 未发现已知漏洞。
- 受控的三主机投递探针验证了重复投递幂等性，并拒绝了未授权、格式错误和超限
  请求。
- 提供的基准覆盖 G-Counter、PN-Counter、G-Set、OR-Set 和 MV-Register 的
  `Merge`、`ApplyDelta` 与 `MarshalBinary`。在确定容量限制前，请在目标硬件上运行
  `make benchmark`。

本 README 底部的不同场景测评记录了当前本地样本：重复投递、存活状态序列化和
墓碑密集状态序列化。精确结果取决于 CPU、Go 版本、元素编解码器、集合大小和
变更组合。

运行 `make test-extreme` 可在普通和 race 插桩模式下重现高基数场景。内部分析数据
和部署运行手册刻意保留在公开发布树之外。

## 发布 `v1.0.0`

发布前，请运行以上验证命令、审阅公开 API、提交审核过的发布内容，并确保仓库可
通过模块路径公开访问。然后创建不可变的语义版本标签：

```sh
go mod tidy
go test ./...
git tag -a v1.0.0 -m "Release v1.0.0"
git push origin v1.0.0
GOPROXY=proxy.golang.org go list -m github.com/DarkInno/crdt@v1.0.0
```

不要移动或复用已发布标签。对于 Go 模块，首个稳定版之后的破坏性变更需要新的主
版本模块路径，例如 `github.com/DarkInno/crdt/v2`。

## 贡献

请先创建 issue 再提出 API 扩展。贡献应包含聚焦的测试，保持线协议编码确定性，
限制不可信输入，并通过上述验证命令。

## 不同场景性能测评 — 2026-07-28

在 Apple M4 Pro、Go 1.26.5（`darwin/arm64`）本地测得。夹具包含 128 个字符串
元素；每项结果为三次、每次两秒采样的四舍五入均值。各次采样的分配数据保持稳定。

| 场景 | `GOMAXPROCS=1` | `GOMAXPROCS=4` | 每操作分配 |
| --- | ---: | ---: | ---: |
| `Merge` | 56.0 µs/op | 42.9 µs/op | 57,768 B；259 allocs |
| 重复 `ApplyDelta` | 131 ns/op | 131 ns/op | 0 B；0 allocs |
| 并行重复 `ApplyDelta` | 132 ns/op | 105 ns/op | 0 B；0 allocs |
| `MarshalBinary`（128 个存活元素） | 36.8 µs/op | 26.6 µs/op | 29,952 B；132 allocs |
| `MarshalBinary`（墓碑密集） | 24.8 µs/op | 17.4 µs/op | 15,616 B；2 allocs |

`Merge`、普通 `ApplyDelta` 与 `MarshalBinary` 使用串行基准循环；它们的
`GOMAXPROCS=4` 数据仅是运行时设置样本，并非四核吞吐量测量。只有“并行重复
`ApplyDelta`”一行使用了 `RunParallel`。与采用同一存活状态夹具和方法的早期本地
样本相比，`MarshalBinary` 现为每操作 29,952 B、132 次分配，低于原先的
96,312 B、778 次分配。这些是本地预发布测量，不是容量规划或 SLA 保证；设置限制
前请在部署目标上重新运行 `make benchmark`。

## PN-Counter 性能测评 — 2026-07-28

在两台独立的 Debian 13（`linux/amd64`）主机上测得；每台均为 4 个 Intel Xeon
Platinum 8272CL vCPU、3.8 GiB 内存。基准二进制由当前修订以 Go 1.26.5 构建，
每项设置执行 3 次、每次 `-benchtime=2s`；下表为四舍五入均值。夹具在正、负
计数图中各有 128 个副本分量。`MarshalBinary` 括号中为其报告的编码吞吐量；三次
采样的分配数据完全一致。

| 主机 | `GOMAXPROCS` | `Merge` | `ApplyDelta` | `Value` | `MarshalBinary` |
| --- | ---: | --- | --- | --- | --- |
| `210.16.171.72` | 1 | 24.9 µs/op；13,136 B；6 allocs | 149.1 ns/op；0 B；0 allocs | 7.51 µs/op；232 B；10 allocs | 69.4 µs/op（55.3 MB/s）；25,680 B；10 allocs |
| `210.16.171.72` | 4 | 18.6 µs/op；13,136 B；6 allocs | 151.8 ns/op；0 B；0 allocs | 7.29 µs/op；232 B；10 allocs | 53.6 µs/op（71.7 MB/s）；25,680 B；10 allocs |
| `192.140.163.250` | 1 | 25.6 µs/op；13,136 B；6 allocs | 151.8 ns/op；0 B；0 allocs | 7.49 µs/op；232 B；10 allocs | 70.4 µs/op（54.6 MB/s）；25,680 B；10 allocs |
| `192.140.163.250` | 4 | 18.5 µs/op；13,136 B；6 allocs | 153.5 ns/op；0 B；0 allocs | 7.31 µs/op；232 B；10 allocs | 53.3 µs/op（72.1 MB/s）；25,680 B；10 allocs |

`GOMAXPROCS=4` 的各行仍是串行基准测量，不是四核汇总吞吐量。这些受控主机样本
是当前修订的公开回归证据，不是容量上限或 SLA 承诺；确定生产限制前，请在部署
目标上用相同命令复测：

```sh
GOMAXPROCS=4 go test -run='^$' \
  -bench='^BenchmarkPNCounter(Merge|ApplyDelta|Value|MarshalBinary)$' \
  -benchmem -benchtime=2s ./counter
```

## 许可证

SPDX-License-Identifier: MIT

依据 [MIT License](LICENSE) 授权。Copyright (c) 2026 DarkInno.
