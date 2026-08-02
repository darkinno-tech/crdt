# 故障复盘：awareness 快路径测试遗漏 bytes 导入

## 基本信息

| 字段 | 内容 |
|---|---|
| 日期 | 2026-08-02 |
| 发现人 | Codex |
| 严重程度 | P3-轻微 |
| 影响范围 | 未提交的 awareness 单元测试；生产二进制未构建或发布 |
| 关联 Issue/PR | 无 |
| 关联提交 | 待本轮验证后提交 |

## 1. 问题描述

### 1.1 问题场景

为 `Store.Set` 的同 canonical state 心跳快路径补充 TTL 恢复和返回副本断言时，测试使用了
`bytes.Equal`，但 `awareness/awareness_test.go` 的 import 列表未加入标准库 `bytes`。

### 1.2 具体表现

首次运行针对 awareness 的测试/基准命令在编译阶段停止；性能数字、竞态和 fuzz 结论均未被
当作该改动的验证证据。

### 1.3 错误信息

```text
awareness/awareness_test.go:139:39: undefined: bytes
awareness/awareness_test.go:143:61: undefined: bytes
```

## 2. 临时解决方案

补齐 `bytes` 导入并从定向单元测试重新开始验证。没有运行生产代码、没有状态迁移，也没有临时
绕过测试或编译检查。

## 3. 根本原因分析

1. CPU profile 表明 unchanged `Set` 的 JSON canonicalization 是主要分配来源。
2. 快路径与语义测试一并加入，测试中使用 `bytes.Equal` 比较状态副本。
3. 首次 Go 编译准确定位到缺失 import，因此在任何基准或提交前中止。
4. 根因是增补测试后没有先执行最小编译门禁，遗漏了测试文件的标准库依赖。

直接原因是 `awareness/awareness_test.go` 缺少 `bytes` import。根本原因是把定向测试与基准
合并为同一次命令执行，未在补丁后先运行轻量编译/单元门禁。

## 4. 解决方案

在 `awareness/awareness_test.go` import 块加入 `bytes`，保留状态比较断言。该修复只影响测试
编译，不改变 Store、协议、资源限制或对外 API。

## 5. 预防措施

- [x] 将本次编译失败作为无效验证，不复用失败命令后的任何性能结论。
- [x] 修复后先执行目标测试，再执行 benchmark、race 与 fuzz。
- [ ] 每次新增测试断言后，先运行对应包的最小 `go test -run` 编译门禁。
- [ ] 提交前持续保留 `gofmt` 与 `git diff --check` 检查。

## 6. 经验总结

> 性能实验的每个新断言都必须先通过最小编译门禁；未编译的测试不能作为任何优化结论的依据。
