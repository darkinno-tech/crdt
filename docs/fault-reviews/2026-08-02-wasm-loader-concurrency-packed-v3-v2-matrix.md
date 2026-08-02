# 故障复盘：并发 Wasm 启动测试遗漏 packed-v3-v2 协商工件

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-08-02 |
| 发现人 | Codex 回归验证 |
| 严重程度 | P2-一般 |
| 影响范围 | `wasm-packed-v2-test` 的 TypeScript 真实 Go/Wasm 并发启动回归；不影响已发布工件的运行时协议校验 |
| 关联 Issue/PR | 无 |
| 关联提交 | 本次修复提交待生成；相关并发用例 `ef5f403`，新工件提交 `fed82ac` |

## 1. 问题描述

### 1.1 问题场景

在 `origin/beta` 的并发 Wasm loader 回归用例加入后，构建
`WASM_RGA_PROTOCOL=packed-v3-v2` 并执行 `make wasm-packed-v2-test`。该路径会把环境变量映射为
TypeScript loader 的精确 Manifest 期望，再发起 24 个并发启动调用。

### 1.2 具体表现

模块尚未进入测试主体便在夹具初始化阶段退出，因而无法验证新工件在“只下载一次、每个调用仍严格验协议”的并发启动条件下的行为。其余 89 个兼容测试通过，但整个命令以失败结束。

### 1.3 错误信息

```text
Error: CRDT_RGA_PROTOCOL must be run-v2, packed-v3, or v1
    at protocolForArtifact
    (clients/typescript/test/wasm-loader-concurrency.test.mjs:52:9)
```

## 2. 临时解决方案

未采用跳过 `wasm-loader-concurrency.test.mjs` 或把环境变量伪装为普通 packed-v3 的方案。两者都会移除对 outer frame v2 Manifest 的精确验证，不能作为安全的发布条件。

## 3. 根本原因分析

### 3.1 问题分析过程

1. 在变基后的最终真实 Go/Wasm 验证中，`make wasm-packed-v2-test` 出现 89 通过、1 失败。
2. 错误发生在测试文件导入阶段，排除 RGA 编解码、Wasm 构建和并发 loader 本身失败的可能。
3. 检查 Makefile 确认 `wasm-packed-v2-test` 正确传入 `CRDT_RGA_PROTOCOL=packed-v3-v2`。
4. 检查其他 Wasm 集成测试，已发现它们把 `packed-v3-v2` 映射到 `RGA_PROTOCOL_PACKED_V3_V2`。
5. 定位并发启动夹具的 `protocolForArtifact` 仍是旧的三分支枚举，遗漏了新 artifact，故在启动前 fail-fast。

### 3.2 直接原因

`protocolForArtifact` 未导入和返回 `RGA_PROTOCOL_PACKED_V3_V2`。

**相关代码位置**：`clients/typescript/test/wasm-loader-concurrency.test.mjs:8-15,49-55`（修复后）。

### 3.3 根本原因

- **设计层面**：协议工件字符串在多个测试夹具中各自枚举，没有一个共享的 artifact-to-Manifest 映射。
- **开发层面**：新增 `packed-v3-v2` 时更新了主集成和浏览器测试，却遗漏了变基带来的并发启动测试夹具。
- **流程层面**：新增构建变体后虽执行了其兼容命令，但没有先检索所有 `CRDT_RGA_PROTOCOL` 消费点并以矩阵方式逐项登记。

### 3.4 为什么没有提前发现

- **代码审查阶段**：原实现提交早于并发启动测试进入 `beta`，两个提交的测试矩阵未重新合并审视。
- **测试阶段**：首次 `wasm-packed-v2-test` 在变基前的测试列表不包含这个新用例；变基后的重跑才暴露缺口。
- **监控告警**：这是构建时 fail-fast 的本地回归，不应依赖线上监控发现。

## 4. 解决方案

### 4.1 根本解决方案

**修改文件**：`clients/typescript/test/wasm-loader-concurrency.test.mjs`

新增 `RGA_PROTOCOL_PACKED_V3_V2` 导入，并在 `protocolForArtifact` 中对
`"packed-v3-v2"` 返回该常量；未知值仍保留 fail-fast。并发测试随后会以 v2 的完整 Manifest
启动 24 个调用，确认共享下载和对 run-v2 的 `protocol_mismatch` 拒绝均未被放宽。

**方案说明**：精确登记而不是回退到普通 packed-v3，保持 outer frame version 是协议身份一部分这一不变量，也继续让未知构建变量在测试启动前失败。

### 4.2 影响范围评估

- 仅修正测试夹具的工件映射，不改 Go 编码、TypeID、解压上限或生产 loader 行为。
- v1、run-v2、普通 packed-v3 保持原映射；新 v2 工件得到与其它工件同等的并发启动覆盖。
- 不兼容的期望仍必须由 `initRGAWasm` 返回 `protocol_mismatch`，不存在静默降级。

## 5. 预防措施

### 5.1 代码层面

- [x] 新增协议工件时，所有测试夹具均使用对应的公开 Manifest 常量，不以相邻工件替代。
- [ ] 后续将 artifact 字符串到协议常量的映射收敛为测试可复用的单一 helper，减少散落枚举。

### 5.2 测试层面

- [x] 在变基后重跑 `make wasm-packed-v2-test`，覆盖真实 Go/Wasm、24 并发启动与协议拒绝。
- [ ] 每新增 `WASM_RGA_PROTOCOL` 值时，在 CI 中显式执行该值对应的 Wasm 兼容矩阵。

### 5.3 监控层面

- [x] 保持未知 `CRDT_RGA_PROTOCOL` 的 fail-fast 错误，避免将错误 artifact 静默带入运行时。

### 5.4 流程/规范层面

- [x] 将“检索所有环境变量消费者”加入协议工件变更的变基后检查。
- [ ] 评审新 TypeScript/Wasm 协议变体时，逐一核对 Makefile、loader、集成测试、浏览器测试和并发启动测试。

## 6. 经验总结（一句话）

> 协议工件的 outer frame 版本属于 Manifest 身份；每个构建、测试和启动入口都必须显式登记它，不能把新工件默认为相邻协议。
