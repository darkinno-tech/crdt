# 开发者入门指南

[English](getting-started.md) | [简体中文](getting-started.zh-CN.md)

这是一条从新检出代码到实现一个可复制功能的最短安全路径：先用稳定 CRDT 和可执行示例
建立认知，再明确真实网络边界仍需由应用补齐的能力。它不会把本库或示例描述成托管同步服务。

## 1. 跑通一条完整、安全的投递路径

模块的 Go 语言最低版本为 1.21。在已有 Go 应用中，先加入最新稳定模块：

```sh
go get github.com/DarkInno/crdt@latest
```

然后从本地检出中查看并运行最小的完整参考：

```sh
git clone https://github.com/DarkInno/crdt.git
cd crdt
go version
(cd examples && go run ./getting-started)
(cd examples && go test ./getting-started)
```

期望输出：

```text
left=5
right=5
converged=true
```

该示例只使用稳定的 G-Counter 协议。它将本地修改、编码后的 outbox 记录、有界接收解码器和
`ApplyDelta` 保持分离，使应用边界清晰可见；其中一条记录会被故意重复投递，两个副本仍会收敛。
这里的小限额用于教学，不是生产容量建议。

若是修改库本身而不是接入库，请单独验证整个检出：

```sh
go test ./...
```

## 2. 先选数据类型，再写传输代码

先选择已经符合业务事实的合并规则。CRDT 能让已声明的合并规则收敛，不能把不合适的规则
变成业务不变量。

| 需求 | 推荐起点 | 关键规则 |
| --- | --- | --- |
| 只会增加的值，例如已完成工单数 | `counter.GCounter` | 不能递减或重置；每个副本只贡献自己的分量。 |
| 最终一致的可正可负总值 | `counter.PNCounter` | 支持递增和递减，但不能阻止并发超支；余额和预订必须由权威服务约束。 |
| 永不删除的事实 | `set.GSet[T]` | 元素 codec 是 wire 契约的一部分，必须确定性编码。 |
| 支持离线 add/remove 的成员集合 | `set.ORSet[T]` | 并发 add/remove 时 add-wins；复用 ID 前必须持久化带 HLC 的快照。 |
| 并发值都必须保留的字段 | `register.MVRegister` | 读取 `Values()` 并由产品解决并发值；不能用墙上时钟选赢家。 |
| 余额、排他预订、工作流流转、权限决策 | 权威服务 | 不要把最终 CRDT 收敛当作不变量或授权机制。 |

先使用内存内合并定义业务语义。根目录的
[`example_test.go`](../example_test.go) 包含 `ExampleGCounter`、
`ExamplePNCounter`、`ExampleORSet`、`ExampleGSet` 和 `ExampleMVRegister`
这些可执行的最小 API 参考。各包文档还提供了有界 G-Counter 接收、实验性
RGA 投递和 Manifest 的聚焦 Example：

```sh
go test -run '^Example(GCounter|PNCounter|ORSet|GSet|MVRegister)$' .
go test ./counter ./text ./replica
```

完成最小流程后，建议运行协作任务看板这一真实场景。它使用有界的 G-Counter 和 OR-Set 解码器，
故意重复/乱序投递更新，模拟分区中的 add-wins 冲突，并从快照恢复相同 OR-Set 副本 ID：

```sh
(cd examples && go run ./collaborative-board)
```

预期最终状态：

```text
completed-inspections=5
open-tasks=[close-shift inspect-pump replace-filter]
```

要运行包含 framed G-Set、MV-Register、重复投递和恢复的流程：

```sh
(cd examples && go run ./warehouse-replication)
```

## 3. 区分本地修改与远端投递

每次可复制修改都有两项独立职责：

1. 修改本地 CRDT，并保留返回的强类型 delta。
2. 将 delta 编码后放入 outbox；接收端必须先用有界、类型专属的解码器校验，再调用
   `ApplyDelta`。

例如，本地 G-Counter 修改本身很小：

```go
local, err := counter.NewGCounter("warehouse-a")
if err != nil {
	return err
}
delta, err := local.Increment(1)
if err != nil {
	return err
}
encoded, err := delta.MarshalBinary()
if err != nil {
	return err
}
```

接收边界的限额应来自传输与产品预算，而不是不受控的分配。具体解码器会完整校验规范帧，
只有校验成功后 `ApplyDelta` 才能修改状态：

```go
limits := frame.DecoderLimits{
	MaxFrameBytes:  64 << 10,
	MaxPayload:     60 << 10,
	MaxCodecID:     128,
	MaxElements:    1024,
	MaxTags:        2048,
	MaxStringBytes: 1024,
}
received, err := counter.UnmarshalGCounterDeltaWithLimits(encoded, limits)
if err != nil {
	return err // 拒绝，且本地 CRDT 状态不变
}
if err := remote.ApplyDelta(received); err != nil {
	return err
}
```

