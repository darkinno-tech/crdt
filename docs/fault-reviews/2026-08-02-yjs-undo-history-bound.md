# 故障复盘：Yjs 本地 undo history 未设资源上限

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-08-02 |
| 发现人 | Codex 多维代码审计与受控 Yjs 验证 |
| 严重程度 | P2-一般 |
| 影响范围 | `@darkinno/crdt-client/yjs` 的 plain-text `YjsTextUndoManager` 长会话 |
| 关联 Issue/PR | 无 |
| 关联提交 | `a872191`、`13adb26` |

## 1. 问题描述

### 1.1 问题场景

用户在一个已绑定 `Y.Text` 的长时间编辑会话中持续输入、粘贴和分段撤销。binding 已限制
单次 update、cursor 和可见文本长度，并允许通过 `captureTimeout` 合并相邻动作；但每次未
合并的本地编辑仍会向 Yjs `UndoManager` 增加一个 stack item。

### 1.2 具体表现

原实现没有最大 stack item 数。即使 `Y.Text` 没超过 `maxTextUTF16`，本地 undo/redo
history 仍可持续保留删除项和元数据，长期会话没有确定的内存上界。该问题在发布前审计中
发现，尚无线上告警、用户日志或数据修复记录。

### 1.3 错误信息

没有运行时异常。可重复的触发条件是持续产生超过任意预期 UI history 数量的独立本地事务；
旧实现会继续接受它们，而不是返回资源错误或释放历史。

## 2. 临时解决方案（可选）

### 2.1 方案描述

无。没有采用依赖 JavaScript GC、要求调用方周期性 `clear()` 或直接 `splice` Yjs 内部数组
的规避方式。

### 2.2 止血效果

不适用；在 beta 发布前完成根本修复。

### 2.3 临时方案的局限

GC 没有可验证的释放时限；调用方遗漏 `clear()` 仍会无界；直接删除内部 stack item 会绕过
Yjs 对 deleted struct 的 keep/release bookkeeping，可能影响 GC 和之后的撤销语义。

## 3. 根本原因分析

### 3.1 问题分析过程

1. 以 Yjs 为参照逐项核对 browser binding 的 update、state-vector、cursor、observer、undo
   生命周期与资源限制。
2. 确认 `YjsTextUndoManagerOptions` 只有 `captureTimeout`，没有 retention 上限；查看
   `Y.UndoManager` 实现确认它公开维护 `undoStack` 与 `redoStack`。
3. 检查 Yjs 的 `clear()` 路径，确认它会遍历完整 stack 并释放被 undo history 保留的 deleted
   struct；任意数组截断没有等价的公开安全释放 API。
4. 选择在下一个本地替换写入前执行完整、原子化的 local-history reset，再让当前事务按原生
   Yjs 规则入栈。
5. 添加小容量三编辑、undo/redo、后续新编辑和非法上限回归，并运行完整 TypeScript、real
   sidecar/relay 集成和两类性能基准。

### 3.2 直接原因

`YjsTextUndoManager` 创建 Yjs `UndoManager` 时没有最大 history 选项；
`YjsTextBinding.applyLocalReplacement` 也没有在事务前检查 history 容量。

**相关代码位置**：修复后 `clients/typescript/src/yjs.ts:73-81`、
`clients/typescript/src/yjs.ts:384-404`、`clients/typescript/src/yjs.ts:508-573`、
`clients/typescript/src/yjs.ts:1093-1098`。

### 3.3 根本原因

- **设计层面**：初版将 `captureTimeout` 误作资源控制，忽略它只影响相邻事务合并，不限制
  用户显式分段、慢速输入或长会话产生的 item 数。
- **开发层面**：实现重点放在本地 origin 隔离、远端不入栈和补偿性 update，漏审了 Yjs
  history 为保证恢复删除项而持有的资源生命周期。
- **流程层面**：此前测试验证 undo/redo 正确性，却没有在固定小容量下验证 retention、释放
  与下一次本地编辑的行为。

### 3.4 为什么没有提前发现

- **代码审查阶段**：审查清单检查了单条 frame、文本、cursor 与 observer 上限，没有将 undo
  stack 视为单独的 retained-resource 容器。
- **测试阶段**：只覆盖普通 undo/redo 和远端编辑排除，没有模拟多于 UI history 预算的本地
  capture sequence。
- **监控告警**：未以本地 GC 时机或 heap 曲线替代确定性上限，因此也没有已有的线上告警。

## 4. 解决方案

### 4.1 根本解决方案

**修改文件**：`clients/typescript/src/yjs.ts`

新增 `maxStackItems`（默认 256，必须为正安全整数）。每次 binding-owned local replacement
在 mutation 前遍历自身管理的 undo manager；若 undo stack 已到上限，调用原生
`UndoManager.clear()` 同时释放完整 undo/redo history，再按原有 local origin 记录当前
编辑。这样当前编辑可撤销，且不改变远端 update、V1/V2、state vector 或 Yjs wire 格式。

**测试文件**：`clients/typescript/test/yjs.test.mjs`

用 cap=2 验证三次独立编辑后只保留最新 capture、undo/redo 后的新编辑清除过期 redo，且
`maxStackItems: 0` 在构造时失败。

**方案说明**：选择完整 reset 而非“保留最近 N 项”的内部 splice。后者看似保留更多历史，
却不能通过 Yjs 公共 API 对被删除 stack item 的 deleted struct 做对称 release，安全性和
GC 正确性不足。若产品需要跨刷新、可搜索或可审计的历史，必须设计独立持久化方案。

### 4.2 影响范围评估

- 默认用户最多保留 256 个未合并的本地 capture；到达上限后的下一次本地编辑会丢弃旧的
  本地 UI history，但文档内容、远端内容和持久恢复不丢失。
- undo/redo 仍生成新的普通 Yjs update，仍需原有认证、持久回执和重试。
- 不引入 Yjs 与 Go RGA 的转换，不更改 room、store、relay、awareness 或 update 格式。
- 单个 stack item 仍受既有文本/update 上限约束；服务端对不可信 update 的 heap/rate 防护
  仍然必需。

## 5. 预防措施

### 5.1 代码层面

- [x] 为 binding-owned `Y.UndoManager` 增加默认上限与输入校验。
- [x] 仅使用 Yjs 公开完整释放路径，不直接截断内部 stack。
- [ ] 在 retained-resource 审查清单中加入 undo/redo、listener、tombstone、cursor cache 与
  request-scoped engine 对象。

### 5.2 测试层面

- [x] 保留小容量 history reset、undo/redo、redo invalidation 与非法容量回归。
- [x] 保留真实 CodeMirror/JSDOM、V1/V2、state-vector、重排/重复三副本与 sidecar/relay
  集成验证。
- [ ] 在目标浏览器和实际 editor schema 下加入长会话 heap 与 cap-reset 交互测试。

### 5.3 监控层面

- [ ] 产品集成可记录匿名的 history-reset 计数和 UI-level memory pressure，但不得记录文本、
  Yjs update、cursor payload 或认证凭据。

### 5.4 流程/规范层面

- [x] 在 Yjs 集成文档写明 local undo history 的默认容量、reset 行为和非持久边界。
- [ ] 对每个新 retained collection 在设计评审中明确：最大数、单项最大字节、释放时机和
  压力下的 fail/evict 语义。

## 6. 经验总结（一句话）

> `captureTimeout` 只能合并 Yjs 本地操作，不能替代资源上限；历史项一旦参与 deleted struct 的保留，就必须用引擎提供的完整释放路径建立可验证边界。
