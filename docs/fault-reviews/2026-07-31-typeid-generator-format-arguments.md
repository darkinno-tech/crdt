# 故障复盘：TypeID 生成器遗漏格式参数导致生成中断

## 基本信息

| 字段 | 内容 |
|---|---|
| 日期 | 2026-07-31 |
| 发现人 | Codex 本地验证 |
| 严重程度 | P2-一般 |
| 影响范围 | TypeID 注册表生成、Go 构建与 CI 生成校验 |
| 关联 Issue/PR | 无 |
| 关联提交 | 待提交 |

## 1. 问题描述

### 1.1 问题场景

为生成的 TypeID 注册表增加 `FrameTypeRegistration.Name` 后，运行 `go generate ./...`。

### 1.2 具体表现

生成器在格式化生成的 Go 源文件时 panic，随后根包无法编译，因为旧生成物中不存在新的 `frameTypeRegistrations` 符号。

### 1.3 错误信息

```text
panic: format generated Go source: 36:99: missing ',' in composite literal
internal/cmd/typeidgen/main.go:141:98: fmt.Fprintf format %s reads arg #3, but call has 2 args
```

## 2. 根本原因分析

### 2.1 分析过程

1. `go generate` 首先报出生成 Go 源无法解析。
2. 随后的 `go test` 和 linter 均指出根包缺少新注册表符号，说明生成步骤未完成。
3. 定位到 `internal/cmd/typeidgen/main.go:141`，模板包含两个 `%s`，但只传入了一个 TypeID 名称参数。

### 2.2 直接原因

`fmt.Fprintf` 为 state 与 delta 两个占位符提供了不足的参数，输出了损坏的组合字面量。

### 2.3 根本原因

新增生成模板后只运行了已有的注册表校验测试；该测试没有调用 `renderGo`，因此没有覆盖格式化后的输出。

### 2.4 为什么没有提前发现

模板变更后的首次 `go generate` 被放在后续验证批次，而不是紧跟代码编辑后立即执行。

## 3. 解决方案

- 为两个 `%s` 都传入 `frame.Name`，恢复合法的 state/delta 常量引用。
- 新增 `TestRenderGoIncludesEveryRegistrationName`，直接执行 `renderGo`；该函数内部使用 `go/format`，因此测试同时验证生成 Go 代码可解析。

影响仅限构建期生成物；不改变 TypeID 数值、帧字节或协议策略。

## 4. 预防措施

- [x] 每次修改生成模板后立即执行 `go generate ./...`。
- [x] 对 Go 模板新增直接渲染测试，而非只测试输入校验。
- [x] 保留 `make generate-check` 和 CI 生成物漂移检查。

## 5. 经验总结

生成器的输入校验不能替代输出可编译性验证；模板修改必须在同一测试周期内运行生成与编译检查。
