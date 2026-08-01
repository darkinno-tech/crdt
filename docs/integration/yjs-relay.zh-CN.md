# Yjs / y-protocols 兼容 relay

`extensions.NewYJSHandler` 是一个显式启用、资源有界的 WebSocket relay，兼容稳定版
`y-websocket` / `y-protocols` 消息外层。它与本模块的 framed CRDT 完全隔离：**不会**把
RGA、rich-text、snapshot、manifest 或 Go `awareness` 状态转换成 Yjs 文档。

这个边界不能省略。Yjs update 含有 Yjs 专用 client ID、struct clock、delete set 和 shared
type 语义；将其当作 run-v2 RGA delta 会损坏任一侧，或错误承诺并不存在的恢复能力。

## 兼容范围

handler 接受标准 `y-websocket` 二进制消息：

| 顶层 type | 内层含义 | relay 行为 |
| --- | --- | --- |
| `0` | sync Step 1 | 返回合法的空 Step 2；随后以普通 update 消息发送保留 update。 |
| `0` | sync Step 2 或 update | 保留有界的 opaque Yjs update 并 fan-out。 |
| `1` | awareness | 校验 y-protocols awareness wrapper，并 fan-out 最新临时状态。 |
| `3` | awareness query | 返回 room 中有界的最新 awareness 状态。 |

已使用 `yjs@13.6.31`、`y-websocket@2.1.0` 和官方支持 y-protocols 的 provider 完成真实
验证：两个 Node client 完成初始同步、复制 `Y.Text` 变更并传播 awareness。这并不等同于已经
证明任意不可信 Yjs update 的语义有效性。

## 挂载一个显式 room

room 必须在启动时配置；不可信 URL 不能创建会被服务端保留的状态。

```go
room, err := extensions.NewYJSRoom(extensions.YJSRoomConfig{
    Name:            "notes",
    MaxUpdateBytes:  1 << 20,
    MaxHistoryBytes: 8 << 20,
    MaxUpdates:      256,
})
if err != nil { return err }

handler, err := extensions.NewYJSHandler(extensions.YJSConfig{
    Rooms: []*extensions.YJSRoom{room},
    Authenticate: func(request *http.Request) (extensions.Peer, error) {
        // 校验 session、JWT、mTLS identity 或可信代理认证。
        // 绝不能从 Yjs client ID 推导身份。
    },
    AuthorizeSubscription: func(peer extensions.Peer, room string) error {
        // 校验租户/文档读权限。
    },
    Authorize: func(peer extensions.Peer, room string, kind extensions.YJSMessageKind) error {
        // 独立校验文档写入或 presence 权限。
    },
    OriginPatterns: []string{"app.example.com"},
})
if err != nil { return err }

mux.Handle("/yjs/", http.StripPrefix("/yjs", handler))
// y-websocket 连接 wss://host/yjs/notes。
```

handler 禁用 per-message compression，并限制 read、单消息、queue、history、update 和
awareness client 数量。慢 peer 会被断开，不会造成无界应用内存队列。最新 awareness client ID
绑定到已认证连接；其他连接转发的相同或更旧状态仍允许通过，因为 `y-websocket` 会主动重新广播
它们；来自另一连接的更新 clock 则会关闭发送端。

## 持久化和恢复边界

room 仅在 `MaxUpdates` 或 `MaxHistoryBytes` 内保留完整 update。它不能解析、merge、compact、
snapshot 或授权 Yjs 文档内部。历史满时会拒绝写入，而不是静默淘汰；淘汰会让重连 client 看似
已同步，实际却缺失因果数据。

生产环境应在相同的认证 room 边界后接入一个 Yjs-aware durable store：它需要验证/应用 update、
生成合并后的 snapshot/update，并保留恢复 cursor。持久化 update、文档生命周期、subdocument、
权限撤销、限流、TLS、备份恢复和滥用防护仍由宿主负责。

Go [`awareness`](../protocol/awareness-v1.md) 是另一套带认证的 Go-provider 协议；不要将其 update
与 Yjs awareness bytes 混用，也不要把两者持久化为 CRDT document update。

`YJSHandler` 故意没有通用的 Go-awareness ↔ Yjs-awareness 开关。两套协议分别使用已认证身份
（`actor` 与 client ID）、独立的单调 clock，以及可能不同的 cursor schema；复用任一身份或 clock
都会造成 equal-clock conflict、competing-client ownership failure，或让重连覆盖 presence。确有
联邦需求的产品必须实现 application gateway：显式绑定 tenant/room/epoch、注入式分配 external
client ID、为目标协议生成单调 clock、分别授权两个方向、限制 fan-out，并定义 cursor metadata 的
loss policy。它应被视为一个新的 presence capability，并以真实 client interoperability test 验证，
而不是透明 relay option。

## gRPC 已经是原生实现，而非 Yjs transport

仓库已有 [`extensions.Relay`](extensions.md#native-grpc-relay)：它是为 manifest-bound Go CRDT
change 提供的生成式双向 gRPC service。它先交换精确 `replica.Manifest`，再承载 canonical change
envelope，并带有必需的认证、读写授权、有界 queue、telemetry 和 client/server 测试。它故意不承载
Yjs update bytes，因为二者恢复与授权契约不同。

## 验证和性能范围

```sh
go test ./extensions -run 'TestYJS|FuzzUnmarshalYJSMessages'
go test -race ./extensions -run 'TestYJS'
go test -run '^$' -bench='BenchmarkYJSWireDecodeAndAdmission$' -benchmem -benchtime=1s ./extensions
```

该 benchmark 仅测量本地 wire decode 和 duplicate-aware 内存 admission，不代表浏览器、TLS、WAN、
durable store 或服务容量。部署前必须在目标 CPU、限制、文档规模及 Yjs-aware 持久化实现上重新测量。
