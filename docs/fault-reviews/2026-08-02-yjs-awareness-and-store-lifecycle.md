# 故障复盘：Yjs awareness 删除语义与请求级 Y.Doc 生命周期缺口

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-08-02 |
| 发现人 | Codex 代码审计与真实 Yjs 集成验证 |
| 严重程度 | P2-一般 |
| 影响范围 | `extensions.YJSHandler` 的临时 presence，以及 Level 1 `YJSStore` Node sidecar 的 apply、state-vector、diff、snapshot 请求 |
| 关联 Issue/PR | 无 |
| 关联提交 | `b979850`、`2519f6e`、`8c65886` |

## 1. 问题描述

### 1.1 问题场景

一个 Yjs 客户端在 clock `N` 发布 awareness 后，以同一 clock `N` 发布
`null` 表示离线；网络可能随后重放旧的非空状态。另一个场景是同一已认证
用户打开两个浏览器标签页，先关闭其中一个。与此同时，Level 1 sidecar 在每
次 HTTP 操作中都从持久快照构造新的 `Y.Doc`。

### 1.2 具体表现

修复前的 relay 将同 clock 的 `null` 当作普通重复帧忽略，或者在删除后不保留
clock 元数据，使已离线的光标/用户状态可能停留或被延迟的旧状态重新显示。若
ownership 只按认证后的 `Peer.ID` 建模，关闭一个标签页还可能清理同一用户另
一个标签页的 presence。

修复前 sidecar 的请求级 `Y.Doc` 没有在 apply、state-vector、diff、snapshot
结束时显式销毁。连续操作会把观察者、subdoc 和实现持有的引用的释放时机交给
垃圾回收，无法给出稳定的资源生命周期边界。

### 1.3 错误信息

没有线上告警、用户日志或数据修复记录；问题由协议规则和可重复的本地测试
审计发现。`TestYJSAwarenessPreservesStandardNullClockAndTombstoneOrdering`
复现相同 clock 的删除与旧状态延迟重放，sidecar 测试通过替换
`Y.Doc.prototype.destroy` 计数每次请求的释放。

## 2. 临时解决方案（可选）

### 2.1 方案描述

无。未采用在调用方给 clock 加一、按用户互斥标签页或依赖 Node GC 的临时规避，
因为这些做法分别改变 y-protocols 语义、破坏多标签页 presence，或没有可验证的
释放时限。

### 2.2 止血效果

不适用。该问题在发布前的审计中修复。

### 2.3 临时方案的局限

调用方补偿不能覆盖重排/重放，用户级 owner 不能区分连接，GC 也不能替代每个
请求完成时的资源释放。

## 3. 根本原因分析

### 3.1 问题分析过程

1. 先核对原生 Yjs 路径：Go relay 只能处理有界 y-protocols 包装，语义状态由
   固定版本的 Node/Yjs sidecar 持有，不能把 Go CRDT 的时钟规则套用到 awareness。
2. 检查 awareness 接收顺序后，发现删除必须同时表达“当前不存在”和“已见
   clock”；只保存活动状态的 map 无法阻止删除后的旧状态复活。
3. 检查 WebSocket 关闭清理路径后，确认连接与认证主体是一对多关系；`Peer.ID`
   不是 presence 的连接所有权。
4. 检查 `loadDocument` 的所有 HTTP 调用点，发现每个请求创建 fresh `Y.Doc`，但
   没有结构化的 `destroy()` 对称释放。
5. 添加相同 clock `null`、旧帧重放、两标签页断开、tombstone 上限和四类 sidecar
   操作的销毁计数回归；随后执行真实 provider、竞态、fuzz 与基准矩阵。

### 3.2 直接原因

awareness 旧实现没有把活动值与删除 tombstone 分开建模，且所有权粒度低于实际
WebSocket 连接。sidecar 则在请求完成后遗漏了 `Y.Doc.destroy()`。

**相关代码位置**：`extensions/yjs.go:103-117`、
`extensions/yjs.go:505-607`、`yjsstore/runtime/server.mjs:133-224`（均为修复后）。

### 3.3 根本原因

- **设计层面**：把 ephemeral awareness 误当成简单的“最新 JSON”缓存，而不是
  含 clock、删除状态和连接所有权的协议状态；将请求构造的对象误当成 GC 自然
  管理的对象。
- **开发层面**：初版重点覆盖了更新中继、鉴权、消息队列和持久快照，遗漏了
  same-clock `null`、重放以及同主体多连接的组合情形。
