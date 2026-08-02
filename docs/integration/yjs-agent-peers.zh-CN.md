# 服务端 AI Agent 作为 Yjs peer

`YJSAgentPeer` 让一个受信任的服务端 Agent 经由与浏览器相同的持久化更新路径参与某个已配置的 Level 1 Yjs 文档。它不是 LLM 接入、文本 patch API，也不会把 Yjs 转换成 Go CRDT；Agent 的工具运行时必须使用维护中的 Yjs 引擎，并在决定变更后提交一个格式固定的标准 Yjs update。

这与 [Electric collaborative AI editor](https://github.com/electric-sql/collaborative-ai-editor) 所演示的形态一致：服务端 Agent 用文档工具工作，结果写回同一个共享 Yjs 文档。本库有意将模型流、聊天历史、工具策略与文档持久化分开。

```text
浏览器 Yjs peers                       受信 Agent task
  y-websocket provider                  服务身份 + policy
           |                                      |
           +------- 已认证 YJSHandler -----------+
                           |              ^
                 state-vector/diff        | 先持久化，再 fan-out
                           v              |
                   YJSStore sidecar <-----+
                   (固定版本的 Yjs 引擎)
```

## 前置条件与不变量

`OpenYJSAgentPeer` 只接受已配置且 store-backed 的 room。`Tenant`、`Room`、`Epoch`、`Schema` 与 V1/V2 `Format` 共同构成不可变文档身份。Level 0 opaque relay 只有有界的实时历史，不能给服务进程提供完整的语义文档，所以会被拒绝。

宿主必须在认证 task runner 后提供服务 `Peer` 身份，例如 `agent:copy-editor:run-7`，并从受信路由/产品配置选择 room。绝不能从 Yjs client ID、prompt、工具参数或文档内容推导二者；Yjs client ID 是 CRDT 元数据，不是认证 actor。

打开以及每次 `Snapshot`/`Diff` 都会调用 `AuthorizeSubscription`；每次 `Publish` 都会以 `YJSUpdate` 调用 `Authorize`。因此已经吊销的服务账号不能继续使用旧 handle。审计可记录服务身份、room、操作、cursor 与允许/拒绝结果，但不能记录文档字节或 prompt。

`Publish` 的顺序保证为：

```text
授权写入 -> 限制 update -> YJSStore.Apply -> 持久化成功 -> 实时 fan-out
```

sidecar 失败时绝不 fan-out。`Applied=false` 仅表示一个已经持久化的 update 被幂等重试；它不是浏览器 receipt、用户批准或人类已读证明。Agent peer 不提供 awareness 方法：y-protocols awareness 是连接所有的临时状态，不是 Agent 状态的持久化通道。

## 受信工具运行时流程

模型没有选择 room 或直接请求 Yjs store 的权限。task runner 维护一个短生命周期、有界的本地 `Y.Doc`，调用诸如 `propose_rewrite`、`append_citation` 的窄产品工具，校验输入及作用域，再把编码 update 交给应用服务。

```go
agent, err := handler.OpenYJSAgentPeer(
    extensions.Peer{ID: "agent:copy-editor:run-7"},
    "notes-tenant-a", // 仅来自受信应用路由
)
if err != nil { return err }

snapshot, err := agent.Snapshot(ctx) // task runner 用固定 Yjs 引擎应用
if err != nil { return err }
_ = snapshot // 不把文档字节写入日志或无界 prompt history

// 受信 Yjs 工具运行时对本地 Y.Doc 做有界、已审查的工具操作，
// 并产生与 room 相同 V1/V2 格式的 update。
result, err := agent.Publish(ctx, encodedYjsUpdate)
if err != nil { return err } // 保留/恢复 task outbox；不要伪造 receipt
if !result.Applied {
    // 同一 update 已经 durable；根据返回的 state vector 恢复。
}
```

新 task 通常从 `Snapshot` 开始。保留有界本地 Yjs 状态的长 task runner 应调用 `Diff`，避免每次模型 turn 读取完整文档；它必须使用 room 选定的 V1/V2 API，并在工具或出站投递失败后丢弃/恢复本地文档。本库不会把模型文本自动变成 Yjs 操作、选择编辑器 schema 或保留 Agent 本地文档。

不要让 Agent 服务直接调用 `YJSStore`：那会绕过 room 的用户侧授权和实时 fan-out。sidecar bearer token 仅属于受信 Go 到 sidecar 的链路，绝不能进入浏览器、模型工具、prompt 或日志。

## 产品、安全与性能边界

CRDT 的收敛只解决并发 Yjs update；它不保证改写符合用户意图、保留法律含义或已被人批准。高影响操作应生成 suggestion/branch，使用稳定 Yjs-relative position 表达目标范围，在 UI 中展示作者与范围，并在 `Publish` 前要求应用层接受。自动写入时要限制每个工具的作用域、每 task 修改字节数和调用次数，限速服务身份，并在文档外保留审计记录。

聊天记录、模型 token、工具 trace 与 Agent 状态默认放在 Yjs 文档外，除非产品明确将其建模为具备保留和访问规则的共享状态。“Agent 正在写入”应使用独立授权、有界的临时通道；它不能清空 outbox，也不能被当作 durable 文档投递。

既有 Yjs 上限仍必不可少：原始消息/update、state-vector、sync/snapshot、sidecar 并发请求、超时、队列和慢 peer 上限。模型 context 是另一条边界，不能把 sidecar 最大 snapshot 等同于可接受 prompt 大小；在提高任一上限前，优先使用 `Diff`、task-local 摘要和产品定义的作用域。

## 验证矩阵

| 场景 | 必需证据 |
| --- | --- |
| Mock/单测 | 匿名或 opaque room 打开失败；读写吊销会重新校验；超限工作不会触达 store；重复 publish 不会再次 fan-out。 |
| 真实协议 | 真实 `yjs@13.6.31` sidecar 持久化 Agent update；关闭 BroadcastChannel 的全新标准 `y-websocket` client 通过 relay state-vector handshake 恢复它。 |
| 故障/恢复 | sidecar 不可用、无效/错误格式 update、重启、取消、重复重试、陈旧 Agent vector 后，最后 durable snapshot 仍可恢复。 |
| 受控性能 | 在 1/4/16/64 receiver 下测 durable apply/diff/snapshot 的 p50/p95/p99、diff bytes、CPU、heap 与 queue drops。局部测量不能说明 WAN、TLS、模型、授权数据库或生产 fan-out 容量。 |
| 线上验收 | 自动写入前，在目标文档形状、认证 gateway、TLS、真实浏览器/编辑器 binding、慢 receiver、task retry/outbox recovery、授权吊销和模型/工具评测集上验证。 |

sidecar 部署见 [Level 1 store 指南](yjs-store.md)，Yjs/Go 不可混用的协议边界见 [Yjs deeper interoperability decision](../design/yjs-deeper-interoperability.md)。
