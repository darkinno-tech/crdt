# 可选 WebSocket 与 HTTP/SSE relay 参考实现

[English](extensions.md) | [简体中文](extensions.zh-CN.md)

`extensions` 是本 Go 模块提供的、与 Manifest 绑定的 live relay 官方参考实现。它
必须显式开启：零值 feature 不暴露任何端点、不调用认证，也不启动 listener 或后台任务。

接入只需将 handler 挂载到应用自己拥有的 mux，并开启需要的 feature：

```go
handler, err := extensions.NewHandler(extensions.Config{
	Features: extensions.FeatureWebSocket | extensions.FeatureHTTP,
	Groups:   []*extensions.Group{group},
	// 此处必须提供 Authenticate、Authorize 和 AuthorizeSubscription。
})
if err != nil {
	return err
}
if err := handler.Mount(mux, "/crdt/"); err != nil {
	return err
}
```

其中 `group` 是由应用创建的、绑定 Manifest 的 `extensions.Group`，其 `Apply`
回调属于应用状态边界。包含完整认证/授权回调的可运行代码见
[examples/extensions-provider](../../examples/extensions-provider)：

```sh
go run ./examples/extensions-provider
```

预期输出：

```text
websocket_to_http=2
http_to_websocket=5
relay=5
```

示例使用 `httptest`，可独立运行。生产宿主应将同一 handler 挂入自己的
`http.Server`，配置 TLS，并让浏览器使用 `wss://`。

## 功能开关与端点

`Config.Features` 是构建 handler 时确定的攻击面开关，而不是可变的全局设置。若需
变更端点，应在部署/配置重载时创建新 handler；不要把运行中的 listener 当成动态授权机制。

| Feature | 端点 | 默认值 |
| --- | --- | --- |
| `FeatureWebSocket` | `GET <mount>/ws`，子协议 `crdt-sync-v1` | 关闭 |
| `FeatureWebSocketBatch` | 经协商的 `crdt-sync-v2` WebSocket 批量 envelope | 关闭 |
| `FeatureHTTP` | `POST` 投递变更，`GET` 订阅 Server-Sent Events | 关闭 |

当挂载点为 `/crdt/` 时，HTTP 路由如下：

| 方法 | 路径 | 含义 |
| --- | --- | --- |
| `POST` | `/crdt/http/groups/{base64url(group-id)}/changes` | 发布一个规范化变更 envelope（`application/octet-stream`）。 |
| `GET` | `/crdt/http/groups/{base64url(group-id)}/events` | 订阅仅包含后续 live 变更的 SSE 流（`text/event-stream`）。 |

客户端应使用 `ConnectHTTP`，而不是自行拼接 group 路径；它会从提供的
`replica.Manifest` 推导 base64url 路径。`DialWebSocket` 和 `ConnectHTTP` 都会在
接受 live 数据前校验精确 Manifest。
客户端成功返回也是 live 订阅的线性化点：relay 会先注册 peer、再发送确认，因此调用方可
在握手后立即发布，不会存在额外的事件丢失窗口。

## 可选 WebSocket 批量传输

`FeatureWebSocketBatch` 默认关闭，且只能与 `FeatureWebSocket` 一起开启。当 relay 与客户端
都显式开启时，WebSocket 握手可协商 `crdt-sync-v2`；否则客户端回退到 `crdt-sync-v1`，调用
`PublishBatch` 会返回 `ErrBatchUnsupported`。

批量只是 transport 合并，不是原子 CRDT 或存储事务。每个条目都保留自己的 dot 和规范化
v1 envelope。relay 会在第一次 Inbox mutation 前校验并授权全部条目，再按顺序接收。若
应用 callback 或 pending-state 上限拒绝了后续条目，relay 会先转发此前已经接受的条目，
再关闭发布连接。调用方必须在 durable outbox 中保留每个原始条目，并在结果不明确时逐项重试。

批消息同时受 `MaxMessageBytes` 和 `MaxBatchChanges` 限制。默认最多 16 项，且不能超过
`MaxQueuedMessages`，因此 v1 WebSocket 或 SSE peer 要么完整入队全部条目消息，要么被断开，
不会在应用层留下部分队列插入。客户端与 relay 都会执行各自配置的条目上限；应让两者保持兼容
（默认值一致）。支持批量的 WebSocket peer 收到一个 batch envelope；HTTP 与 SSE 仍使用
已文档化的单变更 v1 envelope。

## 该参考实现保证什么

每个连接先协商带版本的 Manifest。每个被接受的变更都会先受字节上限保护，再解码为
规范 CRDT 帧，校验对应 Manifest 和 `ProtocolPolicy`，并在 `Group.Apply` 改变状态前
通过授权。`replica.Inbox` 提供有边界的重复/乱序处理。relay 只向 live peer 转发一次
首次接受的 dot；已安装或已缓冲的重复不会被网络放大。

`GroupConfig.Apply` 会在每个 group 的投递顺序锁持有期间调用。它必须使用有边界的具体
解码器、出错时保持应用状态不变，且不得重入 `Group` 或阻塞等待 transport callback。

读写权限被刻意分离：

- `Authenticate` 在升级或读取 body 前建立应用身份。
- `Authorize` 获得该身份、精确 Manifest 和待写 dot；至少应绑定认证身份与 CRDT actor。
- `AuthorizeSubscription` 独立控制是否能读取 live 事件。

默认上限较保守：单消息 1 MiB、actor ID 128 字节、每 peer 16 条排队消息或 4 MiB、
握手与写入 deadline 均为 10 秒。队列满时会关闭并移除慢 peer，而不是无限增长。WebSocket
压缩已关闭。只有在测量真实消息负载后才能调高限制，同时必须保留有界失败语义。

