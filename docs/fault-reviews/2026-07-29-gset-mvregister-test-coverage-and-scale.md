# 故障复盘：G-Set 与 MV-Register 首轮测试未覆盖协议边界和规模路径

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-07-29 |
| 发现人 | Codex 自动验证 |
| 严重程度 | P3-轻微 |
| 影响范围 | `set.GSet` 与 `register.MVRegister` 的新增协议交付；未发布、无线上影响 |
| 关联 Issue/PR | 无 |
| 关联提交 | 未提交 |

## 1. 问题描述

### 1.1 问题场景

新增 G-Set 和 MV-Register 的状态/Delta frame、快照、并发测试及基础基准后，执行仓库
90% 包覆盖率门槛与多规模性能验证。

### 1.2 具体表现

普通收敛、并发、race 和 fuzz 测试均通过，但首轮包覆盖率为：

```text
github.com/DarkInno/crdt/set       87.0%
github.com/DarkInno/crdt/register  84.2%
```

此外，MV-Register 原有基准连续在两个副本内本地写入；由于本地写会覆盖已观测值，基准只
保留一个可见值，不能代表版本向量和大量并发值的实际成本。

### 1.3 错误信息

```text
coverage: github.com/DarkInno/crdt/set is 87.0%, below required 90%
coverage: github.com/DarkInno/crdt/register is 84.2%, below required 90%
```

## 2. 临时解决方案

无。问题由本地质量门禁拦截，未发布任何产物。

## 3. 根本原因分析

### 3.1 问题分析过程

1. 先运行了收敛、Wire round-trip、并发和短 fuzz，确认主语义可用。
2. 执行包级覆盖率后发现 `set` 和 `register` 未达仓库的 90% 门槛。
3. coverage profile 显示遗漏集中在 nil、codec/类型不匹配、限制、非法快照、同 dot 冲突、
   非规范因果上下文和溢出路径。
4. 审阅性能 fixture 后发现 MV-Register 的 128 次本地写没有构造 128 个并发值，不能验证
   `context + visible values` 的线性成本。
5. 根因是首轮测试按功能流组织，而未按公开 API 的成功/失败/原子性矩阵和数据规模模型组织。

### 3.2 直接原因

- `set/gset_test.go` 与 `register/mvregister_test.go` 缺少独立 golden vector、受限编解码、
  nil 接收者、恢复失败和接收端不变性用例。
- `register/mvregister_benchmark_test.go` 缺少由不同副本各自写入后合并产生的并发值 fixture。

**相关代码位置**：`set/gset.go:51-340`、`register/mvregister.go:73-526`、
`set/gset_benchmark_test.go`、`register/mvregister_benchmark_test.go`。

### 3.3 根本原因

- **设计层面**：将 CRDT 的代数收敛测试误当作完整协议验证，未单独列出 frame/snapshot 的
  原子错误路径。
- **开发层面**：基准 fixture 依据“写入次数”构造，而不是依据 MV-Register 的“并发可见值
  数”和版本向量维度构造。
- **流程层面**：覆盖率与规模基准未在首轮测试完成后立即执行。

### 3.4 为什么没有提前发现

- 代码审查阶段：重点检查了因果合并公式，未逐项核对公开 API 的失败分支。
- 测试阶段：已有测试覆盖了重复投递、乱序合并和 race，但未覆盖所有解码限制和非法恢复。
- 监控告警：这是库开发期问题；覆盖率门禁和基准输出是有效拦截机制。

## 4. 解决方案

### 4.1 根本解决方案

新增 `set/gset_additional_test.go` 和 `register/mvregister_additional_test.go`，覆盖：

- 独立构造的 golden Delta frame；
- codec/type 不匹配、尾随字节、限制拒绝、非规范因果 dot 和溢出；
- 同 dot 不同值冲突、快照恢复失败、nil API 和接收端状态不变；
- MV-Register Delta 的重复投递快路径与内部非法状态拒绝。

新增多规模基准：G-Set 为 16/256/4096 元素；MV-Register 为 16/256/1024 个不同副本的
并发值，并分别测量 merge、重复 Delta 和编码。

修复后：

```text
github.com/DarkInno/crdt/set       90.0%
github.com/DarkInno/crdt/register  92.7%
```

### 4.2 影响范围评估

仅新增测试、基准和复盘文档；不改变 G-Set、MV-Register 的公开语义或 wire 格式。

## 5. 预防措施

### 5.1 代码层面

- [x] 每个新 frame 类型均有独立 golden vector，不能只用同一编码器生成后再解码。
- [x] MV-Register 基准 fixture 以并发副本数，而不是单副本写入次数定义规模。

### 5.2 测试层面

- [x] 状态、Delta、快照 API 均覆盖成功、nil、类型错误、限额错误和接收端原子性。
- [x] G-Set 与 MV-Register decoder fuzz 已加入 `make fuzz`。
- [x] 交付前运行包级 90% 覆盖率和多规模基准。

### 5.3 流程/规范层面

- [x] 将“协议级边界矩阵 + 真实状态规模”作为新增 CRDT 类型的验收项。

## 6. 经验总结（一句话）

> CRDT 的收敛证明只覆盖一半交付：frame/snapshot 的原子失败路径与按真实并发状态构造的规模基准，必须同时成为质量门槛。
