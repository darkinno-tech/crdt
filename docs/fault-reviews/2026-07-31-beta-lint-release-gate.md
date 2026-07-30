# 故障复盘：beta 增量 lint 未覆盖完整发布候选

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-07-31 |
| 发现人 | CI 发布列车 |
| 严重程度 | P1-严重 |
| 影响范围 | `beta -> main` 发布验证与 stable tag 生成 |
| 关联 Issue/PR | #31 |
| 关联提交 | `ab579489b35c22072343d552c7545b57e9f157ae` |

## 1. 问题描述

### 1.1 问题场景

`beta` 在连续集成多个提交时启用了取消旧运行的并发策略。最新 beta
SHA 的完整测试成功，但 `golangci-lint` 配置使用 `issues.new: true`；在
干净 CI checkout 中该模式只比较 `HEAD~`。当 beta 的中间运行被取消后，
更早提交引入的 lint 问题没有在 beta 阶段成为失败条件。

### 1.2 具体表现

PR #31 合并到 `main` 后，merge commit 相对旧 main 的完整差异暴露出 10
项 lint/security 问题，`golangci` job 失败，stable `v1.0.25` 未生成。

### 1.3 错误信息

主失败包括：

```text
QF1008: could remove embedded field "Config" from selector
G115: integer overflow conversion
G304: Potential file inclusion via variable
errorlint: non-wrapping format verb for fmt.Errorf
```

涉及 `persistence/file_store.go`、`list/move_wire.go`、
`richtext/semantic.go` 与 `persistence/file_store_test.go`。

## 2. 根本原因分析

1. 发布后 `main` 的 `golangci` job 失败，而同一 beta SHA 的普通测试、
   race、coverage、Docker、Wasm 和 fuzz 已成功。
2. 对比 `.golangci.yml` 与工作流发现 `issues.new: true` 在没有工作区改动
   时只检查 `HEAD~`，并不代表整个 beta 候选相对发布基线无新增问题。
3. beta 工作流会取消旧 run，因此最终成功的 run 只覆盖最后一个提交，无法
   替代候选范围的静态检查。
4. `list/move_wire.go` 还将 node 与 move 分别和 `MaxTags` 比较，未在
   分配 move map 前约束两者之和；攻击者可构造超过总标签预算的帧。

根本原因是把“单提交新增行检查”误作“可发布 beta 候选检查”，并缺少对
合并后工作流与 beta 门禁等价性的验证。

## 3. 解决方案

### 3.1 根本解决方案

- `.golangci.yml` 使用 `new-from-merge-base: origin/main` 与
  `whole-files: true`，检查 beta 候选相对发布基线的所有已改文件。
- golangci job 以 `fetch-depth: 0` 获取 `origin/main`，保证比较基线存在。
- `list/move_wire.go` 在分配前将 node+move 计入同一标签预算，并新增恶意
  帧回归测试。
- `persistence/file_store.go` 明确路径威胁边界，保留 Lstat/权限检查并增加
  symlink 拒绝测试；同时修复安全转换、嵌入字段选择和错误链。
- `richtext/semantic.go` 使用无窄化转换的块级格式化实现。

### 3.2 影响范围评估

协议帧格式和公开 API 不变。Move-RGA 仅拒绝原本越过声明资源上限的畸形
输入；FileStore 仍要求调用方提供受保护的应用路径。

## 4. 预防措施

- [x] beta lint 基于 `origin/main` 的 merge-base，而不是单一 `HEAD~`。
- [x] 对累计 node+move 标签预算增加解码器回归测试。
- [x] 对 FileStore symlink 拒绝和并发读取错误链增加测试。
- [ ] 每次 beta->main PR 合并前确认 beta 的 CI SHA 与 PR head SHA 相同。
- [ ] 当主分支工作流跳过 beta 已执行的昂贵任务时，保留 beta 上等价且更强的门禁。

## 5. 经验总结

> 取消旧 CI run 时，发布门禁必须相对稳定发布基线检查完整候选差异；只检查
> `HEAD~` 不能证明多提交 beta 列车可发布。