返回帧的校验和只能发现意外损坏。此步骤之前仍要认证和授权发送方，在分配字节切片前限制
HTTP/WebSocket body，并在重试前持久化 mutation/outbox 记录。
[端到端集成教程](integration/overview.zh-CN.md)给出了完整本地投递演练；
[WebSocket Provider 参考实现](integration/websocket-provider.zh-CN.md)给出了有界、
Manifest 绑定的 Go 接入模式。这个拆分的完整、可编译 G-Counter 版本见
[`examples/getting-started`](../examples/getting-started)；应复用其结构，而不是照搬示例限额。

## 4. 持久化完整的恢复记录

同一副本 ID 重启时，仅保存编码后的 CRDT state 并不总是安全。应在一个持久化事务中保存
完整快照、复制 frontier 和应用拥有的投递元数据。

| 类型 | 持久化与恢复方式 | 原因 |
| --- | --- | --- |
| `set.ORSet` | `SnapshotCurrentState()` 和 `set.NewORSetFromSnapshot` | 快照包含生成新且唯一 mutation tag 所需的 HLC state。 |
| `register.MVRegister` | `Snapshot()` 和 `register.NewMVRegisterFromSnapshot` | 因果版本向量可阻止同一 ID 复用已有 dot。 |
| HLC 驱动的 LWW、RGA、OR-Tree、附件引用 | 各自的 `SnapshotCurrentState()` 等价方法与匹配的 `New...FromSnapshot` 构造器 | state、clock 与保留的 tombstone 元数据是一个恢复单元。 |

不要只恢复 `MarshalBinary()` 字节后便用相同 ID 重建 OR-Set 或其他 HLC 类型。全新逻辑副本
必须使用全局唯一的新 ID；相同 ID 重启必须恢复其保存的 clock/context。

## 5. 每个复制组只选择一种协议

将复制组绑定到经过认证的 `replica.Manifest`：group、schema、epoch、具体 state/delta
frame ID、codec ID 和语义版本都必须匹配，之后对端才能发送数据。`ProtocolPolicy` 只是
本地能力清单，不是全局开关，也不能代替认证。

新建 Go RGA 复制组时尤其要注意：

- `crdt.DefaultRGAFrameType()` 选择紧凑的 run-v2 帧 19/20。
- 仅选择 frame type 不会改变编码器：delta 应使用 `Delta.MarshalRunBinary`，完整 state
  与恢复应使用 `RGA.MarshalRunBinary` / `RGA.SnapshotRunCurrentState`。
- 默认浏览器/JavaScript Wasm artifact 接受 run-v2 帧 19/20 与语义版本 2；`make wasm-v1`
  仅保留给经过显式协商的旧版 v1 迁移组。
- 一个 Manifest 只绑定精确的一对帧。没有 Wasm 的原生客户端必须遵循 [RGA run-v2
  线协议](protocol/rga-run-v2.zh-CN.md)及其 canonical vector；绝不能把 v1 帧静默地
  重新解释成 run-v2，反之亦然。

LWW-Set、LWW-Map、旧版 RGA v1 和通用 RGA list 均为稳定协议，在每个 manifest、change、inbox、checkpoint、
session 边界都绑定各自精确的 frame pair 与语义版本。稳定 observed-remove tree v1 则在零值策略下绑定
`tree.SemanticsVersion` 与精确的节点值 schema；它仅支持不可变父链接 add/remove，不支持原地 move。
选择前先阅读[集合扩展设计](design/crdt-extension.md)和
[OR-Tree v1 协议](protocol/or-tree-v1.md)。浏览器部署、持久化和 CSP 要求见
[TypeScript/Wasm 客户端指南](../clients/typescript/README.md)。

## 6. 按目标进入下一份指南

| 目标 | 下一步 |
| --- | --- |
| 学习双副本投递、恢复和反熵 | [端到端集成教程](integration/overview.zh-CN.md) |
| 接入 WebSocket endpoint 与 Go client | [WebSocket Provider 参考实现](integration/websocket-provider.zh-CN.md) |
| 复制不可变媒体/文件引用 | [附件引用集成](integration/attachment.zh-CN.md) |
| 在浏览器或 WebView 中本地合并 RGA | [TypeScript/Wasm 客户端指南](../clients/typescript/README.md) |
| 实现不使用 Wasm 的 RGA 客户端 | [RGA run-v2 线协议](protocol/rga-run-v2.zh-CN.md) |
| 设计新集合或评估协议成熟度 | [集合扩展设计](design/crdt-extension.md) |
| 参与本库开发 | [贡献指南](../CONTRIBUTING.md) |

## 7. 生产就绪前的检查

示例通过只能证明当前修订中的库行为，不能证明部署可用。投产前请独立验证：

- TLS、身份、授权、租户/组隔离、限流和请求/body 限额；
- 持久化 mutation/outbox、actor counter、CRDT state、frontier、snapshot、重试与
  bootstrap/反熵恢复；
- 任何 tombstone compact 之前，可信成员权威和精确的 epoch 绑定确认；以及
- 按真实工作负载限制验证重复、乱序、离线、重连、过载、恢复和对抗输入。

按改动范围运行相应检查。本库本地门禁见[贡献指南](../CONTRIBUTING.md)；`make verify`
还要求 `PATH` 中有固定版本的静态分析工具。
