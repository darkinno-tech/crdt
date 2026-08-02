# 故障复盘：相对位置 anchor 的枚举收窄未显式映射

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-08-02 |
| 发现人 | `golangci-lint` 在最新 beta 基线的回归 |
| 严重程度 | P2-一般 |
| 影响范围 | rich-text 相对位置 metadata 的 `AnchorAssociation` 解码；未观察到协议错误或用户数据影响 |
| 关联 Issue/PR | 无 |
| 关联提交 | 引入 `4fa4632`；修复提交待生成 |

## 1. 问题描述

### 1.1 问题场景

最新 beta 引入 relative anchor metadata 后，`readAnchorPayload` 从 frame uvarint 读取 association。代码先检查只接受 `AnchorBefore` 和 `AnchorAfter`，再将 `uint64` 强转为底层为 `uint8` 的枚举。

### 1.2 具体表现

全仓 lint 仅剩一项：`text/anchor_codec.go:168` 的 G115 `uint64 -> uint8`。逻辑上已有值域检查，但静态规则不会假设该检查始终与强转绑定。

### 1.3 错误信息

```text
text/anchor_codec.go:168:49: G115: integer overflow conversion uint64 -> uint8
anchor := Anchor{Association: AnchorAssociation(association)}
```

## 2. 临时解决方案

未对该行加入 lint 忽略，也未把未知值映射到默认 association。两种做法都会掩盖协议枚举必须 fail-closed 的要求。

## 3. 根本原因分析

### 3.1 问题分析过程

1. 初始同步长度告警修复后全仓 lint 仍报告此一项。
2. `git blame` 确认该行来自 `4fa4632`，不属于初始同步改动。
3. 审查 `AnchorAssociation` 确认为 `uint8`，合法值仅为 `1` 和 `2`。
4. 审查 decoder 发现它已经拒绝其他 uvarint，但其后仍存在通用窄化转换。
5. 用 `AnchorBefore` 初始化，并仅当 wire 值等于 `AnchorAfter` 时显式赋值，移除窄化转换。

### 3.2 直接原因

已验证的 `uint64` association 仍以类型转换写入 `AnchorAssociation`。

**相关代码位置**：`text/anchor_codec.go:159-182`（修复后）。

### 3.3 根本原因

- **设计层面**：wire 枚举的合法域是两个离散值，代码却使用了通用数值转换而非枚举映射。
- **开发层面**：值域校验与赋值分离，无法让安全门禁识别其封闭性。
- **流程层面**：该 rich-text 提交在初始同步分支完成后才进入 beta，最终全仓 lint 才显示此新的基线告警。

### 3.4 为什么没有提前发现

- **代码审查阶段**：语义正确性（未知 association 被拒绝）已覆盖，未将 lint 可证明性作为验收项。
- **测试阶段**：round-trip、未知值拒绝和 fuzz 均通过，因此无法显示静态收窄风险。
- **监控告警**：该类安全边界应由静态分析在合并前阻断。

## 4. 解决方案

### 4.1 根本解决方案

decoder 在已验证 association 后显式构造 `AnchorBefore`；只有 wire 值等于 `AnchorAfter` 时才切换到 `AnchorAfter`。未知值仍在前置检查处返回失败，且没有默认兼容或截断行为。

### 4.2 影响范围评估

- anchor wire 字节、合法值和 round-trip 结果不变。
- 非法值继续 fail-closed 为 `ErrInvalidAnchor`。
- 现有 before/after round-trip、未知 association 拒绝和 metadata fuzz 共同覆盖映射边界。

## 5. 预防措施

### 5.1 代码层面

- [x] wire enum 使用显式分支映射，不将不可信宽整数直接窄化为枚举。
- [ ] 新增协议 enum 时，在 decoder review 中检查每个合法值的穷举赋值。

### 5.2 测试层面

- [x] 运行 anchor round-trip、未知 association 拒绝、race 和 150,000 次 metadata fuzz。
- [ ] 每个新增 host metadata codec 在 beta 前执行全仓 golangci-lint。

### 5.3 监控层面

- [x] 保留 gosec G115 门禁；不记录 anchor 或文档内容作为诊断数据。

### 5.4 流程/规范层面

- [x] 将最新 beta 基线的 lint 重跑纳入协议工件推送流程。

## 6. 经验总结（一句话）

> 只有两个合法 wire 枚举值时，显式映射比经过验证后的数值窄化更安全、更可审计，也更能抵御后续修改绕开原有前提。
