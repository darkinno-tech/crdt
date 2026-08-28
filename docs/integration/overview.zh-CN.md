# 端到端集成教程

[English](overview.md) | [简体中文](overview.zh-CN.md)

本教程将库原语串成可验证链路：两个 HTTP 接收端接收同一份编码 delta，重复投递
被验证为幂等；业务示例覆盖网络分区、add-wins 冲突和安全的 OR-Set 重启。HTTP
工具只是测试探针，不是生产复制服务。

## 场景与数据模型

示例模拟设备维护协作：

| 业务事实 | CRDT | 原因 |
| --- | --- | --- |
| 每位技师已完成的巡检数 | `GCounter` | 技师只增加自己的分量，仪表盘汇总所有分量。 |
| 协作看板中仍开放的任务 | `ORSet[string]` | 车辆离线时，任务仍可能被独立添加或移除。 |

不要用 `GCounter` 表达会减少的值、余额、排他预订或工作流状态迁移；不要将
`ORSet` 当成最后写入胜出的文档存储。此类需严格保持的业务约束应留在权威服务，
只把合并语义可接受的事实交给 CRDT 复制。

## 1. 验证并运行真实业务示例

要求：Go 1.21 或更高版本，以及本模块的本地检出。在仓库根目录运行：

```sh
go test ./...
(cd examples && go run ./collaborative-board)
```

期望输出：

```text
completed-inspections=5
open-tasks=[close-shift inspect-pump replace-filter]
```

程序会序列化每个 delta，并在应用前使用显式接收限额解码；其中一个 counter delta 和一个重新开放
任务的 delta 都被投递两次。现场车辆网络分区时，在已经观察到 `inspect-pump`
后将其移除；调度端同时再次添加同一任务。新 add 带有不同标签，因而能在已观察
到的 remove 后保留，这就是 add-wins。随后程序通过 `SnapshotCurrentState` 保存
状态、以相同 ID 恢复现场副本，并安全地产生新变更。源码见
[examples/collaborative-board/main.go](../../examples/collaborative-board/main.go)。

## 2. 在本地执行真实 HTTP 投递

`crdt-sync-probe` 用于短时间传输校验，只接收库定义的编码 delta，不提供业务 API。
以下命令启动两个接收端，将同一变更广播给两端并核对返回状态。令牌和日志写入
临时目录，避免进入仓库：

```sh
umask 077
scenario_dir="$(mktemp -d)"
openssl rand -hex 32 > "$scenario_dir/probe.token"
go build -o "$scenario_dir/crdt-sync-probe" ./cmd/crdt-sync-probe

"$scenario_dir/crdt-sync-probe" -mode serve -listen 127.0.0.1:49511 -replica dock-a -token-file "$scenario_dir/probe.token" > "$scenario_dir/dock-a.log" 2>&1 &
pid_a=$!
"$scenario_dir/crdt-sync-probe" -mode serve -listen 127.0.0.1:49512 -replica dock-b -token-file "$scenario_dir/probe.token" > "$scenario_dir/dock-b.log" 2>&1 &
pid_b=$!

token="$(tr -d '\n' < "$scenario_dir/probe.token")"
ready=''
for attempt in 1 2 3 4 5; do
  if curl -fsS -H "X-CRDT-Probe-Token: $token" http://127.0.0.1:49511/state >/dev/null && curl -fsS -H "X-CRDT-Probe-Token: $token" http://127.0.0.1:49512/state >/dev/null; then
    ready=1
    break
  fi
  sleep 1
done
test -n "$ready" # 失败时检查 "$scenario_dir"/*.log。

go run ./cmd/crdt-sync-probe -mode send -target http://127.0.0.1:49511,http://127.0.0.1:49512 -replica receiving-gate -token-file "$scenario_dir/probe.token" -counter-increment 4 -element pallet-042 -duplicates 7
```

两个 JSON 报告都必须只包含一个 `receiving-gate: 4` 分量和一个 `pallet-042`
元素，尽管每个 delta 被投递七次。再发送独立变更并读取两端状态：

```sh
go run ./cmd/crdt-sync-probe -mode send -target http://127.0.0.1:49511,http://127.0.0.1:49512 -replica forklift-9 -token-file "$scenario_dir/probe.token" -counter-increment 2 -element pallet-043 -duplicates 3

curl -fsS -H "X-CRDT-Probe-Token: $token" http://127.0.0.1:49511/state
curl -fsS -H "X-CRDT-Probe-Token: $token" http://127.0.0.1:49512/state
```

