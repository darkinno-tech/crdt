# 故障复盘：预发晋级正式后稳定标签未自动发布 Release

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-08-03 |
| 发现人 | 用户反馈与发布链路核验 |
| 严重程度 | P1-严重 |
| 影响范围 | `v1.0.30` 至 `v1.0.32` 的 GitHub Release 未创建；稳定 Go 模块标签仍已发布 |
| 关联 Issue/PR | #42、#43；后续修复 PR 待创建 |
| 关联提交 | 待本次修复提交生成 |

## 1. 问题描述

### 1.1 问题场景

受保护的 `preprod -> main` PR 合并后，Release 工作流会创建根模块和嵌套 Go 模块的稳定标签。用户发现稳定版本标签已经存在，但对应 GitHub Release 没有自动生成。核验下一版本的打标 dry-run 时还发现：嵌套模块必须先把内部依赖升级为目标稳定版本，否则正式 job 会在打标前失败。

### 1.2 具体表现

`v1.0.32` 及其模块标签指向已合并的正式提交，但 `gh release view v1.0.32` 返回“release not found”。最近可见的 GitHub Release 停留在 `v1.0.29`，且其 assets 为空。

### 1.3 运行证据

Release run `30755501939`（#43 合并后）中：

- `create-stable-tag` 成功；
- `publish-notes` 因其仅匹配 tag push 或手动触发的条件而跳过；
- 稳定标签 `v1.0.32` 已存在，GitHub Release 不存在。
- 修复前执行 `./scripts/tag-go-modules.sh v1.0.33 HEAD --dry-run` 时，`durable/go.mod` 仍要求 `github.com/DarkInno/crdt v1.0.32`，与目标 `v1.0.33` 不一致而失败。

## 2. 临时解决方案

现有 `workflow_dispatch` 可以对已有稳定标签手动创建 Release。该路径仅用于回补 `v1.0.30` 至 `v1.0.32`，不能作为后续正式发布的常规流程。

## 3. 根本原因分析

### 3.1 问题分析过程

1. 核验 #43 的合并提交、稳定标签和 GitHub Release，确认标签与 Release 状态不一致。
2. 查看 `.github/workflows/release.yml`，发现 `create-stable-tag` 只在合并后的 PR 事件中创建标签；`publish-notes` 只在 tag push 或手动触发中创建 GitHub Release。
3. 查看运行 `30755501939`，确认前者成功而后者在同一 PR 事件中跳过。
4. 对照 GitHub Actions 事件规则：使用仓库 `GITHUB_TOKEN` 推送的 tag 不会再触发新的 `push` 工作流，因而 `publish-notes` 永远收不到本发布 job 创建的 tag。

### 3.2 直接原因

`release.yml` 将相互依赖的“创建稳定标签”和“创建 GitHub Release”拆到了两个依赖事件中，而标签由 `GITHUB_TOKEN` 推送。

**相关代码位置**：`.github/workflows/release.yml` 的 `create-stable-tag` 与 `publish-notes` jobs。

### 3.3 根本原因

- **设计层面**：发布状态机将 token 触发的 Git 事件误当作能驱动后续 GitHub Actions workflow 的可靠消息。
- **版本一致性层面**：稳定标签脚本正确要求所有嵌套模块的内部依赖与目标版本一致，但发布准备没有在候选阶段提前更新和校验该版本图。
- **开发层面**：新建 `preprod -> main` 自动打标路径时，没有将“标签已创建但 Release 未创建”作为独立状态校验。
- **流程层面**：发布验收只验证了标签和 CI 成功，未验证对应 GitHub Release 实体与版本产物状态。

### 3.4 为什么没有提前发现

- 预发门禁覆盖代码质量和模块标签，但不覆盖 Release 实体存在性。
- `create-stable-tag` 成功使整个 workflow 显示成功，掩盖了 `publish-notes` 的预期跳过。
- 缺少合并后针对 `tag -> GitHub Release` 的端到端验收。

## 4. 解决方案

### 4.1 根本解决方案

在受保护的 `preprod -> main` 合并 job 内，在已完成的 `origin/main == merge SHA` 防陈旧校验和稳定标签创建后，直接幂等调用 `gh release create`。

该步骤：

- 仅接受稳定 `vMAJOR.MINOR.PATCH` 标签；
- 遇到陈旧合并明确跳过；
- 已存在 Release 时安全退出；
- 对外部认证创建 Release 的并发竞态复查 Release 后再判定成功；
- 保留 `publish-notes` job 供外部 tag push 和人工回补使用。

同时将 11 个嵌套 Go 模块中指向仓库内部模块的依赖，以及 `go.work` 中对应的 12 条本地 replace，从 `v1.0.32` 提升至 `v1.0.33`。这与 `scripts/tag-go-modules.sh v1.0.33 HEAD --dry-run` 使用同一规则，确保候选在进入正式发布前已证明模块版本图可打标、可在工作区构建。

### 4.2 影响范围评估

- 不改变 `beta -> preprod` 的完整质量门禁或预发布标签逻辑。
- 不放宽 `main` 的 PR、`preprod-only`、无 force-push、无删除保护。
- 根稳定标签继续通过 `scripts/tag-go-modules.sh` 原子地与嵌套 Go 模块标签一起发布。
- 仅调整仓库内部模块的版本约束；不升级第三方依赖、不改变运行时代码或协议兼容性。
- 当前工作流没有二进制构建或上传步骤；本修复恢复 GitHub Release 实体，不虚构二进制包产物。

## 5. 预防措施

### 5.1 代码层面

- [x] 将 token 内部事件的后续副作用放入同一受保护 job，避免依赖被抑制的工作流触发。
- [x] 使标签与 Release 创建均具备稳定格式校验和幂等处理。

### 5.2 测试层面

- [ ] 在正式 PR 合并后核验：稳定根/模块标签、GitHub Release、目标 SHA 和 Release assets 状态。
- [ ] 对陈旧合并、重复运行、已存在标签无 Release、并发创建四种路径做 workflow 级演练。
- [x] 在候选中执行模块标签 dry-run 和全部模块 `go mod verify`，以捕获版本约束与工作区 replace 不一致。

### 5.3 监控层面

- [ ] 为“稳定标签存在但 GitHub Release 不存在”增加发布后巡检与告警。

### 5.4 流程/规范层面

- [ ] 发布验收清单明确区分：CI 通过、标签创建、GitHub Release 创建、二进制/包产物上传。

## 6. 经验总结（一句话）

由 `GITHUB_TOKEN` 创建的稳定标签不能作为下一工作流的可靠触发器，预发晋级正式所需的标签与 Release 副作用必须在同一受保护、可幂等验证的发布 job 内完成。
