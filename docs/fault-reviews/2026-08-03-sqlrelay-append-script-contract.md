# 故障复盘：SQL relay 追加测试脚本保留失效状态参数

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-08-03 |
| 发现人 | `beta -> preprod` PR #44 的 golangci 门禁 |
| 严重程度 | P1-严重 |
| 影响范围 | SQL relay 追加事务测试、SQL Server durable log 合入后的预发门禁 |
| 关联 Issue/PR | #44 |
| 关联提交 | 待本次修复提交生成 |

## 1. 问题描述

### 1.1 问题场景

SQL Server durable log 复用了 `providers/internal/sqlrelay` 的追加事务测试。测试辅助函数 `appendScript` 仍声明 `highWater`、`count`、`usedBytes` 三个状态参数，但所有调用都传入零。

### 1.2 具体表现

全模块 lint 报告：

```
providers/internal/sqlrelay/store_test.go:479:91: appendScript - highWater always receives 0 (unparam)
```

移除第一个参数后，lint 继续证明 `count` 也始终为零，说明辅助函数的调用契约已经退化为固定的空组追加场景。

## 2. 根本原因分析

### 2.1 问题分析过程

1. 在 OR-Set 修复重放到最新 beta 后运行全模块 golangci，定位到 sqlrelay 测试辅助函数。
2. 枚举全部调用点，确认四处均传入三个零值，且没有测试覆盖非零 group 状态。
3. 检查生成的事务脚本，发现 LockGroup 的返回行和 UpdateGroup 的期望参数必须保持一致，不能只删除形参。
4. 将辅助函数明确收窄为“空组首次追加”脚本：锁定行三项均为零，更新行固定为 sequence=1、count=1、usedBytes=encoded 长度。

### 2.2 直接原因

测试辅助函数的签名仍反映早期可配置状态，但调用者和测试意图只保留固定初始状态。

### 2.3 根本原因

- **设计层面**：测试 fixture 的职责没有随着复用范围收窄而同步收敛。
- **开发层面**：新增 provider 时执行了特定包测试，但合并候选前的全模块 unparam 检查才覆盖到该冗余接口。
- **流程层面**：新模块进入 `go.work` 后，发布候选首次按完整模块列表执行门禁。

### 2.4 为什么没有提前发现

普通测试只校验 mock 事务是否匹配，无法识别永远相同的参数；该问题需要静态的全调用点分析。

## 3. 解决方案

### 3.1 根本解决方案

删除三个无变化的参数和全部零值实参，同时把 mock 的 LockGroup 与 UpdateGroup 期望值显式固定为首次追加的真实状态。若将来需要覆盖非零 group 状态，应新增一个命名的测试脚本，而不是重新扩张这个 fixture 的隐式参数。

### 3.2 影响范围评估

- 仅修改测试 fixture，不改变 durable log 的 SQL、事务隔离、运行时状态机或 wire 协议。
- `providers/internal/sqlrelay` 与 `providers/mssql` 的普通和 race 测试均通过；全模块 golangci 恢复 0 issues。

## 4. 预防措施

### 4.1 代码层面

- [x] 测试 fixture 仅暴露实际变化的输入。
- [x] 固定状态的事务预期用明确常量表达。

### 4.2 测试层面

- [x] 对 sqlrelay 与 mssql 运行普通和 race 测试。
- [x] 在最新 beta 合并候选执行全模块 golangci。
- [ ] 为非零 group 状态新增独立的追加测试脚本。

## 5. 经验总结（一句话）

测试辅助函数的参数必须代表真实变化维度；全部调用点恒定的状态应收窄为清晰的 fixture 契约，而不是保留伪通用接口。
