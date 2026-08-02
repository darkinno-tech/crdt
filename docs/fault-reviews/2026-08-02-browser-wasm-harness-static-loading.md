# 故障复盘：浏览器 Wasm 验收 harness 在静态服务下无法完成

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-08-02 |
| 发现人 | Codex 浏览器验收 |
| 严重程度 | P2-一般 |
| 影响范围 | `clients/typescript/test/browser-harness.html` 的 Chromium 本地验收 |
| 关联 Issue/PR | 无 |
| 关联提交 | `848d65c` |

## 1. 问题描述

### 1.1 问题场景

从仓库根目录以普通静态 HTTP 服务打开 `clients/typescript/test/browser-harness.html`，验证实际 Go/Wasm、IndexedDB 和 BroadcastChannel 路径。

### 1.2 具体表现

页面长期显示 `Starting…`，随后在修正模块入口后又显示 `FAIL: invalid_update`。前者使模块脚本在业务 `try` 块前失败；后者把异步消息尚未到达时的空数组读取当作协议错误。

### 1.3 错误信息

浏览器模块解析报错：

```text
TypeError: Failed to resolve module specifier "yjs".
Relative references must start with either "/", "./", or "../".
```

轮询阶段的错误为：

```text
invalid_update
```

## 2. 临时解决方案（可选）

### 2.1 方案描述

调试时通过直接调用接收端 API 绕过等待窗口来验证 frame 本身有效。

### 2.2 止血效果

确认 RGA/native frame 没有损坏，但不能作为 BroadcastChannel 端到端验收，因此未保留。

### 2.3 临时方案的局限

绕过 transport 会掩盖页面加载和异步到达时序，无法证明真实浏览器路径。

## 3. 根本原因分析

### 3.1 问题分析过程

1. 启动静态服务并用 Chromium 打开 harness；DOM 停留在 `Starting…`，无业务层 console error。
2. 检查浏览器模块错误，定位到聚合入口 `dist/index.js` 递归导入 `yjs`；静态服务没有 import map 或 node_modules 裸模块解析。
3. 将 harness 改为仅导入所需的 `browser`、`frame`、`wasm` 子入口后，实际 Wasm、IndexedDB 均成功执行，但 BroadcastChannel 检查报 `invalid_update`。
4. 通过阶段化状态和实际 frame 检查确认错误发生在轮询谓词；`NativeArray.get(0)` 对空数组按契约抛出范围类 `invalid_update`，而消息仍可能在下一轮到达。
5. 轮询改为先检查 `rightCards.length === 1`，只在有元素后读取索引；最终 Chromium 通过全部路径且 console error 为空。

### 3.2 直接原因

- `browser-harness.html` 从聚合入口导入了不属于该场景的 Yjs 依赖。
- `waitFor` 在没有消息时调用 `NativeArray.get(0)`，违反了该 API 的索引边界约束。

**相关代码位置**：`clients/typescript/test/browser-harness.html:23-31`、`:123-134`（修复后）。

### 3.3 根本原因

- **设计层面**：静态验收页面未保持最小依赖图，错误地假设 ESM 裸模块会被普通 HTTP 服务解析。
- **开发层面**：等待谓词把“未到达”与“读取无效索引”混为同一状态。
- **流程层面**：此前只跑了 Node/Wasm 集成，未在真实浏览器的静态服务环境中执行 harness。

### 3.4 为什么没有提前发现

- **代码审查阶段**：检查了 Wasm loader 与持久化逻辑，没有验证 HTML 模块图。
- **测试阶段**：Node 的包解析能解析 `yjs`，且其事件调度可能先到达再读取，未暴露浏览器静态解析和空数组轮询。
- **监控告警**：harness 没有阶段状态；修复后失败结果会包含 phase 与异步错误摘要。

## 4. 解决方案

### 4.1 根本解决方案

**修改文件**：`clients/typescript/test/browser-harness.html`

1. 将聚合入口改为 `browser.js`、`frame.js`、`wasm.js` 三个相对模块，避免加载本验收不需要的 `yjs` 裸依赖。
2. 显式声明两端的 `cards` 根，保持共享根类型一致。
3. 将接收检查改为先确认长度为 1，再读取索引 0；维持两秒超时而不是吞掉错误。
4. 加入可读的 phase 和异步错误信息，使真实浏览器失败可定位而不记录 CRDT 内容或身份数据。

**方案说明**：子入口导入与真实发布包的模块边界一致；没有为测试页面引入不受控 import map、打包器或网络依赖。长度前置检查保留了数组 API 的 fail-closed 范围语义。

### 4.2 影响范围评估

- 不改变 TypeScript CRDT wire 格式、Go/Wasm artifact、RGA 协议或持久化数据。
- harness 可由普通本地静态 HTTP 服务执行。
- 真实浏览器验收仍不等同于服务端持久回执、移动端配额或断电恢复验证。

## 5. 预防措施

### 5.1 代码层面

- [x] 静态验收页面仅导入所需的相对 ESM 子入口。
- [x] 异步轮询在索引前先验证容器状态。
- [x] 失败输出携带阶段信息，不把错误静默为无限 `Starting…`。

### 5.2 测试层面

- [x] Chromium 验证页面加载、真实 Go/Wasm frame、native/RGA IndexedDB 恢复、128 次 RGA append 和 BroadcastChannel 无回执边界。
- [ ] 在 CI 浏览器矩阵可用时，将该静态 harness 纳入 Chromium/WebKit 的可重复任务。

### 5.3 监控层面

- [ ] 生产集成可安全记录 loader/IndexedDB 的错误码与阶段；不得记录 document 内容、frame 或认证信息。

### 5.4 流程/规范层面

- [x] 将静态模块解析与真实浏览器事件时序列为 TS/Wasm 发布前检查。
- [ ] 所有示例 HTML 的 import 图应在无打包器的目标服务方式下验证。

## 6. 经验总结（一句话）

> 浏览器验收页面必须使用可由目标静态服务解析的最小模块图，并把“数据尚未到达”与“非法索引”作为不同状态处理。
