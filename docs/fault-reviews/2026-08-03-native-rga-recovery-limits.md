# 故障复盘：原生 RGA 恢复路径丢失协商资源上限

## 基本信息

| 字段 | 内容 |
| --- | --- |
| 日期 | 2026-08-03 |
| 发现人 | Codex 审计 |
| 严重程度 | P2-一般 |
| 影响范围 | Rust C ABI、Python、Swift、C++ 的 `rga-run-v2` RGA 恢复构造 |
| 关联 Issue/PR | 无 |
| 关联提交 | `cafb3de` (`fix(native): retain RGA limits across recovery`) |

## 1. 问题描述

### 1.1 问题场景

复制组在 manifest 中为 RGA 协商较小的 `max_nodes`、帧、墓碑或 pending-parent 限制。首次创建的 C++ RGA 可传入 `crdt_limits`，但进程持久化 `state + HLC + frontier/outbox` 后只能通过默认上限恢复。Python 和 Swift 则没有公开 RGA 限制入口。

### 1.2 具体表现

恢复副本会回到 `Limits::default()`，从而接受首次运行时会被拒绝的帧或保留状态。该行为不改变 TypeID `19/20`、合并顺序或 HLC 语义，但会扩大单副本内存和待决图的可用预算，违反同一复制组的资源策略。

### 1.3 错误信息

这是发布前静态审计发现的边界缺陷，没有生产告警或用户数据损坏记录。复现策略为：以 `max_nodes=1` 创建并快照一个节点，在恢复副本接收第二个根节点；修复前恢复构造使用默认限制，修复后各绑定均返回资源限制状态码 `3`，且状态和 HLC 不变。

## 2. 临时解决方案（可选）

### 2.1 方案描述

没有采用运行时开关或在包装层手工预检查。临时要求调用方重新创建默认限制会改变已认证 manifest 的含义，不能作为安全方案。

### 2.2 止血效果

不适用。该缺陷在 beta 发布前修复。

### 2.3 临时方案的局限

包装层无法可靠地复刻 Rust 解码、pending-parent 和记账边界；只检查帧字节数也无法覆盖节点、墓碑和待决字节限制。

## 3. 根本原因分析

### 3.1 问题分析过程

1. 对原生绑定的构造 API 与 `LWWMap` 的恢复 API 逐项对照，发现 LWW-Map 已有 `new_from_clock_with_limits`，RGA 只有 `new_with_limits`。
2. 检查 Rust ABI 实现，`crdt_rga_new_from_clock` 在恢复时固定传入 `Limits::default()`。
3. 检查 Python、Swift、C++ 包装层，确认 Python/Swift 无 RGA 限制类型，C++ 虽可用自定义限制创建却无法带限制恢复。
4. 使用一个已快照节点和一个外来根节点模拟重启后的增量接收，确定该问题发生在恢复构造，且应由 Rust 核心而非包装层统一修复。

### 3.2 直接原因

RGA C ABI 缺少带限制的时钟恢复构造；原有恢复函数在 `clients/rust/src/ffi.rs` 中固定使用 `Limits::default()`。因此各语言包装层不能把经过认证的资源策略传入恢复实例。

**相关代码位置**：`clients/rust/src/ffi.rs:226-304`、`clients/python/crdt_rga/__init__.py:141-279`、`clients/swift/Sources/CRDTRGA/CRDTRGA.swift:33-182`、`clients/cpp/include/im10furry/crdt_rga.hpp:63-171`

### 3.3 根本原因

- **设计层面**：首次构造与时钟恢复被视作不对称 API，未将限制视为持久化恢复上下文的一部分。
- **开发层面**：新增 RGA 构造限制时只覆盖了新副本，不像稍后的 LWW-Map 一样覆盖恢复分支。
- **流程层面**：跨语言测试覆盖了同 ID HLC 恢复，但没有以非默认限制重复该场景。

### 3.4 为什么没有提前发现

- **代码审查阶段**：检查重点放在 ABI 缓冲区所有权、帧规范化和 HLC 冲突，没有将 RGA 与 LWW-Map 的恢复构造做成对称性检查。
- **测试阶段**：原有恢复用例均使用默认限制，无法观察恢复后的策略放宽。
- **监控告警**：库不拥有生产传输或容量指标，不能从运行时告警发现 manifest 与副本限制漂移。

## 4. 解决方案

### 4.1 根本解决方案

新增 `crdt_rga_new_from_clock_with_limits`，它与新建的 `crdt_rga_new_with_limits` 使用同一 `crdt_limits` 映射和 Rust `Limits` 校验。Python 暴露 `RGALimits`，Swift 暴露 `RGALimits`，C++ 提供带 `ClockState + crdt_limits` 的 RAII 构造；三者均将无参数构造保留为显式默认限制的兼容路径。

**修改文件**：

- `clients/rust/src/ffi.rs`
- `clients/rust/include/crdt_rga.h`
- `clients/python/crdt_rga/__init__.py`
- `clients/swift/Sources/CRDTRGA/CRDTRGA.swift`
- `clients/cpp/include/im10furry/crdt_rga.hpp`

**验证代码位置**：`clients/python/tests/test_rga.py:55-72`、`clients/swift/Sources/CRDTRGAConformance/main.swift:32-51`、`clients/cpp/tests/conformance.cpp:94-125`

**方案说明**：限制仍由经过认证的 manifest 决定，Rust 核心继续执行预分配/预提交检查。包装层不复制协议判断，因此不会引入各语言不一致的资源计算或部分变更。

### 4.2 影响范围评估

新增 C ABI 符号和可选构造重载，不删除现有符号或更改帧格式。现有默认调用保持默认限制；需要较严格策略的主机可在重启后完整恢复同一策略。新增 Python、Swift、C++ 动态库用例覆盖该路径。

## 5. 预防措施

### 5.1 代码层面

- [x] 将 RGA 首次构造与时钟恢复共用一个 Rust 内部构造辅助函数，避免默认值分叉。
- [x] 要求新增原生资源限制 API 同时评审“首次构造、恢复构造、默认兼容构造”三条路径。

### 5.2 测试层面

- [x] Python、Swift、C++ 在 `max_nodes=1` 下验证恢复后的外来节点原子拒绝和 HLC 不变。
- [x] 保留 Rust、Python、Swift、C++ 的 Go 向量、乱序/重复和快照恢复门禁。
- [x] 运行 Rust 与 C++ 受控插入、同步、快照恢复基准；结果只作为回归信号，不宣称生产容量。

### 5.3 监控层面

- [ ] 宿主应用在生产侧记录已认证 manifest 的限制版本与资源拒绝计数；库本身不收集用户文档内容或遥测。

### 5.4 流程/规范层面

- [x] 在原生多语言设计文档中明确 `Limits` 是 manifest 数据，必须跨重启复用。
- [x] beta 的 native CI 继续执行 Python、C++、Swift 动态链接测试，禁止仅以 Rust 单测宣称绑定覆盖。

## 6. 经验总结（一句话）

> CRDT 的资源上限属于复制组状态的一部分：首次构造能限制、恢复构造却回退默认值，等同于重启后悄然放宽安全边界。
