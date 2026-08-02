# 故障复盘：解码器签名变更遗漏 packed-v3 测试调用点

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-08-02 |
| 发现人 | Codex 本地编译检查 |
| 严重程度 | P3-轻微 |
| 影响范围 | 本次性能分支的 `text` 测试编译；未进入 beta，未影响已发布协议或运行时 |
| 关联 Issue/PR | 无 |
| 关联提交 | 本次优化提交待生成 |

## 1. 问题描述

### 1.1 问题场景

为复用 run-v2 与 packed-v3 解码器已经完成的规范排序，内部函数 `unmarshalRGARun` 和 `unmarshalRGAPacked` 新增了可选的 `canonicalNodeIDs` 输出参数。生产解码路径和部分测试已更新，但 packed-v3 的覆盖与跨语言向量辅助调用仍使用旧的四参数签名。

### 1.2 具体表现

在修改后立即执行仅编译检查时，`text` 包无法构建；这阻断了后续单元、竞态和性能验证。

### 1.3 错误信息

```text
text/rga_cross_language_vectors_test.go:118:110: not enough arguments in call to unmarshalRGAPacked
text/wire_packed_test.go:530:105: not enough arguments in call to unmarshalRGAPacked
```

## 2. 根本原因分析

### 2.1 问题分析过程

1. 完成解码器缓存改动后先运行 `go test ./text -run '^$'` 进行快速编译检查。
2. 编译器把错误限定在 packed-v3 覆盖测试和跨语言向量测试；生产包未出现类型错误。
3. 用 `rg 'unmarshalRGARun|unmarshalRGAPacked' text --glob '*.go'` 枚举所有内部调用点，确认有五个 packed-v3 测试调用遗漏。
4. 所有状态解码和负例测试调用显式传入 `nil`，只有 `Unmarshal*DeltaWithLimits` 传入缓存输出地址。
5. 随后 `go test ./text`、`go test ./richtext` 和 `go test ./replica` 均通过。

### 2.2 直接原因

`unmarshalRGAPacked` 的参数列表从四项扩展为五项后，以下测试文件没有同时更新：

- `text/wire_packed_test.go:530,539,562,577`
- `text/rga_cross_language_vectors_test.go:118`

### 2.3 根本原因

- **设计层面**：解码共享函数同时被生产代码、向量测试和覆盖门使用，签名演进没有集中调用入口。
- **开发层面**：初次补丁根据局部上下文更新了 run-v2 调用点，未在落补丁前完整检索 packed-v3 的内部调用者。
- **流程层面**：快速编译检查在补丁之后才执行，未在签名变更的同一步加入全量调用点枚举。

### 2.4 为什么没有提前发现

- 代码审查阶段：此变更尚未形成提交，编译器检查先于提交发现问题。
- 测试阶段：此前尚未开始单元测试；`go test -run '^$'` 正确阻止了错误继续流入更慢的测试矩阵。
- 监控告警：这是本地开发期编译错误，不应依赖线上监控发现。

## 3. 解决方案

### 3.1 根本解决方案

所有不需要返回排序提示的内部调用显式传入 `nil`，而 run-v2/packed-v3 delta 解码入口传入私有缓存地址。这样状态恢复、负例、覆盖和跨语言向量仍走同一规范校验路径，不会放宽任何帧校验或限额。

### 3.2 影响范围评估

- 仅涉及未导出的函数调用与测试辅助代码。
- 不修改 wire payload、TypeID、Manifest、资源上限或公开 API。
- 修复后跨语言规范向量继续逐字节比对，错误帧仍必须被拒绝。

## 4. 预防措施

### 4.1 代码层面

- [x] 内部解码器的可选优化输出显式使用 `nil`，使非 delta 调用意图清晰。
- [x] 缓存只在完整规范字节校验成功后写入，并在 `ApplyDelta` 中重新检查排序和成员资格。

### 4.2 测试层面

- [x] 签名变更后先执行 `go test ./text -run '^$'`。
- [x] 执行调用点检索并重跑 `text`、`richtext` 和 `replica` 包测试。

### 4.3 流程/规范层面

- [x] 对未导出但跨文件复用的函数改签名时，先用 `rg` 枚举全部调用点，再运行最小编译门。

## 5. 经验总结（一句话）

> 内部函数即使不属于公开 API，签名变更也必须先枚举所有生产与测试调用点；快速编译门应位于更慢验证之前。