两次最终响应应具有相同的 `counts`（`receiving-gate: 4`、`forklift-9: 2`）和
排序后的元素（`pallet-042`、`pallet-043`）。只停止本次命令创建的 PID，再删除
精确的临时目录：

```sh
kill "$pid_a" "$pid_b"
wait "$pid_a" "$pid_b" 2>/dev/null || true
rm -rf "$scenario_dir"
```

探针的每个端点都要求 `X-CRDT-Probe-Token`，默认仅绑定 loopback，counter/OR-Set
请求体上限为 1 MiB。它没有 TLS、持久化状态、成员管理、重放策略或授权模型，绝不能
暴露到公网。

### RGA 诊断边界

`/rga` 默认使用稳定的 RGA run-v2（TypeID 19/20）。它仅用于受控诊断，不会协商 Manifest
或生产级 `ProtocolPolicy`。旧标量 v1（TypeID 11/12）是稳定的兼容格式，发送端和接收端都必须
显式使用 `-rga-protocol=v1`；格式不匹配的帧会在修改文本前被拒绝。该路由的单个 delta
最多为 16 MiB、最多生成 200,000 个 rune，成功后返回空 `204`，最终收敛信息只从 `/state`
获取。

```sh
# 两个接收端和此发送端不传协议参数时，默认都是稳定的 run-v2。
go run ./cmd/crdt-sync-probe -mode send \
  -target http://127.0.0.1:49511,http://127.0.0.1:49512 \
  -replica text-gate -token-file "$scenario_dir/probe.token" \
  -counter-increment 0 -element '' \
  -rga-runes 4096 -rga-rune 'λ' -duplicates 3
```

两个最终 `text` 对象的 `protocol`、`runes`、`sha256` 必须一致，且 `pending` 为零。这只证明
被演练的内存内重复/乱序路径，不能证明 HLC 持久化、恢复、成员关系或墓碑 GC 安全性。

## 3. 接入生产传输层时由应用负责的契约

探针展示了接入应用必须负责的边界：

1. 为每个存活逻辑副本分配全局唯一且非空的 ID。OR-Set 重启后若复用 ID，必须
   恢复 HLC 状态；MV-Register 复用 ID 时必须恢复因果快照；否则要使用新 ID。
2. 本地变更时调用 `Add`、`Remove`、`Increment` 或 `Set`，在同一个持久化事务/outbox 中
   保存本地 CRDT 状态及编码 delta。OR-Set 通过 `SnapshotCurrentState()` 或
   `MarshalBinaryWithClockState()` 将状态和 HLC 状态一起保存。
3. 接收 payload 前完成发送方认证与授权；按应用预算限制消息字节数、元素数、标签
   数和字符串大小。用 `Unmarshal*WithLimits` 解码不可信输入，在记录接收结果的
   同一个持久化事务中调用 `ApplyDelta`。
4. 重试 outbox 项直到接收端确认。CRDT join 能承受重复和乱序投递，但不能替代网络、
   持久化、认证或业务授权。
5. 定期交换完整状态或 Merkle 摘要以发现缺失历史，再合并状态修复。单靠重试队列
   无法修复进入队列前已经丢失的 delta。
6. 交换任何 state 或 delta 帧前，必须认证绑定精确 state/delta pair 的 Manifest。
   只有双方确认同一 pair 后才可发送该类型；
   未知、仅预留或未共同启用的类型都应作为协议错误处理。

OR-Set 接收端的核心形态如下：

```go
encoded := boundedRequestBody(request)
delta, err := set.UnmarshalORSetDeltaWithLimits(encoded, taskCodec, limits)
if err != nil {
    return badRequest(err)
}
if err := workboard.ApplyDelta(delta); err != nil {
    return internalError(err)
}
// 原子持久化 workboard 状态、其 HLC 状态以及接收记录/outbox 数据。
```

`taskCodec.ID`、`Marshal` 和 `Unmarshal` 都是线协议契约。所有副本必须使用稳定
codec ID 和确定性编码；字节格式变化时要显式版本化。这里的 `taskCodec` 仅为示例，
真实任务通常应使用规范化 ID，而不是任意展示文本。

## 4. 稳定的 G-Set 与 MV-Register 集成

