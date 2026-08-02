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

## 浏览器原生路径：不需要 Go/Wasm 或 manifest 协商

当一个文档明确选择 Yjs 作为协作契约时，浏览器直接使用标准 Yjs client 和对应编辑器绑定即可，
不需要 Go/Wasm runtime、frame decoder 或 `replica.Manifest`。可以直接使用维护中的上游
binding，也可以使用可选的原生
[`@darkinno/crdt-client/yjs` CodeMirror binding](yjs-native-editor-bindings.md)；后者仍直接处理
Yjs update 与 y-protocols awareness，并不混入本仓库的 Go 或 `native-ts-v1` 协议。

```ts
import * as Y from "yjs";
import { WebsocketProvider } from "y-websocket";

const document = new Y.Doc();
const provider = new WebsocketProvider(
  "wss://collab.example/yjs",
  "notes",
  document,
);

const text = document.getText("shared");
// 按编辑器选择维护中的 adapter，例如 y-prosemirror、y-quill，
// 或产品自有的 schema-preserving binding。
```

若需要带资源上限的 CodeMirror plain-text 增量表面，可将可选原生 binding 绑定到同一个
`document` 与 `Y.Text`，但不能为同一文档配置第二个传输 owner。其 limit、cursor 模型和富文本
边界见 [原生编辑器 binding 指南](yjs-native-editor-bindings.md)。

将 `YJSHandler` 挂到 `/yjs/` 后，该 client 即连接 `/yjs/notes`，无需额外 adapter
协议。浏览器鉴权应使用同源（或正确 scope）的 Secure、HttpOnly session cookie。浏览器
WebSocket 不能附带 `Authorization` header；把长期 bearer token 放进
`WebsocketProvider` query 参数很容易被 URL、代理日志和诊断信息泄露。若跨站部署无法使用
cookie，应由已认证 HTTPS endpoint 签发极短期、绑定 room、单次使用的 connection ticket，
并在全部代理中脱敏；ticket 的重放保护属于应用自身的安全契约。

`Authenticate` 仍会在升级前执行，因此 cookie 校验保持为普通应用认证：

```go
Authenticate: func(request *http.Request) (extensions.Peer, error) {
    session, err := authenticateSessionCookie(request)
    if err != nil {
        return extensions.Peer{}, extensions.ErrUnauthorized
    }
    return extensions.Peer{ID: session.Subject}, nil
},
```

provider URL 和 room 选择应由产品配置决定，不要让页面选择未配置的 room、store identity、
schema、epoch 或字节限制。

## 挂载一个显式 room

room 必须在启动时配置；不可信 URL 不能创建会被服务端保留的状态。

```go
room, err := extensions.NewYJSRoom(extensions.YJSRoomConfig{
    Name:                   "notes",
    MaxUpdateBytes:         1 << 20,
    MaxHistoryBytes:        8 << 20,
    MaxUpdates:             256,
    MaxAwarenessTombstones: 256, // 仅保留 clock；零值采用该默认值
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
awareness client 数量。慢 peer 会被断开，不会造成无界应用内存队列。活跃 awareness client ID
绑定到精确 WebSocket，而非仅绑定已认证主体，因此同一用户第二个浏览器 tab 不会因第一个 tab
断开而被移除。relay 接受标准的同 clock `null` 下线状态，并只保留有上限的
clock/owner tombstone（不保留 awareness JSON），以阻止延迟到达的旧状态复活 ghost cursor。
相同或更旧的非空重传会被安全忽略；来自另一连接的当前竞争状态会关闭其发送端。

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
(cd extensions && go test . -run 'TestYJS|FuzzUnmarshalYJSMessages')
(cd extensions && go test -race . -run 'TestYJS')
(cd extensions && go test -run '^$' -bench='BenchmarkYJSWireDecodeAndAdmission$' -benchmem -benchtime=1s .)
```

该 benchmark 仅测量本地 wire decode 和 duplicate-aware 内存 admission，不代表浏览器、TLS、WAN、
durable store 或服务容量。部署前必须在目标 CPU、限制、文档规模及 Yjs-aware 持久化实现上重新测量。