- **流程层面**：真实 provider 测试覆盖了收敛与恢复，但没有把 presence 删除
  顺序和每个 request materialization 的销毁次数列为明确断言。

### 3.4 为什么没有提前发现

- **代码审查阶段**：审查了 owner 是否可鉴别，却没有质疑其是否应等同于认证
  主体；审查了内存上限，却没有要求短生命周期对象显式释放。
- **测试阶段**：覆盖了较新 clock 的更新和普通断开，没有覆盖相同 clock 的
  `null`、删除后的旧帧和同用户两条 WebSocket。
- **监控告警**：没有线上事件可作为容量结论。本次用确定性的协议与 destroy
  计数测试建立回归，而非用 GC 时机或生产遥测推断正确性。

## 4. 解决方案

### 4.1 根本解决方案

**修改文件**：`extensions/yjs.go`

`yjsAwarenessOwner` 以 `*yjsSubscriber` 标识实时连接，直接测试才回退到
`Peer.ID`。活动值与 `yjsAwarenessTombstone` 分离；同 clock 的 `null` 清除活动
状态，并保留不含 JSON 的 clock/owner tombstone。`MaxAwarenessTombstones` 默认
为 256，并在加入新 tombstone 前淘汰最早项，因而删除重放防护不无限增长。

**修改文件**：`yjsstore/runtime/server.mjs`

apply、state-vector、diff、snapshot 都把 `loadDocument` 的结果包在 `try/finally`
中，并在 finally 调用 `state.document.destroy()`。这覆盖正常返回、格式拒绝和
持久化失败路径。

**测试文件**：`extensions/yjs_test.go:112-198`、
`yjsstore/runtime/runtime.test.mjs:108-129`。

**方案说明**：该方案保持 wire payload、room identity、V1/V2 格式钉扎与认证/
授权边界不变。它只补齐 y-protocols 的删除顺序、连接级资源所有权和 request
资源清理；不尝试把 Yjs awareness 或状态向量转换为本库的 Go CRDT 协议。

### 4.2 影响范围评估

- presence 删除即时广播 `null`，后到的同 clock 非空状态不会重建已删除记录。
- 同一用户的两个连接互不清理对方 presence；不同连接不能接管已有 client ID。
- tombstone 只保存 clock、owner、时间，不保存用户状态 JSON；其容量独立受限。
- sidecar 每次操作多一次确定性销毁；文档快照和响应格式不变。
- 该修复不证明任何 live event 是持久回执、业务事务或授权证明，也没有引入
  Yjs 与 Go RGA 的互操作转换。

## 5. 预防措施

### 5.1 代码层面

- [x] 所有不可信输入驱动的 retained map 都在分配前验证上限，并为删除 metadata
  单独定义容量与淘汰规则。
- [x] presence/lease/cursor 所有权使用实际连接或不可复用会话标识，不能用可多开
  的用户主体替代。
- [ ] 评审所有 protocol clock 合并逻辑时列出 `<`、`==`、`>` 与 tombstone 的完整
  接受表。
- [ ] 评审所有 request-scoped Yjs 对象时要求 `try/finally` 的构造/销毁对称性。

### 5.2 测试层面

- [x] 保留 same-clock `null`、旧状态重放、连接级断开和 tombstone 容量回归。
- [x] 保留 real Yjs V1/V2、真实 `y-websocket` provider、离线并发合并和恢复测试。
- [x] 保留 relay race、畸形 wire fuzz 和 1/4/16/64 receiver 的受控 loopback 基准。
- [ ] 发布容量结论前，在目标 TLS/gateway/存储/浏览器环境重跑真实多客户端压测。

### 5.3 监控层面

- [ ] 产品接入时记录匿名化的 awareness 活动数、tombstone 命中/淘汰、队列关闭、
  sidecar 操作延迟/错误和 Node heap；不得记录 awareness JSON、文档 update 或
  凭据。

### 5.4 流程/规范层面

- [x] 将 Yjs 的 Level 0 live relay 与 Level 1 semantic/durable sidecar 边界写入
  集成文档和验证记录。
- [ ] 将“live 广播不是 durable receipt”“同主体可多连接”“删除状态也要受限”
  加入协议代码评审清单。

## 6. 经验总结（一句话）

> 对协作 presence，删除不是丢掉一条 JSON：必须同时保留有界 clock 证据、按连接界定所有权，并在每个请求结束时显式释放语义引擎对象。