G-Set 与 MV-Register 是零值 `crdt.ProtocolPolicy` 默认包含的稳定 framed 协议。G-Set
只能增长；产品需要移除元素时应使用 OR-Set。MV-Register 的 `Set` 可能保留多个因果上
并发的值；应读取 `Values()` 并明确产品层的选择逻辑，不能假定总有一个最后写入者。

必须在同一 outbox/接收记录事务中，原子持久化 MV-Register 状态帧和 `Snapshot()`。复用
同一 ID 的副本必须通过 `register.NewMVRegisterFromSnapshot` 恢复；仅有状态字节会丢失
因果上下文。G-Set 和 MV-Register 帧使用零值策略。

## 5. 稳定文本、富文本与 observed-remove tree 集成

run-v2 RGA（`text`，TypeID 19/20）是零值策略包含的稳定 framed 协议。必须在经过认证的
Manifest 中绑定 `text.StableFrameType()` 和 `text.RunV2SemanticsVersion`，使用
`text.UnmarshalRGARunDeltaWithLimits` 解码 delta，且不能因帧不匹配而回退到旧 v1。
旧标量 RGA v1（11/12）、LWW-Set（`lww.Set`）、LWW-Map（`lww.Map`）与通用 RGA list（`list.RGA`）
均为稳定能力，必须通过上述精确 Manifest 检查；该策略仅属于一个复制组，并不是动态插件机制。不能仅因
不可信帧的校验和有效就按某种类型分派它。

行内富文本（`richtext`，TypeID 23/24）同样是零值策略允许的稳定协议。必须在独立 Manifest
中绑定 `richtext.SemanticsVersion` 与一个精确的应用渲染/属性 `SchemaID`，使用
`richtext.UnmarshalDeltaWithLimits` 解码，并原子持久化状态、共享 RGA HLC 时钟、投递 frontier
和 outbox。属性键和值不是 HTML 或授权声明：渲染前必须校验 Manifest 选择的 schema 并完成安全
净化。[富文本 v1 协议](../protocol/richtext-v1.md)规定了规范向量、限制与精确确认式压缩。

observed-remove tree（`tree`，TypeID 17/18）同样是零值策略允许的稳定协议。必须在独立 Manifest 中
绑定 `tree.SemanticsVersion` 与精确的应用节点值 `SchemaID`，使用 `tree.UnmarshalDeltaWithLimits`
解码，并原子持久化状态、HLC 时钟、投递 frontier 和 outbox。Tree v1 使用不可变父链接，仅支持
add/remove：move 必须表示为 remove 后创建新节点，绝不能重写父链接。
[OR-Tree v1 协议](../protocol/or-tree-v1.md)规定了规范向量、限制与精确确认式压缩。

必须在同一 outbox/接收记录事务中，原子持久化本地 LWW-Set、LWW-Map、RGA 或 OR-Tree 状态帧及其 HLC
状态。复用同一 replica ID 时，只能通过 `SnapshotCurrentState()` 和各包的
`NewFromSnapshot` 恢复；仅有状态字节不能证明下一枚本地标签唯一。RGA 和 OR-Tree
为处理乱序投递而保留删除墓碑。RGA 的 `CompactTombstones` 只能移除已删除的叶节点；应用
必须先建立经过认证的精确确认纪元、持久化压缩后快照并淘汰旧 delta。Tree 的
`CompactEligibleTombstones` 可按叶到根回收已精确确认的已删除分支；未选择或仍存活的子节点仍是锚点。
LWW-Set 与 LWW-Map 仅能通过精确确认协调器回收；证明完成前仍须为墓碑设定预算并监控。

### 5.1 附件引用

`attachment.Register` 是 LWW-Map 帧面向图片、音频、视频和任意数据引用的稳定、受 schema
限制的用法。每个附件复制组都要单独建立 Manifest：状态/增量 ID 为 9/10，schema ID 为
`github.com/darkinno-tech/crdt/attachment-reference/v1`，codec ID 为空，语义版本使用
`attachment.SemanticsVersion`。不得用同一个 Manifest 承载 RGA 文本：一个 Manifest 只能绑定一种
具体 CRDT 协议。

接收边界应先认证精确 Manifest，再使用传输限制和附件留存限制调用
`attachment.UnmarshalDeltaWithLimits`，之后才应用 delta。原子持久化
`SnapshotCurrentState()`，同 ID 副本通过 `attachment.NewFromSnapshotWithOptions` 恢复。

