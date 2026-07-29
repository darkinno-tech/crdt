# 故障复盘：CI 模糊测试清单未同步新增 G-Set 目标

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-07-29 |
| 发现人 | Codex 自动验证 |
| 严重程度 | P1-严重 |
| 影响范围 | `beta` 分支的 Test 工作流；阻断预发布标签和后续合并 |
| 关联 Issue/PR | 无 |
| 关联提交 | `42c6016`、`6999910` |

## 1. 问题描述

### 1.1 问题场景

G-Set 新增 `FuzzGSetUnmarshalBinary` 后，`set` 包同时包含它和既有的
`FuzzORSetUnmarshalBinary`。本地 Makefile 和 GitHub Actions 都保留了泛匹配的旧命令，
且两份清单各自维护。

### 1.2 具体表现

`beta` 的两个推送工作流（`42c6016` 和 `6999910`）都在 `Fuzz decoders` 步骤失败，造成
`prerelease-tag` 不能运行。

### 1.3 错误信息

```text
testing: will not fuzz, -fuzz matches more than one fuzz test:
[FuzzGSetUnmarshalBinary FuzzORSetUnmarshalBinary]
```

## 2. 根本原因分析

### 2.1 问题分析过程

1. 查询失败运行 `30385158853` 和 `30385605562`，其余 test、coverage、staticcheck、
   golangci 和 docker 检查均成功。
2. 两个运行都在 `./set` 的同一条 `-fuzz=Fuzz` 命令失败，说明不是提交 `6999910` 的
   JSON 诊断逻辑造成的回归。
3. 对比 `Makefile` 与 `.github/workflows/test.yml`，两者都对 `./set` 使用
   `-fuzz=Fuzz`；Makefile 虽已增加 MV-Register 和 replica 目标，但没有把 set 的
   模糊选择器拆开。
4. 最终定位为泛匹配不再满足 Go 的单目标 fuzz 规则，且同一测试清单在本地和 CI 各维护一份。

### 2.2 直接原因

`.github/workflows/test.yml` 的 `Fuzz decoders` 使用：

```sh
go test -run=^$ -fuzz=Fuzz -fuzztime=10s ./set
```

Go fuzz 一次只能选择一个 fuzz target；该正则现在匹配两个测试函数。

### 2.3 根本原因

- **设计层面**：CI 没有复用 Makefile 这一唯一的 fuzz 清单来源。
- **开发层面**：新增 G-Set fuzz 测试时，未将已有的泛匹配替换成两个精确选择器。
- **流程层面**：新增 fuzz target 的验收清单没有要求本地与 CI 命令逐项一致。

### 2.4 为什么没有提前发现

普通单元、race、vet、coverage 和 lint 都不执行该 CI 内联 fuzz 列表；而提交前也未触发或
等待一次远程 beta 工作流，因此差异直到推送后才暴露。

## 3. 解决方案

### 3.1 根本解决方案

将 `Fuzz decoders` 步骤改为 `make fuzz`，并将 Makefile 的 `./set` 命令拆为
`FuzzGSetUnmarshalBinary` 与 `FuzzORSetUnmarshalBinary` 两个精确选择器。Makefile 同时
覆盖 MV-Register 和 replica 的解码/输入边界。

### 3.2 影响范围评估

不改变库 API、wire 格式或运行时语义。CI 会多执行 Makefile 中已有的 MV-Register 与 replica
fuzz，因此运行时间略增，但本地 `make verify` 与远程检查将一致。

## 4. 预防措施

### 4.1 代码与 CI

- [x] CI 通过 `make fuzz` 复用单一 fuzz 清单，不再维护副本。
- [x] 对含多个 fuzz target 的包使用精确 `-fuzz` 选择器。

### 4.2 测试与流程

- [x] 修复后运行 `make fuzz`、完整单元测试、race 和 vet。
- [x] 已在 beta Test 运行 `30386948856` 中确认远程 CI 使用同一 fuzz 清单并通过。
- [x] 将“新增 fuzz target 后检查 Makefile 与工作流是否共用入口”加入本次变更验收。

## 5. 经验总结（一句话）

> 模糊测试目标增加时，CI 必须复用唯一的测试清单；重复的内联命令会在最需要防护的边界测试上发生静默漂移。