对带 `Origin` 的浏览器请求，默认允许 request host。`OriginPatterns` 可额外配置
不区分大小写的**主机** glob，例如 `app.example` 或 `*.example.internal`；不得包含
scheme、path、query、fragment、空值或 `*`。HTTP/SSE 与 WebSocket 使用同一套主机模式，
避免跨域规则漂移。没有 `Origin` 的非浏览器客户端仍须通过正常认证和授权。

## 有意保留的生产边界

这是 live relay 参考实现，不是持久化复制服务，也不替代明确声明为非生产诊断工具的
`crdt-sync-probe`。它没有操作日志、快照存储、持久化 outbox、重放端点、自动重连、
反熵循环、成员权威、TLS listener 或 token/session 实现。HTTP/SSE 一次发布一个变更，
仅流式发送此后发生的 live 事件，不能恢复错过的事件。

因此宿主应用必须自行负责：

1. 在同一事务中持久化 CRDT 状态及对应的 `replica.Frontier`，并维护 outbox/receipt 策略。
2. 从 checkpoint 恢复，并使用状态/Merkle 反熵修复错过的历史。
3. TLS、认证/session 生命周期、按租户的 group 查询、限流、滥用防护、可观测性和容量规划。
4. 已授权的成员关系、checkpoint 分发、副本退役和墓碑生命周期。

## gRPC 原生 relay

`extensions.Relay` 提供原生双向 gRPC stream；其生成式 schema 位于
[`extensions/relay.proto`](../../extensions/relay.proto)。首个双向消息都是精确的
`replica.Manifest` 编码，之后只承载既有规范 change envelope，不会为 gRPC 另造 CRDT
frame 或混入 WebSocket 子协议。

```go
server, relay, err := extensions.NewGRPCServer(extensions.GRPCConfig{
	Groups:                []*extensions.Group{group},
	Authenticate:          authenticateGRPC, // mTLS、可信 interceptor 或已验证 metadata。
	Authorize:             authorizeWrite,
	AuthorizeSubscription: authorizeRead,
})
_ = relay
_ = server // 由宿主以自己的 listener、TLS 与 graceful shutdown 提供服务。
```

共享 `grpc.Server` 时，先创建 `NewGRPCRelay`，在构造 server 时传入
`relay.ServerOptions()`，再调用 `RegisterRelayServer`。这样保留宿主自己的 mTLS、
interceptor、health、指标和生命周期。`GRPCAuthenticate` 收到的是 context；metadata
在完成凭据验证前不可信，绝不能把 CRDT actor 当作身份。

relay 在注册订阅后才发回 manifest 确认，因此客户端成功握手就是 live subscription 的
线性化点。它复用 `Group` 的 Manifest/policy 校验、读写授权、有界 Inbox、重复/乱序收敛和
仅首次接受 dot 的 fan-out。HTTP/2 的 gRPC flow control 不等于应用内存上限；每 stream 仍有
有界 queue，慢消费者会被断开。

客户端必须设置现实的 deadline，并在 stream context 取消时停止自己的工作。`Send` 成功仅表示
gRPC transport 接收了消息，不能证明对端已持久化。持久 outbox、snapshot/frontier 恢复及重连后
的反熵仍由应用负责。

## 多维设计审查

| 维度 | 决策 | 证据/后果 |
| --- | --- | --- |
| 正确性 | 精确 Manifest 与封闭 `ProtocolPolicy`；有界 `Inbox`；仅对首次接受 dot fan-out。 | schema/epoch/protocol 不匹配和损坏帧会在应用状态改变前失败；重试和乱序可收敛。 |
| 安全 | 默认关闭；应用拥有认证并分离读写授权；严格 host pattern；关闭压缩。 | 仅 import 包不会出现匿名端点；两种传输的跨域策略一致。 |
| 性能 | 固定帧与队列上限；断开慢 peer；HTTP 发布是单个有界请求。 | 每 peer 内存有上限；调高限制前必须在目标负载上测量。 |
| 可用性 | 不隐藏 listener、存储、重试或重连循环。 | 宿主可沿用自己的 HTTP/TLS/可观测性栈并控制生命周期。 |
| 兼容性 | WebSocket 使用 `crdt-sync-v1`；gRPC 使用生成的 `Relay.Sync`；二者均协商精确 Manifest 并承载同一 CRDT envelope。 | 未知 transport 版本和不兼容 group 会 fail closed，而不是猜测兼容。 |

## 验证命令

测试区分本地 loopback 集成和确定性网络模拟；两者均不能证明浏览器、外部身份提供方或生产
数据库事务已经正确。

```sh
# 单元、真实 loopback WS/HTTP/SSE、重复/乱序、并发和示例。
go test ./extensions ./examples/extensions-provider

# 共享状态与连接生命周期竞态。
go test -race ./extensions

# 有界解析器健壮性。
go test -run='^$' -fuzz=FuzzWireDecoders -fuzztime=10s ./extensions

# 当前机器上的 loopback 传输成本；不能将其当作 SLA。
go test -run='^$' \
  -bench='Benchmark(GroupReceive|WebSocket(Batch)?Publish|HTTPPublish)Loopback$' \
  -benchmem ./extensions
```

WebSocket 与 HTTP 基准会在下一次发送前等待发布客户端观察到自己已被接受的 live 变更。
这测量的是有界的端到端 loopback 路径；未确认的持续灌入会受到慢 peer 队列上限约束，并可能被断开。

`make test-unit`、`make fuzz` 与 `make coverage` 均已包含扩展包；仓库保持每包 90%
覆盖率门禁。