附件引用只有元数据。外围应用负责存储授权、上传/下载、扫描、加密和重试。已授权下载后、解码或
渲染前必须调用 `Reference.Verify`；它以流式方式校验并拒绝截断、超长或 SHA-256 不匹配对象。
完整流程和限制清单见[附件引用集成文档](attachment.zh-CN.md)及其
[可运行示例](../../examples/attachment-collaboration)。

### 5.2 浏览器与 JavaScript/WebView RGA 客户端

`clients/typescript` 将跨语言边界保持得很窄：TypeScript 模块只验证有边界的公共
frame 外层，Go/Wasm RGA runtime 调用规范的 Go 解码器和合并引擎。`make wasm` 构建默认的
run-v2 artifact（state/delta TypeID 19/20、语义版本 2），与
`crdt.DefaultRGAFrameType()` 保持一致。`make wasm-test` 验证 Go 到客户端的帧，以及重复/
乱序投递的三副本会话；`make wasm-v1-test` 则单独验证旧标量 v1 artifact（TypeID 11/12）。

先完成一个精确、经过认证的 Manifest/能力协商（包括 state/delta ID 与语义版本），再按应用
传输体限制调用 `document.applyDelta`。CRC-32C 只检测意外损坏。必须将返回的
`{ state, clock, frontier }` 作为一条原子本地记录持久化；只恢复 state 会导致重用的
replica ID 产生不安全的 HLC 标签。一次编辑事务超过 64 KiB 或 16,384 rune 时必须在本地
插入前按顺序拆分；长文档应在 Worker 中合并。

一个 Manifest 只绑定一种 RGA wire 格式，不能把旧 v1 artifact 连接到 run-v2 组，反之亦然。
没有兼容 Wasm runtime 的原生客户端，必须先实现并验证规范的 [RGA run-v2 wire
协议](../protocol/rga-run-v2.md)，包括其 canonical vector 套件，才能加入 run-v2 复制组。

需要应用自有的 WebSocket 集成参考时，见 [WebSocket Provider 指南](websocket-provider.zh-CN.md)
及其[可运行示例](../../examples/websocket-provider)。

## 6. 恢复、反熵与墓碑

新副本或恢复副本应从完整状态快照启动。OR-Set 绝不能只从 `MarshalBinary()` 字节
恢复相同 ID 的副本，否则下一枚 HLC 标签可能与此前本地标签冲突。应原子保存状态帧
与 HLC 状态，例如使用 `SnapshotCurrentState()` 和 `NewORSetFromSnapshot()`。

不要只因某对端最大标签较新就压缩墓碑。乱序投递下，最大标签不能证明历史没有
缺口。只有拥有经过认证、权威的活跃成员视图，并获得当前成员纪元精确的
`TombstoneTags()` 确认时，才可使用 `tombstonegc.Coordinator`。已退役成员必须从
压缩后的快照重新引导才能回归。

对于可丢弃的本地缓存，或能够从权威源重建且永远不会合并延迟操作的服务端默认值，可
显式使用 `tombstonegc.SimpleCollector` 做有界的仅本地回收。它不会放宽上述复制流程；
选择前请阅读 [tombstone GC 模式选择](tombstone-gc.zh-CN.md)。

仓库集成测试也是可执行参考：它验证三副本 delta 投递、批处理、恢复，以及 Merkle
反熵差异后的最终收敛。

```sh
make test-integration
```

## 验收清单

| 检查项 | 必需证据 |
| --- | --- |
| 稳定身份 | 已记录并测试 replica-ID 生命周期、OR-Set HLC 持久化和 MV-Register 因果快照持久化。 |
| 重复/乱序投递 | 同一编码 delta 被多次投递，最终状态不变。 |
| 分区修复 | 副本经快照引导或状态/Merkle 交换修复后收敛。 |
| 输入安全 | 解码前已认证；有边界的解码器拒绝损坏、超限、类型或 codec 不匹配帧。 |
| 业务语义 | 产品方已接受 add-wins、只增长 G-Set、计数器及 MV-Register 并发值语义。 |
| 协议一致性 | 所有稳定 frame pair 都必须经认证的精确 Manifest 比对后启用；所有 HLC 协议都持久化时钟状态，并在精确确认允许回收前保留墓碑。 |
| 运维归属 | outbox 重试、监控、备份、成员退役和墓碑策略均有明确负责人。 |

`go test` 通过只证明当前修订中的库和示例；它不证明浏览器、移动端、生产网络、
身份提供方、数据库事务或真实成员生命周期。上述边界仍须在接入服务中逐项验证。
