# 故障复盘：YJSStore 未限制活跃请求且监听前未复核存储目录

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-08-02 |
| 发现人 | Codex 多维架构、安全与性能审计 |
| 严重程度 | P2-一般 |
| 影响范围 | `yjsstore/runtime` 的 loopback HTTP sidecar；使用直接 server 构造或遭遇慢请求/突发请求的部署 |
| 关联 Issue/PR | 无 |
| 关联提交 | `48ed32c`、`4f4de95` |

## 1. 问题描述

### 1.1 问题场景

Level 1 YJSStore 是受信 Go 进程调用的 loopback 持久化 sidecar。每条 HTTP 请求会读取 JSON
body、base64 解码、从磁盘 materialize 一个 `Y.Doc`，并在成功后 fsync/rename snapshot。原实现
仅限制单条请求的字节数和每个 document 的 keyed lock，没有限制同时进入这些步骤的请求数。

此外，`loadConfig()` 会验证数据目录是非 symlink 的 `0700`，但导出的
`createYJSStoreServer()` 在实际监听前不再验证。嵌入方直接构造 server，或配置加载到监听之间
权限被改为更宽时，持久化信任边界不再得到运行时保证。

### 1.2 具体表现

- Node HTTP server 的默认完整请求接收超时是五分钟；半包请求可在进入 `readBody` 后长期占用
  request、body buffer、后续 document lock 和潜在的 `Y.Doc` materialization 预算。
- 多个不同 document 或同一 document 的突发请求没有 process-wide admission，单条有界并不等于
  总资源有界，过载会表现为 heap/GC 压力和尾延迟放大，而非明确可恢复的 backpressure。
- 已加载的 0700 目录如果在 `listen()` 前变为 0755，旧实现仍会开始监听。

该问题在 beta 发布前审计与真实 loopback 半包复现中发现；没有生产用户数据、报警或修复记录。

### 1.3 错误信息

修复后的受控复现结果：

```text
maxConcurrentRequests=1 + half request body -> parallel snapshot: 503 {"code":"unavailable"}
complete first request -> later snapshot: 200
requestTimeoutMillis=1000 + half request body -> Node response: 408
chmod loaded data directory to 0755 -> listen(): YJS_STORE_DATA_DIR must be a non-symlink 0700 directory
```

以上是本地测试输出，不包含生产日志、document bytes、token 或用户身份。

## 2. 临时解决方案（可选）

### 2.1 方案描述

无。没有把依赖网关“通常会限流”、增大 Node heap、或在超载时继续积压 body 作为永久方案。

### 2.2 止血效果

不适用；在 beta 发布前完成运行时 admission、超时和目录复核修复。

### 2.3 临时方案的局限

网关限流不会覆盖 loopback 上被错误配置、直接访问或正常内部突发的语义工作；仅增加 heap 会扩大
可消耗资源而不定义公平的失败语义。配置加载时的一次目录检查也不能覆盖 listen 前的权限变化。

## 3. 根本原因分析

### 3.1 问题分析过程

1. 按 update、snapshot、state vector、磁盘 fsync、Yjs materialization、keyed lock 和 HTTP
   body 的完整调用链审计资源上界。
2. 确认 `handleRequest()` 在 `readBody()` 前没有 process-wide gate；`KeyedLock` 只序列化相同
   document，不能限制不同 key 的活跃请求，也不会对同 key 的等待者提供总数上限。
3. 用 Node HTTP API 的受控 loopback 半 JSON body 保持第一条请求未完成；第二条请求在旧设计中
   没有明确 admission 失败点。核对 Node v26 文档，默认完整请求超时为 300 秒。
4. 审查 `loadConfig()` 和 `createYJSStoreServer()`：前者调用 `ensureSecureDataDirectory()`，后者
   仅作字段校验。将加载后的临时目录 chmod 为 0755 可重现信任边界漂移。
5. 选择“请求 body 前 fail-fast admission + Node 的 nonzero receive/header deadline + listen 前
   recheck”而非修改 Yjs wire、缓存长期 Y.Doc、或让 handler 静默重试。
6. 新增慢 body、over-cap 503、slot 释放、408、权限漂移及配置边界测试；同时保留显式
   16-slot 的并发写者真实 Yjs 收敛验证，随后运行 Go/Node 集成和 1/4/16/64 benchmark。

### 3.2 直接原因

修复前 `yjsstore/runtime/server.mjs` 的 `createYJSStoreServer()` 直接把每个 request 交给
`handleRequest()`；`handleRequest()` 在认证、content-type 和 content-length 检查后无条件进入
`readBody()`。`loadConfig()` 的目录验证不在 `listen()` 重新执行。

**相关代码位置**：修复后 `yjsstore/runtime/server.mjs:15-21`、`43-101`、`121-130`、
`569-603`；回归在 `yjsstore/runtime/runtime.test.mjs` 的 permission、saturation 和 timeout
用例。

### 3.3 根本原因

- **设计层面**：把“每条 payload 有上限”和“每个 document 有锁”误当成 process-wide work
  budget，忽略 body 收集、等待锁和 Yjs materialization 可被多个独立请求同时占用。
