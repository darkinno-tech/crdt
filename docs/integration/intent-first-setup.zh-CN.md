# 按业务意图配置 CRDT

[English](intent-first-setup.md) | [简体中文](intent-first-setup.zh-CN.md)

CRDT 是已声明的合并规则，不是业务权威服务的替代品。本指南先从产品要共享的
事实出发，再让开发者和工具在把 TypeID 写进 Manifest 前都能检查所选规则。

## 1. 先判断这个事实是否应该由 CRDT 承载

如果允许并发、离线修改会破坏业务不变量，应使用权威服务而不是 CRDT。余额、库存
预留、排他预约、工作流迁移、权限控制和身份决策都属于此类。

对于可最终一致的事实，选择能表达该业务含义的最小规则：

| 问题 | 应检查的 profile | 并发结果 |
| --- | --- | --- |
| 每个副本是否只增加自己的贡献？ | `counter/grow-only` | 增量累加。 |
| 离线成员是否会被独立添加或移除？ | `set/add-wins` | 并发 add 仍然存在。 |
| 并发字段写入是否必须保留给产品层决策？ | `register/multi-value` | 保留全部因果并发值。 |
| 是否是新的纯文本协作文档？ | `text/run-v2` | 插入有确定顺序，删除保留锚点。 |
| 文档是否需要固定、已声明且全量同边界复制的子 CRDT 层级？ | `document/tree-v2` | 所有嵌套子操作按一个完整协议合并。 |

完整清单还覆盖 LWW、旧文本、顺序列表、可移动列表、observed-remove tree 和富文本
profile。profile 会展示冲突规则和不适用场景；它本身不是安全策略，也不是容量配置。

## 2. 查询 profile，而不是猜测协议 ID

根包提供 profile 的不可变副本。ID 区分大小写，避免拼写错误被静默映射到另一种
合并规则。

```go
profile, ok := crdt.ReplicationProfileFor("text/run-v2")
if !ok {
	return errors.New("unknown CRDT profile")
}

for _, requirement := range profile.HostRequirements {
	log.Println(requirement)
}
```

`profile.FrameType` 是本版本封闭协议注册表中的规范 state ID、delta ID、语义版本与
HLC 标记。它只是一段元数据：不能因为 profile 提到某个帧，就接收该帧。

供人阅读的终端输出：

```sh
go run ./cmd/crdt-profile -id text/run-v2
```

供代码生成器、审查机器人或 AI 辅助配置流程读取的确定性 JSON：

```sh
go run ./cmd/crdt-profile -id text/run-v2 -format json
```

JSON 描述合并语义与仍由宿主负责的工作，不包含凭据、传输端点、生产限额或授权。
它应作为设计审查输入，而不是可直接部署的配置。

## 3. 不手抄协议字段地构造 Manifest

产品和安全审查选择 profile 后，通过 replica helper 转换其规范 `FrameType`。helper
会拒绝不完整或被篡改的 type pair，旧 delta ID 因此不能与新语义版本误组合。

```go
profile, ok := crdt.ReplicationProfileFor("text/run-v2")
if !ok {
	return errors.New("unknown CRDT profile")
}

builder, err := replica.NewSessionBuilderForFrameType(
	"notes-42",                         // 应用 group ID
	"example.com/notes/plain-text/v1",  // 应用 schema ID
	1,                                  // membership/contract epoch
	profile.FrameType,
	"", // profile.RequiresCodecID 时传入确定性、已版本化 codec ID
	crdt.ProtocolPolicy{},
)
if err != nil {
	return fmt.Errorf("create manifest: %w", err)
}
manifest := builder.Manifest()
```

该写法等价于自行创建 `replica.Protocol` 后调用
`replica.NewSessionBuilder`，但无需重复三项协议字段。它**不会**认证 `manifest`；只有
先完成经过认证的精确 Manifest 比对，发送方才可发布或订阅。

可运行的完整本地示例会选择 `counter/grow-only`、从 profile 创建 Manifest、验证绑定
Manifest 的 delta，并进行一次有界的重复投递：

```sh
(cd examples && go run ./intent-first-setup)
# profile=counter/grow-only
# state_type=1
# delta_type=3
# value=3
```

## 4. 保持未被替代的边界清晰可见

每个 profile 都将以下职责保留给接入服务：

1. 接收帧前认证对端并比较一个精确 Manifest。checksum、TypeID、profile 或一次成功的
   gRPC 调用都不是对端身份或授权。
2. 解码前执行传输 body 限额，再用具体的 `Unmarshal*WithLimits` 解码器，之后才可
   修改状态。profile 元数据没有默认字节、元素、tag、队列或保留预算。
3. 对操作及其值授权。可收敛的合并规则不会授权一次 increment、授予角色或预留库存。
4. 复用 ID 前，将 profile 要求的 HLC 或因果恢复状态与具体 CRDT 状态、投递 frontier 和
   outbox 原子持久化。
5. 将保留和墓碑压缩当作经过认证的成员关系协议，绝不能当作本地清理优化。

profile 命令和示例刻意保持离线、确定性。若需真实 HTTP 重复投递演练、恢复和反熵
验收标准，请继续阅读[端到端集成教程](overview.zh-CN.md)。
