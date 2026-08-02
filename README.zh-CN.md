# crdt

[English](README.md)

> 面向收敛状态、增量复制、恢复与明确协议边界的有界 Go CRDT 库。

`crdt` 提供确定性的 CRDT 原语和二进制帧编码，使副本在重复、乱序或延迟投递下仍能收敛。它是一个库，不是完整的协作服务：身份、授权、存储、传输、成员关系、保留策略和业务不变量仍由宿主应用负责。

## 三分钟开始

要求 Go 1.21 或更高版本。

```sh
go get github.com/DarkInno/crdt@latest
```

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
	if _, err := left.Increment(3); err != nil {
		log.Fatal(err)
	}
	if err := right.Merge(left); err != nil {
		log.Fatal(err)
	}
	value, err := right.Value()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(value) // 3
}
```

本地检出：

```sh
git clone https://github.com/DarkInno/crdt.git
cd crdt
make test
```

仓库根目录的 `go test ./...` 有意只验证无依赖核心；`make test` 会遍历核心和全部按需模块。

## 包含的能力

- G-Counter、PN-Counter、G-Set、add-wins OR-Set 和因果 MV-Register。
- 有界规范化 state/delta 帧、确定性 snapshot、恢复计划，以及可复用 replica ID 所需的 HLC 状态。
- 默认使用稳定 run-v2 帧的 RGA 协作文本；并提供对密集 HLC 链显式协商的 packed-v3 帧，以及稳定有界 rich-text、observed-remove tree、list、XML fragment 层。
- Delta 批处理、Merkle 反熵、精确确认的 tombstone-GC 协调，以及 Manifest 绑定的 replica/inbox 恢复辅助能力。
- 以按需模块提供的有界 live WebSocket provider、独立 bbolt durable relay、Redis/PostgreSQL/MySQL/SQLite durable-log 实现，以及本地 bbolt/文件 checkpoint Store 参考实现。
- 可选、由 Manifest 协商的[压缩感知外层帧 v2](docs/protocol/frame-v2.md)，提供显式 v1 转换，但不改变 CRDT TypeID 或语义。
- [RGA 诊断混淆](docs/integration/debug-obfuscation.zh-CN.md)：替换文本内容，同时保留隔离调试时间线的结构。

所有已实现的 frame pair 均为稳定协议，使用零值 `ProtocolPolicy`。LWW-Set/Map、scalar RGA v1、list RGA、run-v2 RGA、packed-v3 RGA、rich-text v1 和 observed-remove tree v1 仍必须绑定经过认证的精确 Manifest：帧类型本身从来不是已协商的协议、已认证的 peer，也不是 compact tombstone 的许可。

## 按目标选择入口

| 目标 | 阅读或运行 |
| --- | --- |
| 学习基础 API | [入门指南](docs/getting-started.zh-CN.md) 与[可运行示例](examples) |
| 不接触 CRDT 底层细节地使用命名 Map/Array | [共享文档指南](docs/integration/shared-document.zh-CN.md) 与 `(cd examples && go run ./shared-document)` |
| 不手抄协议 ID 地选择 CRDT | [按业务意图配置](docs/integration/intent-first-setup.zh-CN.md) 和 `go run ./cmd/crdt-profile -format json` |
| 构建完整客户端流程 | [端到端集成](docs/integration/overview.zh-CN.md) |
| 安全跨越本地重启 | [本地 checkpoint Store](docs/integration/local-checkpoint.zh-CN.md) 与 `(cd examples && go run ./persistent-replica)` |
| 增加重放与重连 | [durable relay 参考](docs/integration/durable-provider.zh-CN.md) |
| 选择浏览器、WebRTC、Redis、PostgreSQL、MySQL 或 SQLite 边界 | [Provider architecture](docs/integration/provider-architecture.md) |
| 使用有界 live relay | [WebSocket provider 参考](docs/integration/websocket-provider.zh-CN.md) |
| 连接稳定版 Yjs/y-websocket client | [Yjs / y-protocols 兼容 relay](docs/integration/yjs-relay.zh-CN.md) |
| 使用受控富文本格式绑定 Quill Delta | [富文本编辑器绑定](docs/integration/richtext-editor-bindings.md) |
| 安全规划更深层的 Yjs 支持 | [Yjs 深层互操作决策](docs/design/yjs-deeper-interoperability.md) |
| 在不复制媒体字节的前提下附加媒体 | [附件集成](docs/integration/attachment.zh-CN.md) |
| 在 Go/Wasm 之外实现 run-v2 | [RGA run-v2 协议与向量](docs/protocol/rga-run-v2.zh-CN.md) |
| 在不改变 scalar RGA 语义下缩小大文本帧 | [Packed RGA v3 协议](docs/protocol/rga-packed-v3.zh-CN.md) 与 `go run ./cmd/crdt-compare -protocol=packed-v3` |
| 使用 Rust、Python、Swift 或 C++ 原生 RGA runtime | [多语言 RGA 交付决策](docs/design/native-multilanguage-rga.zh-CN.md) |
| 实现稳定格式化或树 | [rich-text v1](docs/protocol/richtext-v1.md) 和 [observed-remove tree v1](docs/protocol/or-tree-v1.md) |

[文档索引](docs/README.md) 将入门、集成、协议/设计、运维资料分层。详细性能证据和部署手册放在对应文档中，而不是让入口 README 变成操作手册。

## 持久化与恢复

对于 HLC CRDT，只保存状态字节不足以恢复 replica。复用 replica ID 前，必须原子保存 state frame、HLC 状态和应用的投递 frontier/outbox。按需的 `persistence.Store` 为一种有类型的 CRDT schema 和一个 active process 提供 bbolt 与文件参考实现：保存前和每次加载时都会校验具体状态。

```sh
(cd examples && go run ./persistent-replica)
# recovered=true cursor=41 outbox_bytes=24
```

它不是集群数据库、已认证传输或通用业务事务管理器。静态加密、备份/恢复、远端授权、租户隔离、成员关系和 tombstone 生命周期仍由宿主负责。

`durable` 有意只持久化 relay 操作日志和重放 cursor。客户端必须先持久化具体 CRDT checkpoint，才能推进该 cursor；请配合阅读[本地检查点](docs/integration/local-checkpoint.zh-CN.md)和 [durable relay](docs/integration/durable-provider.zh-CN.md)。

## 模块与依赖

已发布的根模块 `github.com/DarkInno/crdt` 没有任何非标准库 module 依赖。耐久存储、传输和数据库后端都是独立版本的按需模块，因此仅使用核心的应用不会解析它们的依赖图。

| 模块 | 按需能力 |
| --- | --- |
| `github.com/DarkInno/crdt/durable` | bbolt durable relay 与 WebSocket 重连客户端。 |
| `github.com/DarkInno/crdt/persistence` | bbolt 与文件 checkpoint Store。 |
| `github.com/DarkInno/crdt/telemetry` | 有界 telemetry 与按需 OpenTelemetry metrics adapter。 |
| `github.com/DarkInno/crdt/extensions` | WebSocket、HTTP/SSE、gRPC 与 Yjs relay 参考。 |
| `github.com/DarkInno/crdt/providers/{redis,postgres,mysql,sqlite,webrtc}` | durable-log 与 DataChannel 后端。 |
| `github.com/DarkInno/crdt/examples` | 可运行示例，包括 WebSocket 参考。 |

例如，选择 MySQL 的应用只安装核心、durable 契约和 MySQL provider（以及应用自行选择的 driver）：

```sh
go get github.com/DarkInno/crdt@latest
go get github.com/DarkInno/crdt/durable@latest
go get github.com/DarkInno/crdt/providers/mysql@latest
```

## 包地图

| 包 | 职责 |
| --- | --- |
| `counter`、`set`、`register` | Counter、Set、Register CRDT。 |
| `shared` | 稳定 document-tree-v1 帧之上的高层命名 Map/Array facade。 |
| `lww`、`tree`、`text`、`list`、`xml`、`richtext` | 基于 HLC 的有序协作结构。 |
| `encoding`、`delta`、`snapshot`、`clock` | 帧、受限批次、snapshot 与 HLC 状态。 |
| `replica`、`membership`、`tombstonegc`、`merkle` | 投递连续性、成员关系、安全 GC 协调与反熵。 |
| `github.com/DarkInno/crdt/persistence` | 按需的本地有界 bbolt 和文件 CRDT checkpoint Store 参考。 |
| `github.com/DarkInno/crdt/telemetry` | 按需的无 payload 有界 telemetry 和 OpenTelemetry adapter。 |
| `github.com/DarkInno/crdt/durable`、`github.com/DarkInno/crdt/extensions`、`observe` | 按需的 durable relay 和 live relay；核心中的进程内观察。 |
| `attachment` | 不可变媒体引用元数据，绝不保存原始媒体字节。 |

## 验证与测量

修改单个包时先运行聚焦检查：

```sh
(cd persistence && go test .)
(cd examples && go test ./persistent-replica)
(cd persistence && go test -race .)
(cd persistence && go test -run='^$' -fuzz=FuzzUnmarshalCheckpoint -fuzztime=250000x -parallel=1 .)
(cd persistence && go test -run='^$' -fuzz=FuzzUnmarshalFileRecords -fuzztime=250000x -parallel=1 .)
(cd persistence && go test -run='^$' -bench='Benchmark((File)?Store(Save|Load|SaveParallel|Delete|LoadLegacyMigration)|(File)?ConfigFromLoader)$' -benchmem -benchtime=2s .)
```

仓库门禁：

```sh
make test
make race
make vet
make coverage
make verify
```

`make verify` 还会运行有界 fuzz、静态分析、lint、集成与极限场景。`make benchmark` 是受控开发测量，不是生产容量承诺；设置限制前，请在目标磁盘、CPU、Go 版本、网络和工作负载上重跑聚焦基准。

## 关键边界

- CRC-32C、SHA-256 和帧类型只能发现格式损坏，不能认证 peer。请在认证握手中绑定精确 Manifest 和协议策略。
- 最大已观察 tag 不是连续投递的证明，也不是删除 tombstone 的许可。请使用对应的 frontier、inbox 和 membership 契约。
- 两个 checkpoint 后端都要求单一 active process，且都不是 HA 存储。bbolt 持有本地独占文件锁；文件参考实现没有进程间锁，绝不能由多个 active pod 共享。
- 库不会强制业务不变量。身份、租户、值权限、限流、保留策略和备份访问都必须由宿主校验。

## 贡献与发布

贡献应包含聚焦测试、保持规范化编码、在分配或变异前限制不可信输入，并更新最贴近的文档。请阅读 [CONTRIBUTING.md](CONTRIBUTING.md)；beta 变更应走经过评审的 beta-to-preprod-to-main 发布路径，不能手动移动已发布 tag。

## 许可证

SPDX-License-Identifier: MIT。见 [LICENSE](LICENSE)。