- **开发层面**：依赖 Node 默认 receive timeout，未把 Go client 的十秒默认 deadline和 sidecar
  的接收边界作为一个配套契约；将 `loadConfig` 的启动期检查误当成 server 生命周期不变量。
- **流程层面**：已有测试关注无效/超大 update、持久回滚和 16 writer 收敛，没有模拟半包、满
  admission、超时释放和加载/监听之间的权限漂移。

### 3.4 为什么没有提前发现

- **代码审查阶段**：重点审查 Yjs V1/V2、base64 前边界、atomic rename 和 redirect token，遗漏
  了 HTTP read-body 前的总并发和 Node runtime 默认超时。
- **测试阶段**：并发 writer 测试只证明 Yjs 合并正确性，未将容量设为小值来验证 backpressure
  与恢复；目录测试只在 `loadConfig()` 时执行。
- **监控告警**：没有 active/rejected/timeout 的匿名聚合指标，过载只能从 Node heap 或尾延迟
  间接判断。

## 4. 解决方案

### 4.1 根本解决方案

**修改文件**：`yjsstore/runtime/server.mjs`

新增 `YJS_STORE_MAX_CONCURRENT_REQUESTS`（默认 4，范围 1..64）和
`YJS_STORE_REQUEST_TIMEOUT_MS`（默认 10000，范围 1000..120000）。server 在请求 listener
开始处尝试获得 `RequestAdmission`；满载时 `resume()` 丢弃 body 并返回 `unavailable`，不进入
`readBody`。已接纳请求无论成功、业务错误或 HTTP timeout 都在 `finally` 释放 slot。Node 设置
request timeout、header timeout 和一秒检查间隔。`listen()` 在 bind 前复用
`ensureSecureDataDirectory()`。

**测试文件**：`yjsstore/runtime/runtime.test.mjs`

使用真实 Node HTTP client 写半包 JSON，而非 mock：验证 503、首请求完成后恢复、1 秒 timeout
的 408 与恢复。加载后 chmod 0755 验证 server 拒绝监听；16 writer 测试显式配置 16 slot 后继续
验证真实 Yjs 合并和重复 cursor。

**方案说明**：选择 fail-fast 503，不在 sidecar 保存无界等待队列。这样拒绝的 application body
不被收集，调用方可在其已有 durable outbox/state-vector 恢复层退避。没有将 Yjs document 常驻
缓存以“提高并发”：那会改变既有每请求 destroy 的资源模型，并要求新的 eviction、崩溃一致性和
heap 合约。新环境变量可选，直接 embedding server 获得相同安全默认值。

### 4.2 影响范围评估

- 正常低于 configured capacity 的请求语义、Yjs update、V1/V2、state vector、snapshot record、
  Go `YJSStore` API 和 y-websocket relay 不变。
- 超过 configured capacity 的 Apply/Diff/Snapshot/Merge 会得到 `unavailable`；Go 已将其映射为
  `ErrYJSStoreUnavailable`，上层不得把该 update 当作持久成功或再次编辑来“重试”。
- 默认 4 是保守的 heap/尾延迟预算。需要 16 个同时 durable writers 的部署必须显式设置 16，
  并同时压测 Node heap、磁盘、Go deadline 和 retry backoff。
- Node 408 响应来自 HTTP receive timeout，客户端可能只看到中断/`unavailable`；它不包含 Yjs
  业务错误，也不会产生 durable mutation。

## 5. 预防措施

### 5.1 代码层面

- [x] 在 body 收集和 semantic work 前建立 process-wide bounded admission。
- [x] 配置 nonzero header/body receive deadline，并在超时/错误/成功时释放 slot。
- [x] 在实际 listen 点复核持久化目录权限，直接 server 构造也有安全默认值。
- [ ] 所有 Node sidecar 新端点都必须在设计评审中写清：single-request bytes、active work、
  排队/拒绝、receive deadline、heap 预算和 durable-success 边界。

### 5.2 测试层面

- [x] 保留真实半包、503、slot recovery、408、directory permission drift 与 16 writer 回归。
- [x] 保留 real Yjs V1/V2、corruption/restart、Go/Node/offical y-websocket 集成和 1/4/16/64
  loopback benchmark。
- [ ] 在目标宿主加入 1/4/16/64 simultaneous writer、slow disk、contention、retry backoff、
  process restart 和 Node heap ceiling 的长时压测。

### 5.3 监控层面

- [ ] 仅在可信进程边界记录 aggregate active/rejected/timeout/request-duration/heap 指标；禁止
  记录 document bytes、room identity、Yjs client ID、bearer token 或用户身份。

### 5.4 流程/规范层面

- [x] 集成文档明确 admission、503、408、directory recheck、配置范围与 caller recovery。
- [ ] 在 CRDT/Yjs 安全检查表增加“生命周期时的配置不变量”和“默认 runtime timeout 是否与调用方
  deadline 配套”两项。

## 6. 经验总结（一句话）

> 单条 Yjs payload 有界和 per-document 锁并不等于 sidecar 总工作有界；必须在读 body 前限活跃请求、为慢请求设置可验证 deadline，并在真正监听时复核持久化信任边界。
