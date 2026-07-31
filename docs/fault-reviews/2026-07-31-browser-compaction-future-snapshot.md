# 故障复盘：浏览器追加日志压缩可能提前持久化后续内存更新

## 基本信息

| 字段 | 内容 |
| --- | --- |
| 日期 | 2026-07-31 |
| 发现人 | Codex 静态时序审计与定向测试 |
| 严重程度 | P2-一般 |
| 影响范围 | `native-ts-v1` 浏览器 IndexedDB 追加日志的连续远端/本地更新与压缩路径 |
| 关联 Issue/PR | 无 |
| 关联提交 | 待本次 beta 分点提交 |

## 1. 问题描述

### 1.1 问题场景

浏览器在同一 JavaScript 调用栈内连续接收两条更新。第一条更新达到压缩阈值后进入异步
IndexedDB 队列；第二条已经进入内存 CRDT，但自己的 `append()` 尚未执行。如果第一条队列任务
或其稍后的回执处理调用“当前 document 的 snapshot”，该 snapshot 会包含第二条更新。

### 1.2 具体表现

若第二条 `append()` 因配额、事务或 I/O 错误失败，压缩后的 base 已经包含第二条更新，但该条
更新没有自己的追加记录和本地 outbox 语义。重启可见状态与可重发记录不再是同一个前缀，破坏
了“每个可恢复状态变化都有成功追加边界”的离线优先不变量。

### 1.3 错误信息

该问题在内存存储的定向故障注入测试中复现为第二次 append 的受控错误：

```text
NativeBrowserError: persistence_failed
```

修复前，该错误发生后 compacted snapshot 会错误包含 `title=second`；修复后它仅包含已经成功
追加的 `title=first`。

## 2. 根本原因分析

### 2.1 问题分析过程

1. 审计新增 Go/Wasm RGA 浏览器 facade 时，发现压缩必须使用“该更新发生时”的快照，不能在
   异步队列或回执处理中读取可被后续同步事件改变的 document。
2. 对比已有 native-ts 实现，发现它在 `recordUpdate()` 的队列任务里直接执行
   `this.document.snapshot()`。
3. 构造两条连续远端 Map 更新：第一条触发压缩，第二条在第一条任务运行前改变内存状态；注入
   第二次 append 失败。
4. 复现表明第一条 compact 使用了第二条之后的内存快照，确认是异步任务与可变对象读取之间的
   时序缺口，而不是解码、HLC 或 IndexedDB 原子事务问题。

### 2.2 直接原因

`clients/typescript/src/browser.ts` 的旧压缩逻辑在已排队的异步任务中读取当前 document snapshot，
没有绑定本次更新对应的状态前缀。

### 2.3 根本原因

- **设计层面**：追加日志的“记录顺序”与内存 document 的“当前状态”没有被当作两个不同时间点。
- **开发层面**：原有测试验证了压缩、恢复和失败，但没有把“后续同步更新已入内存、其 append
  失败”与前一条压缩交错。
- **流程层面**：离线持久化审查清单没有明确要求每个压缩 snapshot 绑定已成功追加的状态前缀。

### 2.4 为什么没有提前发现

- 既有 happy-path 测试会让全部 append 成功，包含未来更新的快照会被后续日志重复应用为幂等
  no-op，因而表面上仍可恢复。
- 常规 storage failure 测试只检查报错和 outbox，不检查失败帧是否已经泄漏进先前 compacted base。

## 3. 解决方案

### 3.1 根本解决方案

`recordUpdate()` 在更新事件发生后立即尝试捕获完整快照，并把这个不可变副本传入同一条 append
队列任务。压缩只使用该副本；若数组父节点未解析而不能形成完整快照，继续保留日志而不压缩。
回执后压缩会先等待已有持久化队列稳定、确认没有写入错误且没有新增 mutation，再将读取和压缩
排在之后 mutation 之前；因此它也只能使用与当时持久化前缀一致的快照。

同一规则已用于新 Go/Wasm RGA 浏览器 facade，避免两个浏览器持久化实现出现不同的前缀一致性
语义。

### 3.2 影响范围评估

- 不改变 `native-ts-v1`、Go RGA 或 manifest/wire 格式。
- 不改变 IndexedDB schema 中已有 native 记录。
- 正常路径只在每次 update 事件额外生成一次候选快照；压缩仍由原有阈值和 outbox receipt 控制。

## 4. 预防措施

### 4.1 代码层面

- [x] 所有 append-log compaction 都接受事件时捕获的 immutable snapshot，禁止在异步队列内读取
  可变 document 的当前状态。
- [x] 未完成依赖的状态只能保留追加日志，不能生成部分 base。

### 4.2 测试层面

- [x] 新增“第一条压缩、第二条 append 失败”及“第一条 receipt、第二条 append 失败”的确定性故障注入测试。
- [x] Go/Wasm facade 覆盖 durable outbox、receipt-gated compact、三副本乱序重复投递和重启恢复。

### 4.3 流程/规范层面

- [x] 在浏览器 Go/Wasm RGA 设计文档中明确 snapshot 必须绑定已追加状态前缀。
- [ ] 后续所有持久化 facade 增加“future in-memory state cannot enter earlier compacted base”的审查项。

## 5. 经验总结（一句话）

> 追加日志压缩必须使用事件发生时捕获的不可变状态前缀；在异步队列中读取当前可变状态，会让尚未成功追加的未来更新泄漏进恢复基线。
