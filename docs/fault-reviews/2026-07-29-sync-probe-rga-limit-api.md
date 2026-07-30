# 故障复盘：sync-probe RGA 受限编码 API 缺失

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-07-29 |
| 发现人 | 代码审查 |
| 严重程度 | P2-一般（提交前构建阻断） |
| 影响范围 | `cmd/crdt-sync-probe` 的新增 RGA 诊断投递路径；未进入已发布版本 |
| 关联 Issue/PR | 待创建 |
| 关联提交 | 待提交：`feat(probe): add explicit bounded RGA diagnostics` |

## 1. 问题描述

### 1.1 问题场景

新增的 `crdt-sync-probe` RGA 发送路径需要按接收端相同的帧限制编码 delta，以避免
诊断工具在生成端绕过 16 MiB/元素上限。实现调用了
`text.Delta.MarshalBinaryWithLimits`，并计划为 `run-v2` 做等价处理。

### 1.2 具体表现

当该未提交版本参与构建时，探针依赖的公开 `text` API 不存在，RGA 路径无法完成编译；
即使改用默认编码，也会失去发送端与接收端使用同一受限帧边界的保证。

### 1.3 错误信息

接口比对结果：`cmd/crdt-sync-probe/main.go` 调用
`Delta.MarshalBinaryWithLimits(...)`，而当时 `text/wire.go` 仅公开
`Delta.MarshalBinary()`；`run-v2` 也只有未导出的受限解码内部函数。

## 2. 根本原因分析

### 2.1 问题分析过程

1. 审查新增探针代码时发现发送端调用了不存在的受限编码方法。
2. 检查 `text/wire.go`，确认内部已有 `marshalRGAWithLimits`，但没有 `Delta` 的公开包装。
3. 检查 `text/wire_run.go`，确认 `run-v2` 同样缺少对外的受限 marshal/unmarshal 对称 API。
4. 对比接收路径可知，若发送端退回默认限制，诊断工具不能证明其生成帧满足配置的接收上限。
5. 最终定位为：调用方先扩展、底层受限 API 后补，且没有先运行完整包构建来验证公开 API 闭环。

### 2.2 直接原因

**相关代码位置**：

- `cmd/crdt-sync-probe/main.go:398-435`
- 修复后 `text/wire.go:27-36`
- 修复后 `text/wire_run.go:39-48,204-216`

探针需要使用受限编码，但原公开 API 只有默认限制版本，导致调用契约不成立。

### 2.3 根本原因

- **设计层面**：v1 与 run-v2 的受限编码 API 没有作为成对边界设计，内部能力未暴露给受限传输调用方。
- **开发层面**：新增命令与底层 API 的改动没有在同一编译闭环中完成。
- **流程层面**：提交前未先执行受影响包的 `go test`，未能尽早暴露 API 缺口。

### 2.4 为什么没有提前发现

- 原有 `text` 测试只覆盖已有公开 API，无法发现一个尚未纳入构建的命令调用。
- 新路径缺少 v1/run-v2 受限 encode/decode 的端到端测试。
- 文档先描述了 RGA 扩展，但没有将“显式协议选择 + 同一限制编码”固化为可执行检查。

## 3. 解决方案

### 3.1 根本解决方案

1. 新增 `Delta.MarshalBinaryWithLimits`，复用既有 canonical v1 编码与限制校验。
2. 新增 `Delta.MarshalRunBinaryWithLimits` 和
   `UnmarshalRGARunDeltaWithLimits`，保持 run-v2 的显式、受限对称边界。
3. 将 probe 的 RGA 路径默认设为 `disabled`；只有发送端和接收端都显式选择同一个
   `v1` 或 `run-v2` 才开放 `/rga`。
4. 所有成功投递返回空 `204`，最终 `/state` 才构造文本摘要；保留字节、rune、节点、
   pending 和非回环监听的边界。

### 3.2 影响范围评估

- v1 与 run-v2 既有默认 API 保持不变；新增方法只提供调用方自选限制。
- `crdt-sync-probe` 的 counter/OR-Set 路径继续可用；RGA 是默认关闭的新诊断面。
- run-v2 没有被提升为稳定协议，也不与 v1 自动互通。

## 4. 预防措施

### 4.1 代码层面

- [x] 对所有不可信传输增加“发送端受限编码 + 接收端受限解码”的成对 API。
- [x] 对实验性 RGA 要求显式协议值，拒绝默认猜测或 v1/run-v2 混用。
- [x] 以回环监听为默认强制边界，外部监听必须显式 opt-in。

### 4.2 测试层面

- [x] 覆盖 v1/run-v2 两种 RGA 投递、重复投递、协议不匹配、禁用路由与边界输入。
- [x] 覆盖公开受限 marshal/unmarshal API 的 round-trip 与限制拒绝。
- [x] 增加 v1/run-v2 编码解码基准，避免将压缩字节数误解为端到端延迟收益。

### 4.3 流程层面

- [x] 在提交前执行受影响包测试、竞态、模糊、覆盖率与完整仓库门禁。
- [ ] 后续新增 CLI 调用公开 API 时，先以最小 `go test ./cmd/<tool>` 验证 API 存在，再扩展文档和场景。

## 5. 经验总结（一句话）

> 受限传输不能只在接收端校验：发送端编码、协议选择与接收端解码必须使用可编译、可测试的一组显式边界 API。
