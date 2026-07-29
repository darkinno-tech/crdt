# 故障复盘：过期 CI run 尝试为已推进分支打标

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-07-29 |
| 发现人 | CI 校验 |
| 严重程度 | P2-一般 |
| 影响范围 | beta 预发布打标 job；测试结果与已发布 tag 未受影响 |
| 关联 Issue/PR | #11 |
| 关联提交 | 待本次修复提交 |

## 1. 问题描述

beta 在 `66061e1` 的测试尚未完成时推进到 `e830f53`。旧 run `30421987905` 的测试、race、vet、fuzz、覆盖率、Docker、lint 和 staticcheck 都成功，但其 `prerelease-tag` job 仍尝试为过期 SHA 创建 `v1.0.7-beta.2`，并失败。

关键错误：

```
remote rejected ... refusing to allow a GitHub App to create or update workflow .github/workflows/test.yml without workflows permission
```

## 2. 根本原因分析

打标脚本在测试完成后只执行 `git fetch --force --tags origin`，没有比较远端受保护分支的当前 SHA 与触发该 run 的 `GITHUB_SHA`。因此分支已前进时，过期 run 仍会尝试打标旧提交；涉及 workflow 文件的旧提交会被 GitHub App 权限规则拒绝。

此前只覆盖了单一分支 head 的成功打标，未覆盖“测试运行期间再次 push”的竞态。

## 3. 解决方案

修改 `.github/workflows/test.yml`：

- beta/main 打标前显式刷新对应远端分支；
- 仅当 `origin/beta` 或 `origin/main` 与 `GITHUB_SHA` 完全一致时创建 tag；
- 过期 run 成功退出并报告 `skipped-stale`，不创建或替换 tag。

这保证每个自动 tag 都对应当时仍是目标分支 head 的提交；后续 head 的独立 workflow 负责打标。

## 4. 预防措施

- [x] 在 beta 与 main 的打标路径使用同一 head-equality 防护。
- [x] 用连续 beta push 的真实 CI 复验最新 head 可以通过并打标。
- [ ] 未来修改发布 workflow 时，保留“过期 run 不得产生发布副作用”的审查项。

## 5. 经验总结

> CI 的测试结论可以属于历史 SHA，但发布副作用必须只作用于当前目标分支 head。
