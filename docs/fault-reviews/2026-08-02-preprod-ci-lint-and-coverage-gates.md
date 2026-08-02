# 故障复盘：预发候选的静态检查与 Wasm 覆盖率门禁缺口

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-08-02 |
| 发现人 | Codex / GitHub Actions |
| 严重程度 | P2-一般 |
| 影响范围 | beta 到 preprod 的发布候选 PR #42；未合并、未部署 |
| 关联 Issue/PR | #42 |
| 关联提交 | 待本次分点提交 |

## 1. 问题描述

### 1.1 问题场景

预发候选 CI 同时运行跨 Go module 的 staticcheck、golangci-lint、每包 90% coverage 和相同 coverage 入口的 Docker 验证。

### 1.2 具体表现

首轮 PR #42 出现以下确定性门禁失败：

```text
text/sequence.go:94:6: func markerPriority is unused (U1000)
replica/inbox_delivery_test.go:72: comparing with != will fail on wrapped errors (errorlint)
internal/wasm coverage: 88.0%, below required 90%
performance baseline: main module does not contain package github.com/DarkInno/crdt/.benchmark-baseline/counter
```

Docker 任务通过 `Dockerfile.ci` 执行 `make coverage`，因此与第三项共享同一覆盖率失败根因；候选未合并，未产生预发布 tag 或环境写入。

## 3. 根本原因分析

### 3.1 问题分析过程

1. 读取 PR #42 注释，确认 staticcheck 和 golangci 的精确文件/行号。
2. 在隔离 beta 工作树运行 `make staticcheck`，复现已失去调用方的 `markerPriority`。
3. 运行 `make lint`，复现测试对 sentinel error 使用 `!=` 而非可包装的 `errors.Is`。
4. 运行 `make coverage`，定位 `internal/wasm` 的新增单锚点包装 API 尚未由测试直达，包覆盖率为 88.0%。
5. 补齐单锚点、未知句柄和 nil runtime 分支后，`internal/wasm` 覆盖率升至 92.5%，完整 `make coverage` 通过。
6. 下载 performance 任务日志，发现基线 checkout 位于候选根目录下，自动向上查找的候选 `go.work` 把 `.benchmark-baseline/*` 解释为根模块外的路径。
7. 首次在基线子树设置 `GOWORK=off`；Actions 日志仍将相对包路径解析为候选树，因此不把该环境行为当作可靠隔离。
8. 将 Actions checkout 的基线移动到 `$RUNNER_TEMP` 后再运行基准，并在本地以“嵌套普通 clone 后移至独立临时目录”的同构拓扑验证 root、examples 与完整基线比较通过。

### 3.2 直接原因

- `text/sequence.go` 保留了已不再被生产代码或测试使用的私有 helper。
- `replica/inbox_delivery_test.go` 固定比较 error 实例，违背项目其余测试的可包装 sentinel error 契约。
- `internal/wasm/richtext_test.go` 覆盖范围 API，但遗漏单锚点和 nil/未知 document 边界。
- `.github/workflows/test.yml` 的 performance job 在候选目录中嵌套 baseline checkout；仅依赖进程级 `GOWORK=off` 未能在 Actions runner 上可靠隔离相对包路径解析。

### 3.3 根本原因

重构后只验证了主功能路径，没有把“删除无调用 helper”“包装错误比较”“每个新增公开包装方法的成功/失败分支”以及“嵌套基线 checkout 的 workspace 边界”纳入同一发布前检查。

### 3.4 为什么没有提前发现

此前本地验证聚焦 YJSStore Node/Go 合约和性能，不会触发整个 beta 候选的 Wasm 每包 coverage、staticcheck 与 golangci 门禁。

## 4. 解决方案

### 4.1 根本解决方案

- 删除无调用的 `markerPriority`，保留由 `markerPriorities` 统一计算 entry/exit priority 的路径。
- 使用 `errors.Is(err, ErrInvalidChange)` 验证测试的 sentinel error。
- 为 `RichTextRuntime` 的单锚点 encode/decode/resolve、未知句柄和 nil runtime 增加真实边界测试。
- 在运行基准前把 `.benchmark-baseline` 移至 `$RUNNER_TEMP`，并在基线 root/examples 命令显式 `GOWORK=off`；候选命令保持现有多模块 workspace 行为。

**修改文件**：`text/sequence.go`、`replica/inbox_delivery_test.go`、`internal/wasm/richtext_test.go`、`.github/workflows/test.yml`。

### 4.2 验证结果

```text
make staticcheck  PASS
make lint         PASS
make coverage     PASS
internal/wasm     92.5% (required >= 90%)
make benchmark-regression BENCHMARK_BASE=<origin/preprod worktree>  PASS
nested .benchmark-baseline commands with GOWORK=off                  PASS
nested clone moved to an independent temporary directory              PASS
```

## 5. 预防措施

### 5.1 代码层面

- [x] 删除不再有调用方的私有辅助函数，而不保留“可能供测试使用”的死代码。
- [x] 测试中统一使用 `errors.Is` 断言 sentinel error。

### 5.2 测试层面

- [x] 每个新增公开 Wasm runtime 包装 API 至少覆盖成功、未知 document 与 nil runtime 边界。
- [x] 发布候选推送前执行 staticcheck、golangci、coverage 和基线性能比较。
- [x] 基线 checkout 嵌套在候选目录时，移动至独立目录并明确验证 `GOWORK` 解析边界。

### 5.3 流程层面

- [x] 将 Docker coverage 失败与其底层 `make coverage` 根因分开报告，避免重复归因。

## 6. 经验总结（一句话）

跨模块发布门禁必须在 beta→preprod 之前本地运行一次：主路径通过并不能替代静态分析、错误包装语义和每包覆盖率验证。
