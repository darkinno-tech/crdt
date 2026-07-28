# 故障复盘：Change 未绑定 manifest 导致旧 epoch 增量可能进入新复制组

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-07-29 |
| 发现人 | Codex |
| 严重程度 | P2-一般 |
| 影响范围 | `replica.Inbox` 的 checkpoint 重启与 epoch 切换路径；尚未发布的 `replica` 包 |
| 关联 Issue/PR | 无 |
| 关联提交 | 未提交 |

## 1. 问题描述

### 1.1 问题场景

复制组完成 checkpoint 重基并将 manifest 的 epoch 从 1 切换到 2 后，旧成员可能重试 epoch 1 中尚未送达的 delta。旧、新 epoch 若使用同一个 StateID、DeltaID、CodecID 和语义版本，该 delta 的帧外形完全相同。

### 1.2 具体表现

原 `Change` 只保存 `Dot` 和 delta 字节。`Inbox.Receive` 仅验证 delta 的 TypeID 与 CodecID，因此它无法区分同一组在不同 epoch 产生的帧；只要 Dot 高于新 Inbox 的 frontier，旧 epoch 增量可能被应用。

### 1.3 错误信息

无运行时日志。该缺口由“旧 epoch 必须被拒绝”的新增测试设计发现。

## 2. 根本原因分析

### 2.1 问题分析过程

1. 检查 checkpoint 恢复测试时确认，frontier 可以跳过已经纳入 checkpoint 的旧 Dot。
2. 检查 epoch 切换时发现 `Manifest` 存在于 `Inbox`，但 `Change` 的值中没有组、schema 或 epoch 身份。
3. `NewChange` 虽接收 manifest 参数，却只用它校验 delta 帧外形，随后丢弃该上下文。
4. 因此相同协议和 codec 下的跨 epoch 帧无法在 `Inbox.Receive` 处分辨，最终定位为消息身份缺失而非 frontier 算法错误。

### 2.2 直接原因

**相关代码位置**：`replica/replica.go:193-230`

修复前 `Change` 没有 manifest 字段，接收端只检查 delta 的 TypeID 和 CodecID。

### 2.3 根本原因

- **设计层面**：把 manifest 视为连接建立时的一次性协商信息，没有把它建模为每个可重试、可持久化 change 的身份约束。
- **开发层面**：初始测试覆盖了类型、codec、乱序和重复投递，但没有覆盖“同协议、不同 epoch”的帧重放。
- **流程层面**：checkpoint/epoch 的测试先覆盖了持久化确认，未先建立旧 epoch 重放的拒绝矩阵。

### 2.4 为什么没有提前发现

- 代码审查阶段只审查了 frame 与 frontier 的本地正确性，没有追踪 change 从旧 checkpoint 生命周期重放到新 Inbox 的完整链路。
- 测试阶段的伪 delta 使用同一 manifest，无法触发跨 epoch 身份缺失。
- 包尚未接入生产传输层，因此没有运行时告警。

## 3. 解决方案

### 3.1 根本解决方案

`Change` 私有保存创建它的 `Manifest`；`Inbox.Receive` 在解码 delta 前调用 `Change.validate`，要求接收 Inbox 的 manifest 与 change manifest 完全兼容。组、schema、epoch 不同返回 `ErrManifestMismatch`，协议或语义版本不同返回 `ErrProtocolMismatch`。

**修改文件**：`replica/replica.go:193-230`

同时新增两类回归测试：

- `replica/replica_test.go:525`：同协议不同 epoch 的 change 必须被拒绝，新 epoch 的 change 可以被应用。
- `replica/replica_test.go:553`：从真实 G-Counter checkpoint 恢复后，Dot 小于等于 checkpoint frontier 的增量被忽略，未来 Dot 正常应用。

### 3.2 影响范围评估

`Change` 的 manifest 是私有字段，外部调用方式保持 `NewChange(manifest, dot, delta)` 不变。旧 epoch change 会从“可能被接收”改为确定拒绝；这是 checkpoint 重基的预期安全行为。

## 4. 预防措施

### 4.1 代码层面

- [x] 所有可持久化或可重试的复制消息绑定组、schema、epoch 和协议语义身份。
- [x] 接收端在调用具体 CRDT `ApplyDelta` 前验证消息身份。

### 4.2 测试层面

- [x] 增加旧 epoch 重放拒绝测试。
- [x] 增加真实 G-Counter checkpoint 恢复与未来 delta 接续测试。
- [ ] RGA 与 OR-Tree 实现 checkpoint 重基前，增加“旧锚点/旧父引用不能跨 epoch 复活”的同类测试。

### 4.3 监控层面

- [ ] 传输适配层接入后，记录并统计 `ErrManifestMismatch` 与 `ErrProtocolMismatch`，以识别滞后副本或错误部署。

### 4.4 流程/规范层面

- [ ] 协议评审清单增加“消息是否携带足够的 epoch/版本身份，以安全拒绝重放”条目。

## 5. 经验总结（一句话）

> 复制协议的 manifest 不能只在握手时校验；任何可能延迟、重试或持久化的 change 都必须携带并在接收端验证其 epoch 和语义身份。
