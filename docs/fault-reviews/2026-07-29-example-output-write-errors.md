# 故障复盘：复制示例忽略输出写入失败

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-07-29 |
| 发现人 | Codex 静态检查 |
| 严重程度 | P3-轻微 |
| 影响范围 | `examples/experimental-collaboration` 与 `examples/warehouse-replication` 的命令输出；示例计算成功但输出失败时会错误返回成功 |
| 关联 Issue/PR | 无 |
| 关联提交 | 待合并提交 |

## 1. 问题描述

### 1.1 问题场景

调用方传入的 `io.Writer` 已关闭、磁盘满或网络流中断时，两个示例在完成复制计算后调用 `fmt.Fprintf` 输出结果。

### 1.2 具体表现

原实现忽略 `fmt.Fprintf` 返回的错误，`run` 继续返回 `nil`，调用方无法判断结果是否已经送达。

### 1.3 错误信息

`golangci-lint run` 的 `errcheck` 报告：

```
Error return value of `fmt.Fprintf` is not checked (errcheck)
```

## 3. 根本原因分析

### 3.1 问题分析过程

1. 合并远端示例后，完整静态检查在两个 `fmt.Fprintf` 调用处失败。
2. 两处 `run(io.Writer) error` 均有错误返回通道，但写入操作没有接入该通道。
3. 使用失败 writer 的回归测试确认，未修复版本会错误返回成功。

### 3.2 直接原因

**相关代码位置**：`examples/experimental-collaboration/main.go:57`、`examples/warehouse-replication/main.go:37`。

写入函数的 `(int, error)` 返回值未被检查。

### 3.3 根本原因

- **开发层面**：示例将输出视为展示步骤，遗漏了 `io.Writer` 的失败语义。
- **测试层面**：仅覆盖成功 writer，没有覆盖错误 writer。
- **流程层面**：lint 在本地合并前未对新增示例完整运行。

### 3.4 为什么没有提前发现

成功输出测试不能暴露写入失败；`errcheck` 是首次覆盖这两个新增示例的门槛。

## 4. 解决方案

### 4.1 根本解决方案

两个 `run` 函数检查 `fmt.Fprintf` 的错误并使用 `%w` 返回上下文；对应测试使用返回 `io.ErrClosedPipe` 的 writer 断言错误可被 `errors.Is` 识别。

### 4.2 影响范围评估

正常输出不变；失败输出现在正确返回错误，不改变 CRDT 复制语义或协议。

## 5. 预防措施

### 5.1 代码层面

- [x] 所有示例中的 `io.Writer` 写入都必须处理错误。
- [x] 保持 `golangci-lint` 的 `errcheck` 门槛。

### 5.2 测试层面

- [x] 为两个示例加入失败 writer 回归测试。

## 6. 经验总结（一句话）

只要 API 暴露 `io.Writer` 和 `error` 返回值，示例输出失败也必须向调用方传播，不能把它当作可忽略的展示细节。
