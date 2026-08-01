# 故障复盘：CodeMirror 本地编辑全量投影导致大文档延迟

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-08-01 |
| 发现人 | Codex 性能审查 |
| 严重程度 | P2-一般 |
| 影响范围 | `@darkinno/crdt-client` 的 CodeMirror 6 纯文本 RGA 绑定；大文档上的本地单区间编辑 |
| 关联 Issue/PR | 无 |
| 关联提交 | `d5e34d4` |

## 1. 问题描述

### 1.1 问题场景

CodeMirror 6 已经提供一笔事务的变更区间，但 `applyViewUpdate()` 只把
`docChanged` 转交给全文读取路径。每次输入都会把旧/新全文转为 rune 数组，
扫描共同前后缀，再用一个 RGA 替换表达结果。

### 1.2 具体表现

局部替换的工作量随全文长度增长。受控 262,144-rune 模拟中，512 次单字符
本地替换的中位耗时为 3.69 ms/编辑；真实 Go/Wasm 12,288-rune 路径为
1.19 ms/编辑。远端投影不属于本次问题范围。

### 1.3 错误信息

没有异常或告警；这是 CPU、短期分配和交互延迟问题。基准通过文档与帧数
一致性检查，确认差异来自绑定层全量比较而不是 RGA 收敛失败。

## 2. 临时解决方案

无。没有通过放宽 RGA frame/文本上限、拆分删除与插入、或跳过一致性检查来
换取吞吐，因为这些做法会破坏原子性或资源边界。

## 3. 根本原因分析

### 3.1 问题分析过程

1. 检查 CodeMirror 绑定入口，确认 `applyViewUpdate()` 仅依赖 `docChanged`：
   `clients/typescript/src/bindings.ts:420-430`。
2. 沿监听器进入通用文本路径，确认它读取全文并在
   `Array.from(previousText)`/`Array.from(next)` 上做前后缀扫描：
   `clients/typescript/src/bindings.ts:185-212`。
3. 对照 CodeMirror 真实测试和结构类型，确认更新对象可提供
   `changes.iterChanges` 的旧/新 UTF-16 坐标：
   `clients/typescript/src/bindings.ts:360-376`。
4. 验证 RGA 的 `replace` 已经能在一个 frame 内原子预检插入和 tombstone，
   因而不需要改变 wire 协议。
5. 确认多区间原生事务不能安全地拆成多帧；若没有原子多替换 wire 操作，必须
   保留现有单帧全文回退。

### 3.2 直接原因

CodeMirror 适配器丢弃了编辑器原生的变更描述，通用路径因此只能通过全文差异
重建局部替换。

**相关代码位置**：修复前 `clients/typescript/src/bindings.ts:369-374`；修复后
`clients/typescript/src/bindings.ts:420-430`。

### 3.3 根本原因

- **设计层面**：最初的最小编辑器端口只有“全文已变”的观察接口，没有把
  原生事务作为可选能力建模。
- **开发层面**：实现优先保证 Unicode、原子替换和远端 no-echo，未将大文档
  局部输入纳入热路径设计。
- **流程层面**：既有基准没有在相同脚本中并排运行原生增量与全文回退，性能
  回归难以直观看出。

### 3.4 为什么没有提前发现

- 代码审查阶段着重检查协议/原子性，没有要求编辑器变化来源进入适配器。
- 测试覆盖了结果收敛和 Unicode，但没有断言单区间更新不读取全文。
- 基准曾只输出单一路径，无法隔离绑定层的全文比较成本。

## 4. 解决方案

### 4.1 根本解决方案

新增可选 `CodeMirrorChangeSet` 和 `EditorUTF16Replacement`。恰好一段、坐标
自洽的 CodeMirror 变化先校验 UTF-16 边界和新文档长度，再通过 4,096 UTF-16
unit 的分块 Fenwick 索引换算 rune 区间，调用既有原子 `document.replace()`。
常规局部输入只更新受影响块；跨块变化重建索引。多区间、缺失或不一致的变化
仍走全文比较并只发一个 frame。

**修改文件**：

- `clients/typescript/src/bindings.ts:150-212,360-430,891-1169`
- `clients/typescript/test/bindings.test.mjs:392-455`
- `clients/typescript/test/bindings.real.test.mjs:77-100`

**方案说明**：这保留了现有 RGA TypeID、frame 编码、字节/rune 上限和远端投影
语义。没有为多区间事务引入多个可部分成功的本地帧，也没有假设远端 frame
天然携带显示增量。

### 4.2 影响范围评估

只扩展 TypeScript 绑定 API 的可选结构字段，旧的 `{ docChanged: true }` 调用
保持全文回退兼容。富文本、块、嵌入、认证、传输和 RGA wire 兼容性均未改变。

## 5. 预防措施

### 5.1 代码层面

- [x] 单区间原生编辑先验证范围、UTF-16 边界、新长度和现有编辑上限，再变更 RGA。
- [x] 多区间事务保持一个原子回退 frame，不拆分为删除/插入序列。
- [ ] 新增编辑器适配器时，将“是否可提供可信原生变更集”加入审查清单。

### 5.2 测试层面

- [x] 覆盖 Unicode surrogate、4,096-unit 分块边界、超限拒绝、真实 CodeMirror
  单区间及多区间事务、远端 no-echo 和真实 Go/Wasm 互操作。
- [x] 基准并排输出 `native_incremental` 与 `full_projection_fallback`。

### 5.3 监控层面

- [ ] 宿主应用可按编辑器类型/文档长度采集本地事务耗时；库本身不新增遥测或
  上传用户文本。

### 5.4 流程/规范层面

- [x] 在 RGA 编辑器绑定集成文档中记录增量适用范围和远端全量投影边界。
- [ ] 发布前在目标浏览器/设备上复测用户实际文档分布；Node 基准不等同设备 SLA。

## 6. 经验总结（一句话）

编辑器已提供可信单区间事务时，应保留其增量语义并在本地转换坐标；无法原子
表达的多区间事务必须保守回退，不能用拆帧换取表面性能。
