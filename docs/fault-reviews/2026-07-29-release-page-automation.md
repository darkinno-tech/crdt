# 故障复盘：正式标签未生成 GitHub Release 页面

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-07-29 |
| 发现人 | 用户 |
| 严重程度 | P2-一般 |
| 影响范围 | GitHub Releases 页面与版本说明可见性 |
| 关联 Issue/PR | Release CI PR 待创建 |
| 关联提交 | `18d2f63`（正式 tag 自动化） |

## 1. 问题描述

### 1.1 问题场景

`main` 已自动生成正式标签 `v1.0.1` 与 `v1.0.2`，但 GitHub Releases 页面最初没有
对应发布条目和自动生成的变更说明。

### 1.2 具体表现

查询 `GET /repos/DarkInno/crdt/releases/tags/v1.0.1` 返回 404；远端 Git tag 本身
存在并指向 `main` 合并提交。

## 2. 根本原因分析

### 2.1 问题分析过程

1. 检查 `v1.0.1`，确认标签已推送且指向正式发布提交。
2. 查询 GitHub Releases API，得到 404，确认不是页面缓存或权限问题。
3. 检查 `.github/workflows/test.yml`，发现 `release-tag` 只执行 `git tag` 与
   `git push origin <tag>`。
4. 新增独立的 tag 触发工作流后，`v1.0.1` 的手动 dispatch 可以创建 Release，但
   `release-tag` 通过 `GITHUB_TOKEN` 推送的 `v1.0.2` 没有触发该工作流。
5. GitHub 为避免递归，不会让 `GITHUB_TOKEN` 创建的 push 触发新的 workflow；因此
   自动发布链路必须在创建 tag 的同一个 job 内创建 Release。

### 2.2 直接原因

`.github/workflows/test.yml` 的正式发布 job 未配置创建 GitHub Release 的步骤；仅依赖
独立 tag workflow 也无法覆盖其 `GITHUB_TOKEN` 推送的标签。

### 2.3 根本原因

发布流程将 Git tag 与 GitHub Release 视为同一产物，且未考虑 Actions 令牌推送不会
触发下游工作流的事件边界。

### 2.4 为什么没有提前发现

验收只核对了远端 tag 是否存在，未核对 Releases API 是否能读取同名发布。

## 3. 解决方案

在 `.github/workflows/test.yml` 的 `release-tag` job 内直接执行
`gh release create --verify-tag --generate-notes`，使正式 tag 和 Release 同一事务式
发布；已存在 Release 时幂等退出。`.github/workflows/release.yml` 保留用于外部推送的
稳定 tag 与 `workflow_dispatch` 补发，beta 预发布 tag 始终跳过。

## 4. 预防措施

- [x] Actions 创建的正式 tag 在同一 job 中创建 GitHub Release。
- [x] 独立 Release 工作流仅处理外部 stable tag 与手动补发。
- [x] 只接受稳定 SemVer，beta 预发布标签不创建正式 Release。
- [x] 已存在 Release 时幂等退出，避免覆盖手工编辑的发布说明。
- [ ] 发布验收同时核对 tag 指向和 Releases API 的发布条目。

## 5. 经验总结

Git tag 只标记提交；对外发布还必须显式创建带说明的 GitHub Release，并验证其页面存在。
