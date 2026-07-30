# 故障复盘：awareness wire 测试将语义错误误判为格式错误

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-07-30 |
| 发现人 | Codex 自动测试 |
| 严重程度 | P3-轻微 |
| 影响范围 | 新增 `awareness` 包的单元测试；未发布、未影响运行时用户 |
| 关联 Issue/PR | 无 |
| 关联提交 | 待创建 |

## 1. 问题描述

### 1.1 问题场景

为 awareness-v1 解码器增加畸形输入测试时，构造了版本、时钟和离线标记均合法、但 actor 长度为零的消息 `01 00 01 00`。

### 1.2 具体表现

`go test ./awareness` 失败：测试期待 `ErrInvalidUpdate`，实际返回 `ErrInvalidActor`。

### 1.3 错误信息

```
--- FAIL: TestUpdateWireRoundTripAndRejectsInvalidInput
    awareness_test.go:101: UnmarshalUpdate(01000100) = awareness: invalid actor, want ErrInvalidUpdate
```

## 3. 根本原因分析

### 3.1 问题分析过程

1. 先确认输入的 varint、非零时钟和 removal 状态都符合 wire 结构。
2. 查看 `UnmarshalUpdate`：它先完成无分配边界解码，再调用 `Normalize` 做 actor 和 JSON 的语义校验。
3. `Normalize` 对空 actor 正确返回 `ErrInvalidActor`；运行时代码无缺陷。
4. 最终定位为测试把“结构非法”和“字段语义非法”混为同一个错误类别。

### 3.2 直接原因

`awareness/awareness_test.go` 将空 actor 样本放在只断言 `ErrInvalidUpdate` 的畸形 wire 表中。

### 3.3 根本原因

- 设计层面：解码 API 有意保留细粒度错误，测试没有按两阶段验证边界组织。
- 开发层面：新增负例时只按字节形状分类，没有逐项核对语义验证路径。
- 流程层面：首轮测试先运行后修正，尚未形成该包的错误分类矩阵。

### 3.4 为什么没有提前发现

- 代码审查前没有逐个运行新增包测试。
- 单测只覆盖了成功 round-trip，负例首次执行才暴露精确错误断言不一致。

## 4. 解决方案

### 4.1 根本解决方案

将空 actor 样本移到独立断言，明确期望 `ErrInvalidActor`；保留 varint、截断和尾随字节样本只断言 `ErrInvalidUpdate`。

**修改文件**：`awareness/awareness_test.go`

**方案说明**：这保持运行时错误分类不变，调用者仍可区分需要断开连接的结构错误与需要拒绝身份字段的语义错误。

### 4.2 影响范围评估

仅收紧测试契约；不改变 wire 格式、存储行为或兼容性。

## 5. 预防措施

### 5.1 代码层面

- [x] 解码器测试按“wire 结构、字段语义、状态冲突”分组。
- [x] 为 actor、clock、JSON object 和资源上限各保留独立负例。

### 5.2 测试层面

- [x] 增加 fuzz 入口，保证任意字节序列不会 panic。
- [x] 在提交前运行包级测试和 race 检测。

## 6. 经验总结（一句话）

> 受限协议的测试必须区分字节结构错误与字段语义错误，否则会掩盖有价值的拒绝原因。
