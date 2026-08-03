# 故障复盘：Monaco 事件运行时校验破坏 TypeScript 类型收窄

## 基本信息

| 字段 | 内容 |
|---|---|
| 日期 | 2026-08-03 |
| 发现人 | Codex |
| 严重程度 | P3-轻微 |
| 影响范围 | 未发布的 TypeScript Monaco 增量绑定候选；构建阶段阻断，无运行时用户影响 |
| 关联 Issue/PR | 无 |
| 关联提交 | `b2da71a` |

## 1. 问题描述

### 1.1 问题场景

为 `bindMonacoPlainText` 新增不可信运行时事件的单变更快路径时，需要同时校验事件形状、UTF-16 范围和模型长度，并把结果转换为 `EditorUTF16Replacement`。

### 1.2 具体表现

初版将 `unknown` 事件先收窄为通用 `Record<string, unknown>`，再直接读取 `rangeOffset`、`rangeLength` 和 `documentLength`。严格 TypeScript 编译拒绝将这些属性用作数字，候选无法构建。

### 1.3 错误信息

```text
src/bindings.ts(1255,46): error TS18048: 'documentLength' is possibly 'undefined'.
src/bindings.ts(1262,50): error TS18046: 'change.rangeOffset' is of type 'unknown'.
src/bindings.ts(1268,5): error TS2322: Type 'unknown' is not assignable to type 'number'.
```

## 2. 临时解决方案

无。问题在本地构建阶段发现，未进入测试或发布流程。

## 3. 根本原因分析

### 3.1 问题分析过程

1. 首次 `npm run build` 在 Monaco 辅助函数处失败。
2. 检查错误定位后确认，`isRecord` 只保证对象属性可读取为 `unknown`，不会证明这些属性是安全整数或字符串。
3. 同时确认 `Number.isSafeInteger` 本身不能把 `number | undefined` 收窄为 `number`。
4. 将辅助函数入口保留为 `unknown`，先显式检查模型长度的 `typeof`，再以专用 `isMonacoContentChange` 类型守卫逐一验证三个事件字段。
5. 重新执行构建、适配器类型检查、完整 TypeScript 套件和真实 Go/Wasm 互通测试，均通过。

### 3.2 直接原因

`clients/typescript/src/bindings.ts` 的初版把通用对象检查误当成字段类型检查，导致 `EditorUTF16Replacement` 构造时仍携带 `unknown` 和可选值。

### 3.3 根本原因

- **设计层面**：运行时边界的验证责任与静态类型证明没有分层表达。
- **开发层面**：新增外部编辑器事件接口后先写了聚合条件，没有先定义可复用的字段级类型守卫。
- **流程层面**：增量实现后才首次运行 TypeScript 编译，未在补丁前执行最小构建检查。

### 3.4 为什么没有提前发现

- 代码审查阶段：初版尚未进入审查。
- 测试阶段：JavaScript 运行时测试不执行 TypeScript 静态检查。
- 监控告警：这属于本地未发布候选的编译错误，不适用线上监控。

## 4. 解决方案

### 4.1 根本解决方案

**修改文件**：`clients/typescript/src/bindings.ts`

新增 `isMonacoContentChange(value): value is MonacoContentChange`，显式验证 `rangeOffset`、`rangeLength` 为非负安全整数且 `text` 为字符串；`singleMonacoReplacement` 只在完成长度和字段验证后构造替换对象。无法证明的事件统一进入已有的整文档原子回退。

**方案说明**：专用守卫同时满足 TypeScript 的静态收窄和不可信事件的运行时拒绝需求；不使用类型断言，避免异常编辑器事件绕过边界检查。

### 4.2 影响范围评估

- 新增 Monaco 快路径只接受一处已验证的 UTF-16 替换。
- 批量、刷新、EOL 变更、旧端口和错误事件仍沿用已验证的一帧原子回退。
- RGA 线协议、资源上限、远端帧处理和其他编辑器绑定没有变化。

## 5. 预防措施

### 5.1 代码层面

- [x] 外部事件先用字段级类型守卫验证，再构造内部强类型对象。
- [x] 不以 `Record<string, unknown>` 代替数值、数组或字符串字段验证。

### 5.2 测试层面

- [x] 在适配器类型检查中加入 Monaco 结构端口赋值。
- [x] 覆盖 Unicode 偏移、批量/刷新/EOL 回退、资源拒绝恢复和真实 Go/Wasm 帧互通。

### 5.3 监控层面

- [ ] 产品宿主应统计快路径与回退比例；本库不采集用户编辑内容或遥测。

### 5.4 流程/规范层面

- [x] 新增外部编辑器协议适配时，先运行 `npm run build`，再执行运行时测试。

## 6. 经验总结（一句话）

外部编辑器事件必须以字段级运行时验证进入内部强类型路径，通用对象判断既不能替代 TypeScript 收窄，也不能替代资源与边界检查。
