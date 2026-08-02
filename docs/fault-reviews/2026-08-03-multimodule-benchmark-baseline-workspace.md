# 故障复盘：多模块预发基准被强制单模块解析

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-08-03 |
| 发现人 | 预发 PR #44 性能门禁 |
| 严重程度 | P1-严重 |
| 影响范围 | 拆分 Go 模块后的 `beta -> preprod` 性能门禁无法完成，阻断正式发布 |
| 关联 Issue/PR | #44 |
| 关联提交 | 待本次修复提交生成 |

## 1. 问题描述

### 1.1 问题场景

性能工作流把 preprod 基线 checkout 移至候选工作区外以保证候选与基线隔离，但仍为全部基准命令强制设置 `GOWORK=off`。模块拆分后，基线的嵌套 `examples` 模块依赖同一 checkout 中由 `go.work` 映射的内部模块。

### 1.2 具体表现

PR #44 的 performance job `91518299512` 失败，报错：

```
missing go.sum entry needed to verify package github.com/DarkInno/crdt/examples/websocket-provider/provider is provided by exactly one module
```

### 1.3 复现证据

以 `preprod@71758e7` 作为隔离 checkout：

- `cd examples && GOWORK=off go test ... ./websocket-provider/provider` 稳定失败；
- 在同一隔离 checkout 中不设置 `GOWORK=off`，根、text 和 provider 三个基准均通过。

## 2. 根本原因分析

### 2.1 直接原因

`.github/workflows/test.yml` 的基线命令在 checkout 已经物理隔离后仍禁止 Go 工作区，导致内部模块只能解析为远端版本；基线 `go.sum` 不需要、也不应包含由工作区提供的内部模块校验和。

### 2.2 根本原因

原先的修复只处理了“嵌套基线位于候选目录时误用候选 workspace”的问题，遗漏了模块拆分后的相反约束：隔离后的基线必须使用自己的 workspace，才能可靠地解析尚未以独立模块形式下载的内部依赖。

### 2.3 为什么没有提前发现

- 旧基线是单模块布局，`GOWORK=off` 没有暴露问题；
- 原有本地性能验证采用旧 preprod 基线，未覆盖模块拆分后的 checkout；
- 性能门禁没有针对“独立多模块基线”设置回归演练。

## 3. 解决方案

保留基线目录迁移到候选工作区外的隔离设计，删除 CI 基线命令的 `GOWORK=off`。这样候选与基准分别使用各自 checkout 的 `go.work`，不会交叉解析源码；单模块旧基线在未找到 `go.work` 时仍按自身 `go.mod` 正常运行。

本地 `make benchmark-regression` 已采用该语义；本次只将 CI workflow 恢复为相同的基线解析规则，并在注释中固化边界。

## 4. 预防措施

- [x] 使用当前多模块 `preprod@71758e7` 复现失败和验证修复。
- [ ] 为多模块隔离基线增加 workflow 级回归演练。
- [ ] 性能门禁变更同时覆盖旧单模块和当前多模块基线布局。

## 5. 经验总结

基线隔离解决的是源码边界，不是禁止 workspace；隔离后的多模块基线应使用自己的 `go.work`，而不是被强制降级为单模块模式。
