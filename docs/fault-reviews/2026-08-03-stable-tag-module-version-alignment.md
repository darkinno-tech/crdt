# 故障复盘：稳定标签在内部 Go 模块版本不一致时失败

## 基本信息

| 字段 | 内容 |
|---|---|
| 日期 | 2026-08-03 |
| 发现人 | CI 发布门禁 |
| 严重程度 | P1-严重 |
| 影响范围 | 根模块与 12 个可选 Go 子模块的稳定标签和 GitHub Release 发布 |
| 关联 Issue/PR | #57 |
| 关联提交 | `f4c49e9`（失败的 main 合并）；本次修复提交待定 |

## 1. 问题描述

### 1.1 问题场景

`preprod` 的候选 `v1.0.36-beta.4` 通过完整 beta -> preprod 验证后，经 #57 提升到 `main`。关闭 PR 后，`.github/workflows/release.yml` 计算下一稳定版本，并调用 `scripts/tag-go-modules.sh` 同时创建根模块和子模块标签。

### 1.2 具体表现

`create-stable-tag` 在创建任何标签前终止，导致 `v1.0.36`、子模块标签和 GitHub Release 均未创建；已经合并的 `main` 不会被回滚，也不会出现部分标签。

### 1.3 错误信息

GitHub Actions 作业 `30799130530/91639400202` 输出：

```
./durable/go.mod requires github.com/DarkInno/crdt v1.0.35; expected v1.0.36 for v1.0.36
```

## 2. 临时解决方案

没有绕过检查或手工创建标签。失败脚本在推送前校验所有模块，因此避免了根模块与子模块依赖版本不一致的不可恢复发布。

## 3. 根本原因分析

### 3.1 问题分析过程

1. `preprod-only` 只校验 PR 来源为仓库的 `preprod` 分支，因此 #57 可以合并。
2. 发布作业从最新稳定标签 `v1.0.35` 推导出目标 `v1.0.36`。
3. `scripts/tag-go-modules.sh` 扫描每个 `go.mod` 中的 `github.com/DarkInno/crdt` 依赖，并要求其等于目标标签版本。
4. `durable/go.mod` 首先暴露仍引用 `v1.0.35`；完整扫描确认其他子模块和 `go.work` 的本地替换也保持旧版本。
5. 因为这个可发布性条件仅在 main 合并后的写权限作业中执行，问题在受保护 PR 门禁之后才被发现。

### 3.2 直接原因

发布准备步骤缺少把内部模块引用从 `v1.0.35` 提升到 `v1.0.36` 的提交。

**相关代码位置**：

- `.github/workflows/release.yml:61-78` 推导稳定版本并执行真实标签创建。
- `scripts/tag-go-modules.sh:53-61` 拒绝与目标版本不同的内部模块依赖。
- `durable/go.mod:6-7`、`examples/go.mod:6-8`、`extensions/go.mod:6-7`、`persistence/go.mod:6`、`providers/**/go.mod`、`telemetry/go.mod:6` 原先均保留 `v1.0.35`。
- `go.work:19-43` 原先把工作区替换固定为 `v1.0.35`。

### 3.3 根本原因

- **设计层面**：源分支约束与稳定标签可创建性是两个独立不变量，但 `preprod-only` 只实现了前者。
- **流程层面**：完整 beta -> preprod 检查没有在 main 提升前模拟稳定标签，因此版本准备提交的遗漏无法提前暴露。

### 3.4 为什么没有提前发现

- 现有单元、竞态、fuzz 和性能检查验证运行时行为，不能验证将来发布标签所要求的模块版本一致性。
- 先前发布依赖人工执行 `chore(release): prepare modules for vX.Y.Z`；本次候选遗漏了该步骤。

## 4. 解决方案

### 4.1 根本解决方案

1. 将所有内部模块依赖和 `go.work` 的替换版本对齐为 `v1.0.36`。
2. 保留受保护的 `preprod-only` 名称，并在来源校验之后检出候选、计算下一稳定标签、运行 `scripts/tag-go-modules.sh <tag> <HEAD> --dry-run`。
3. 仅在 dry-run 通过后允许 PR 合入 `main`；真实标签仍只由合并后的发布作业创建。

**修改文件**：`.github/workflows/release-train.yml:32-70`、所有子模块 `go.mod`、`go.work:19-43`。

### 4.2 影响范围评估

- 运行时代码和协议字节未改动，不改变性能、资源或安全边界。
- `go.work` 把未发布的 `v1.0.36` 内部依赖解析到同一工作区，允许在标签创建前执行完整本地多模块测试。
- dry-run 不创建或推送标签；真实发布仍由 `main` 合并后的受控作业执行。

## 5. 预防措施

### 5.1 代码层面

- [x] `preprod-only` 同时验证源分支与稳定模块标签计划。
- [x] 发布脚本继续在写标签前校验所有内部依赖，拒绝部分发布。

### 5.2 测试层面

- [x] 发布准备提交运行 `scripts/tag-go-modules.sh v1.0.36 HEAD --dry-run`。
- [x] `make test` 和 `make vet` 覆盖根模块、12 个子模块与所有可选存储后端。

### 5.3 流程/规范层面

- [x] 将稳定标签 dry-run 固化为 main PR 的必需 `preprod-only` 门禁的一部分。
- [ ] 每次预备稳定发布时，先创建一个独立的 `chore(release): prepare modules for vX.Y.Z` 提交。

## 6. 经验总结

> 多模块发布的分支来源合法并不等于可发布；必须在进入 main 前用同一候选树演练标签依赖一致性。
