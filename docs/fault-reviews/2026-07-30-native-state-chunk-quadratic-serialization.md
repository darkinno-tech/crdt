# 故障复盘：NativeArray 状态分块的二次序列化

## 基本信息

| 字段 | 内容 |
| --- | --- |
| 日期 | 2026-07-30 |
| 发现人 | Codex 开发自测 |
| 严重程度 | P2-一般 |
| 影响范围 | `clients/typescript` 的 `NativeDocument.encodeStateAsUpdates()` 大数组快照导出 |
| 关联 Issue/PR | 无 |
| 关联提交 | 本次 native-ts-v1 交付的性能提交 |

## 1. 问题描述

### 1.1 问题场景

新建纯 TypeScript 共享数组后，状态导出需要将最多 100,000 个 RGA 节点切成不超过 1 MiB
的 canonical JSON 更新。初版在寻找每个分块边界时，都重新序列化“当前分块全部节点 + 新
节点”的候选更新。

### 1.2 具体表现

节点数增长时，同一批已经入选的节点被重复 JSON 序列化，单个大分块的时间复杂度退化为
O(n²)。4,096 节点的状态恢复基准已明显比单次增量操作重；若按同一算法扩展到默认 100,000
节点上限，导出时间会不可接受，并可能在移动浏览器主线程造成长时间卡顿。

### 1.3 错误信息

没有运行时异常或线上告警；问题由 `npm run bench:native` 的状态编码/恢复工作负载和代码
审查发现。它是发布前性能缺陷，不是已发生的数据损坏。

## 2. 临时解决方案

无临时绕过。该功能尚未发布，因此直接修正分块算法。

## 3. 根本原因分析

### 3.1 问题分析过程

1. 先为 append、middle insert、三编辑器乱序合并和状态恢复建立 4,096 节点基准。
2. 检查状态恢复路径，定位到 `chunkArrayEntries()`：循环中构造候选 entries 数组，并对完整
   update 执行 `canonicalJSON()` 以判断是否超过 `maxUpdateBytes`。
3. 该判断在第 k 个节点时再次处理前 k 个节点，分块内总处理量为 1 + 2 + ... + n。
4. 状态输出的保留上限是 100,000 节点，算法复杂度与公开上限不匹配；因此在提交前改为
   增量字节计数。

### 3.2 直接原因

**相关代码位置**：`clients/typescript/src/native.ts` 的 `chunkArrayEntries()` 与
`packOperations()`。

初版等价逻辑是对每个节点都执行：

```ts
const candidate = [...current, entry];
if (encode(canonicalUpdate(candidate)).length > maxUpdateBytes) flush();
```

### 3.3 根本原因

- **设计层面**：先保证了分块输出正确性，但没有把“每次边界判断”的成本纳入 100,000 节点
  资源模型。
- **开发层面**：把 canonical 编码当作常数时间的长度探测，忽略了它会遍历完整嵌套 value。
- **流程层面**：实现初版完成后才加入状态恢复基准，缺少在设计阶段对导出路径进行复杂度审查。

### 3.4 为什么没有提前发现

- 早期单元测试只验证状态可恢复与 byte 上限，没有测量规模增长。
- 原有 TS 基准覆盖 frame 解码和 Wasm RGA，不覆盖 native 状态分块。
- 代码审查清单未明确要求“受 retained-state limit 约束的循环不能重复编码完整前缀”。

## 4. 解决方案

### 4.1 根本解决方案

改为先计算固定 update 前后缀的 UTF-8 字节数，再为每个 entry/operation 仅计算一次
canonical JSON 字节数。每添加一项只加逗号和该项长度；超过上限时提交当前分块并开始新分块。

```ts
const candidateBytes = currentBytes + (current.length === 0 ? 0 : 1) + entryBytes;
if (candidateBytes > maxUpdateBytes) flushCurrentChunk();
```

同一策略也用于多个操作组成的 state update，避免 map/tombstone 数量较大时产生相同问题。

### 4.2 影响范围评估

- 不改变 `native-ts-v1` 的字段、canonical JSON 或 merge 语义。
- 输出仍由 `normalizeUpdate()` 最终验证，因此长度估算错误会在提交前被拒绝，而不会发出超限
  更新。
- 只影响本地 state export 的性能；已有 Go frame、Wasm RGA 和 TypeID 不受影响。

## 5. 预防措施

### 5.1 代码层面

- [x] 所有按 byte limit 分块的循环使用增量长度计数，不重复编码完整前缀。
- [x] state export 的最终 `normalizeUpdate()` 保留为边界校验。

### 5.2 测试层面

- [x] 新增 4,096 节点 state encode/restore 基准。
- [ ] 后续增加 100,000 节点的专用长基准，并在 CI 外定期运行以控制耗时。

### 5.3 监控层面

- [ ] 接入应用时记录 state export 的节点数、update 数、字节数和耗时；超出 UI 帧预算时转
  Worker 或采用增量同步。

### 5.4 流程/规范层面

- [x] 在 native shared-type 架构文档中记录 retained-state 上限与分块复杂度约束。
- [ ] 在 CRDT 性能审查中检查“上限 × 每元素完整序列化”的隐含二次复杂度。

## 6. 经验总结（一句话）

> 对有明确 retained-state 上限的 CRDT 状态导出，分块边界只能增量计数，不能在循环里反复编码完整已选前缀。
