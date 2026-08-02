# 故障复盘：Yjs 手动出站回调异常穿透且未锁存

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-08-02 |
| 发现人 | Codex 多维代码审计与受控 Yjs 验证 |
| 严重程度 | P2-一般 |
| 影响范围 | `@darkinno/crdt-client/yjs` 使用手动 `onLocalUpdate` / `onLocalAwarenessUpdate` 传输的 plain-text binding |
| 关联 Issue/PR | 无 |
| 关联提交 | `7234b05`、`aee786b` |

## 1. 问题描述

### 1.1 问题场景

宿主没有使用标准 `y-websocket` provider，而是由 `YjsTextBinding` 的
`onLocalUpdate` 和 `onLocalAwarenessUpdate` 接入认证传输或本地 retry/outbox。当 outbox
写入、序列化或同步发送抛出异常时，Yjs 已经提交了本地 `Y.Text` 事务或 awareness local state。

### 1.2 具体表现

旧实现直接在同步 Yjs observer 中调用应用回调：

- 原始应用异常穿透给编辑器调用方，没有稳定的 `YjsBindingError` 错误码；
- `onError` 再抛错也会穿透 observer；
- update/awareness 出站字节超限只报告错误，不阻止下一次 binding-owned 写入；
- 调用方可能以为本次编辑没有成功而再次编辑，造成本地已提交但未明确交接状态的 update 链继续增长。

该问题在 beta 发布前的手动传输审计中发现；没有用户数据修复、线上告警或生产事故记录。

### 1.3 错误信息

受控复现中，`onLocalUpdate` 抛出 `Error("outbox unavailable")` 后：

```text
applyLocalReplacement -> Error: outbox unavailable
Y.Text -> "x"  # 事务已提交
```

`onLocalAwarenessUpdate` 和 `onError` 的抛错同样会以任意应用错误穿透。该输出是开发环境
复现，不包含生产日志或用户内容。

## 2. 临时解决方案（可选）

### 2.1 方案描述

无。没有采用“吞掉异常后继续编辑”、回滚已提交 Yjs 事务、或把回调返回当成服务端回执的临时
方案。

### 2.2 止血效果

不适用；在 beta 发布前直接完成根本修复和回归验证。

### 2.3 临时方案的局限

Yjs update observer 位于提交之后，客户端不能安全地伪造回滚；继续允许本 binding 写入会制造更多
交接未知的更新。只重试网络而不建立 caller-owned outbox 也不能处理进程退出、异步拒绝或持久
回执丢失。

## 3. 根本原因分析

### 3.1 问题分析过程

1. 以 Yjs 手动传输模型为基准，分别检查 update、awareness、文本投影、undo/redo 与 `onError`
   在同步 observer 中的执行顺序。
2. 用真实 `Y.Doc` 绑定抛错的 `onLocalUpdate`，确认 `applyLocalReplacement` 在 `Y.Text` 已变成
   `"x"` 后抛出原始 `Error`；排除“Yjs 会自动回滚 callback 异常”的假设。
3. 对 awareness 与 `onError` 重复实验，确认同样存在异常穿透；再以极小 `maxUpdateBytes`/
   `maxAwarenessBytes` 验证超限仅通知而不锁存。
4. 对比 `YjsDeepObserver.#fail`，该路径已经包住 `onError` 并避免重新进入同步 observer，说明
   binding 的回调边界不一致。
5. 在 Yjs transaction 已提交这一不可变前提下，选择“稳定错误码 + 单路径锁存 + 后续写入前检查”，
   而不是不可靠回滚或静默忽略。
6. 增加首次失败、超限、awareness、`onError` 二次抛错和 undo/redo 的测试，再运行完整 TypeScript、
   real Yjs store/relay 集成及两类受控 benchmark。

### 3.2 直接原因

`YjsTextBinding.#handleDocumentUpdate` 和 `#handleAwarenessUpdate` 直接调用用户提供的回调；
`#report` 也直接调用 `onError`。出站容量超限只调用 `#report("resource_limit")`，没有保存失败
状态。`applyLocalReplacement` 与 `YjsTextUndoManager.undo/redo` 因而既无法把已提交后的失败转为
稳定领域错误，也不会阻断下一次 binding-owned 写入。

**相关代码位置**：修复后的 `clients/typescript/src/yjs.ts` 中
`#handleDocumentUpdate`、`#handleAwarenessUpdate`、`#failLocalUpdate`、
`#failLocalAwareness`、`applyLocalReplacement` 与 `YjsTextUndoManager.undo/redo`。

### 3.3 根本原因

