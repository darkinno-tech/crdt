# 故障复盘：测试中比较不可比较的 Checkpoint 值

## 基本信息

| 字段 | 内容 |
|---|---|
| 日期 | 2026-07-31 |
| 发现人 | Codex 本地编译检查 |
| 严重程度 | P3-轻微 |
| 影响范围 | FileStore 边界测试编译 |
| 关联 Issue/PR | 无 |
| 关联提交 | 待提交 |

## 1. 问题描述

为验证缺失记录的 `Load` 返回零值，测试使用 `checkpoint != (Checkpoint{})`。编译器拒绝该比较，因为 `Checkpoint` 的嵌入快照包含不可比较字段。

```text
invalid operation: checkpoint != (Checkpoint{}) (struct containing snapshot.Snapshot cannot be compared)
```

## 2. 根本原因

测试作者将“语义上的零检查”误写为 Go 结构体相等比较，没有先确认所有嵌套字段均可比较。

## 3. 解决方案

改为断言缺失记录的 `found == false`、`Cursor == 0` 与空 `Outbox`；这些字段足以验证 API 合约，且不依赖快照内部表示。

## 4. 预防措施

- 对含切片、映射、函数或嵌入外部状态的结构体，避免直接与零值比较。
- 优先断言公开 API 合约中的可观察字段，而非内部结构相等。
- 新增测试后立即运行目标包编译与覆盖率检查。
