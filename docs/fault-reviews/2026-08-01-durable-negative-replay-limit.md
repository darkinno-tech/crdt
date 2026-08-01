# 故障复盘：负值 replay 限额在无符号转换前未拒绝

## 基本信息

| 字段 | 内容 |
| --- | --- |
| 日期 | 2026-08-01 |
| 发现人 | beta 发布候选静态检查 |
| 严重程度 | P1-严重 |
| 影响范围 | `durable.Config` 的 `MaxReplayEvents` 与 `MaxReplayBytes` 资源边界 |
| 关联 Issue/PR | 无 |
| 关联提交 | 本次 beta 稳定化修复 |

## 1. 问题描述

### 1.1 问题场景

durable relay 在启动时将 `Config` 中的有符号 replay 限额转换成 `uint64`。
负值没有先被拒绝时会发生环绕，得到极大的正整数，绕过原本用于限制回放事件数和字节数的
资源边界。

### 1.2 具体表现

`normalizeLimits(Config{MaxReplayEvents: -1})` 与
`normalizeLimits(Config{MaxReplayBytes: -1})` 在修复前可以继续进入默认值和范围校验流程，
而不是返回 `ErrInvalidConfig`。这会把部署配置错误转化为几乎无限的回放预算。

### 1.3 错误信息

发布候选 lint 报告：

```text
G115: integer overflow conversion int -> uint64
durable/handler.go: MaxReplayEvents / MaxReplayBytes
```

## 2. 根本原因分析

1. `Config` 使用 `int` 表达外部配置，但内部 `limits` 使用 `uint64` 表达非负资源预算。
2. 初版直接转换，没有在类型边界验证负值。
3. 后续的 `maxReplayEvents == 0` 与最小字节数检查无法识别已经环绕的值。
4. 单元测试覆盖了零值默认和过小的正值，未覆盖有符号到无符号的边界。

## 3. 解决方案

### 3.1 根本解决方案

- 在 `durable/handler.go` 的 `normalizeLimits` 开头拒绝任一负 replay 限额，再进行 `uint64`
  转换。
- 在 `durable/boundary_test.go` 增加事件数和字节数分别为 `-1` 的回归断言。
- 保持默认值、协议帧、存储格式和正常正值配置行为不变。

### 3.2 影响范围评估

这是 fail-closed 的配置校正：已有有效配置不变；负值配置现在在启动阶段被确定性拒绝，避免
在运行期以错误的超大预算保留或发送回放数据。

## 4. 预防措施

### 4.1 代码层面

- [x] 所有 signed -> unsigned 的资源限额转换前检查负值。
- [x] 保留最终上限/下限校验，防止默认值组合越界。

### 4.2 测试层面

- [x] 覆盖 `MaxReplayEvents` 和 `MaxReplayBytes` 的负值配置。
- [x] 在完整 beta 门禁中执行静态检查、race、fuzz 与覆盖率门槛。

### 4.3 流程/规范层面

- [ ] 审查新增配置字段时，逐项标记其符号、单位、默认值与最大资源影响。

## 5. 经验总结

> 资源预算跨越 signed/unsigned 边界时，必须在转换前 fail closed；后续范围校验不能修复已经发生的整数环绕。
