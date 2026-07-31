# 故障复盘：富文本 Wasm 初始化默认拒绝 legacy RGA v1 artifact

## 基本信息

| 字段 | 内容 |
| --- | --- |
| 日期 | 2026-07-31 |
| 发现人 | Codex beta 兼容矩阵 |
| 严重程度 | P2-一般 |
| 影响范围 | 使用 `make wasm-v1-test` 构建的 combined Go/Wasm artifact 的 rich-text 初始化 |
| 关联 Issue/PR | 无 |
| 关联提交 | 待本次 beta 分点提交 |

## 1. 问题描述

`initRichTextWasm()` 为启动共享 Go/Wasm 运行时而间接调用 `initRGAWasm()`；后者默认验证
RGA run-v2。于是一个 RGA v1 + rich-text v1 的合法 combined artifact 在 rich-text 运行时读取
前就报 `CRDTRuntimeError: protocol_mismatch`，使 v1 兼容矩阵无法覆盖 rich-text 绑定。

## 2. 根本原因

- rich-text 的 Manifest 期望与同一 artifact 内 RGA 的编译协议被混为一个默认值。
- 测试只在默认 run-v2 artifact 初始化 rich-text，未执行 v1 matrix。

## 3. 解决方案

`InitRichTextWasmOptions` 增加可选 `expectedRGAProtocol`。默认仍严格要求 run-v2；明确加载
legacy artifact 的调用方必须传入其认证 Manifest 选择的 RGA v1 期望。rich-text v1 的
TypeID、语义版本和独立 Manifest 边界不变。

## 4. 预防措施

- [x] `make wasm-v1-test` 以 artifact 实际协议初始化 rich-text 并验证完整绑定测试。
- [x] 文档说明只有经认证的 legacy artifact 才传入 `expectedRGAProtocol`。
- [ ] 新增 combined Wasm surface 时，将每个独立协议放入 run-v2 与 legacy matrix。

## 5. 经验总结

共享二进制的启动协议与独立 CRDT surface 的 wire contract 都必须显式验证，不能让其中一个的
默认值替代另一个。