- **设计层面**：初版明确了“一个文档只有一个 transport owner”，但没有把同步 callback 定义为
  交接点、非持久回执，也没有定义 callback 失败后的 document/编辑器恢复状态机。
- **开发层面**：实现覆盖了远端 origin 不回声和入站解码上限，却把本地 callback 当作不可失败的
  函数调用，漏掉了 outbox、发送和错误上报同样是应用代码。
- **流程层面**：既有回归检查正常的手动转发，却没有故意让 callback、`onError` 或本地生成的
  出站大小限制失败，并在 Yjs 提交顺序下验证后续写入。

### 3.4 为什么没有提前发现

- **代码审查阶段**：审查了传输所有权和 inbound byte limits，没有将“observer 后回调”列为
  commit-after failure boundary。
- **测试阶段**：只验证了成功 callback 与远端不回声，没有验证 callback 抛错、上报回调抛错、
  outbound cap 或失败后的 undo/redo。
- **监控告警**：没有稳定错误码，产品也无法安全聚合 callback-failure latch 与恢复次数。

## 4. 解决方案

### 4.1 根本解决方案

**修改文件**：`clients/typescript/src/yjs.ts`

增加 `local_update_failed` 和 `local_awareness_failed` 错误码；document 与 awareness observer
将回调异常转换为相应失败状态。第一次失败前先锁存路径，再调用受保护的 `onError`。本地文本、
undo、redo 和本地 cursor 写入在 mutation 前检查路径，并在触发 observer 后再次检查，以便把
已提交但未交接的首次失败返回为稳定 `YjsBindingError`；之后在 mutation 前拒绝新的写入。

**测试文件**：`clients/typescript/test/yjs.test.mjs`

覆盖 update/awareness callback 抛错、`onError` 抛错、两种本地出站超限、首次事务已提交、后续
写入锁存和 undo 失败后 redo 被拒绝。

**方案说明**：选择按 update 和 awareness 独立锁存，避免一个临时 presence 失败不必要地阻断
文本恢复。没有回滚 Yjs：回调在 commit 后触发，补偿性编辑会生成新的 update 且可能改变并发
合并语义。没有在 binding 内持久化 update：持久化、加密、身份、重试和 receipt 属于已认证
应用传输层。`onError` 只接收代码，任何其自身异常都会被忽略，避免同步 observer 重入。

### 4.2 影响范围评估

- 使用标准 provider、未配置 `onLocal*` 回调的 binding 不会进入该手动路径。
- 手动传输在 callback 成功时的 update bytes、V1/V2、state vector、room/relay/store 协议均不变。
- callback 或本地出站 cap 失败后，调用方必须停止当前 surface、恢复 outbox 并按既有同步策略
  新建 binding；这是显式 fail-closed 行为，不能假定旧 binding 可自动重试。
- awareness 依旧是临时态，不得用于 durable receipt、审计或授权；文本 durable receipt 仍由
  `YJSStore`/应用 outbox 等上层契约决定。

## 5. 预防措施

### 5.1 代码层面

- [x] 所有 binding-owned 同步 callback 失败都转换为稳定领域错误，而非透传任意应用异常。
- [x] 对 commit-after callback boundary 先锁存，再报告，再阻止后续本地写入。
- [x] `onError` 不能重新进入同步 Yjs observer。
- [ ] 把 callback/outbox 的同步交接、异步失败和 durable receipt 分开列入所有浏览器传输接口的
  设计检查清单。

### 5.2 测试层面

- [x] 维护 update、awareness、cap、错误回调、undo/redo 的失败锁存回归。
- [x] 保留真实 CodeMirror/JSDOM、V1/V2、state-vector、三副本乱序和 real YJSStore/relay 验证。
- [ ] 在实际浏览器和产品 editor schema 中模拟 outbox 磁盘满、断网、Promise rejection、进程重启
  与 1/4/16/64 receiver 恢复。

### 5.3 监控层面

- [ ] 记录匿名聚合的 `local_update_failed`、`local_awareness_failed`、outbound-cap-latched 与
  recovery duration 指标；不得包含 update bytes、文档文本、cursor、token 或用户身份。

### 5.4 流程/规范层面

- [x] 中英文 Yjs 集成文档明确回调是同步交接、不是持久回执，并定义恢复步骤。
- [ ] 每次新增 observer/callback 时，代码审查必须列出：执行时机（commit 前/后）、错误归属、
  是否可重入、失败后是否继续写入，以及 durable receipt 由谁提供。

## 6. 经验总结（一句话）

> 在 Yjs 这类提交后同步 observer 中，应用回调失败不能伪装成可回滚操作；必须把它转成稳定错误、锁存后续本地写入，并把重试和回执明确留给应用自有 outbox/传输层。
