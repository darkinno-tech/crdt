# WebSocket Provider 参考实现

`examples/websocket-provider/provider` 是仓库提供的官方 WebSocket 传输参考实现。
它与 CRDT 核心和 `cmd/crdt-sync-probe` 刻意分离：后者仍是短生命周期 HTTP 测试工具；
本包展示应用如何挂载绑定 Manifest 的 WebSocket 端点并连接 Go 客户端。

它依然只是参考实现，不是生产复制服务。它没有持久化操作日志、快照存储、重连循环、
outbox、反熵协议、成员权威来源或墓碑 GC 策略。TLS、身份、授权、持久化、重试、
监控和故障响应均由嵌入它的应用负责。

## 参考实现已覆盖的边界

- 在 WebSocket 升级之前认证 HTTP 请求。
- 强制 `crdt-sync-v1` 子协议；除非 `OriginPatterns` 显式允许，否则拒绝跨域浏览器请求。
- 在接受任何 CRDT change 前交换并精确比较 `replica.Manifest`。
- 只接受有上限的二进制消息，使用 `replica.NewChangeWithPolicy` 校验其中的规范 delta，
  并通过有界 `replica.Inbox` 保持每个 actor 的连续 delivery frontier。
- 强制应用提供 `Authorize` 回调，把已认证 peer 与其声明的逻辑 actor 绑定。
- 不会再次广播已经安装或已进入 pending 队列的 Dot，从而遏制冲突重试；生产 operation
  store 仍必须持久化绑定每个 actor/counter 与其规范 payload。
- 对每个 peer 使用有界发送队列；跟不上的 peer 会被关闭，不能让广播无限占用内存。

应用提供的 `Apply` 回调仍必须用适合该复制组的限制解码具体 CRDT。外层 frame 的
校验和不能认证来源，也不能校验类型特定 payload。

## 运行完整参考流程

```sh
go run ./examples/websocket-provider
go test -race ./examples/websocket-provider/...
```

命令会启动进程内 HTTP/WebSocket 端点，连接两个 Go 副本，先发送第 2 个 dot、
再发送第 1 个 dot，最后重试第 1 个 dot。预期输出：

```text
relay-value=5
left-value=5
right-value=5
frontier-operator-a=2
duplicate-and-out-of-order-safe=true
```

## 挂载 Handler

先构造应用状态和它的 Manifest。Manifest 必须绑定一个 CRDT 协议、schema、codec、
epoch 与语义版本。生产恢复时，`Frontier` 必须与该 CRDT 状态在同一持久化事务中保存。

```go
group, err := provider.NewGroup(provider.GroupConfig{
	Manifest:          manifest,
	Frontier:          restoredFrontier,
	MaxPendingChanges: 256,
	MaxPendingBytes:   1 << 20,
	Apply: func(encoded []byte) error {
		delta, err := counter.UnmarshalGCounterDeltaWithLimits(encoded, receiveLimits)
		if err != nil {
			return err
		}
		return sharedCounter.ApplyDelta(delta)
	},
})
if err != nil {
	return err
}

handler, err := provider.NewHandler(provider.Config{
	Groups: []*provider.Group{group},
	Authenticate: func(request *http.Request) (provider.Peer, error) {
		// 校验 session/JWT/mTLS 身份；不能信任客户端传来的 actor header。
		return provider.Peer{ID: authenticatedSubject(request)}, nil
	},
	Authorize: func(peer provider.Peer, _ replica.Manifest, dot replica.Dot) error {
		if !actorBelongsToSubject(peer.ID, dot.Actor) {
			return provider.ErrUnauthorized
		}
		return nil
	},
	OriginPatterns: []string{"app.example.com"},
})
if err != nil {
	return err
}
http.Handle("/crdt", handler)
```

示例中的导入路径为：

```go
import provider "github.com/DarkInno/crdt/examples/websocket-provider/provider"
```

该参考实现将 `github.com/coder/websocket` 固定在仓库模块中。v1.8.13 pin 保持了仓库
Go 1.21 的语言最低版本；更新 provider 或提高支持的 Go 版本时，应一并审查该 pin
及其安全状态。

## 从 Go 客户端连接并发布

每个客户端都拥有自己的具体状态和与 Manifest 兼容的 inbox。provider 会把每次新接受的
广播交给 `OnChange`，但不会回显 relay 已知的 Dot；调用 `Publish` 前，调用方必须先应用
本地 CRDT 变更并持久化其 outbox 条目。

```go
inbox, err := replica.NewInbox(manifest, restoredFrontier, 256, 1<<20, applyDelta)
if err != nil {
	return err
}
client, err := provider.Dial(ctx, "wss://sync.example.com/crdt", manifest, provider.ClientConfig{
	Header: http.Header{"Authorization": []string{"Bearer " + accessToken}},
	OnChange: func(change replica.Change) error {
		_, err := inbox.Receive(change)
		return err
	},
})
if err != nil {
	return err
}
defer client.Close()

encoded, err := delta.MarshalBinary()
if err != nil {
	return err
}
change, err := replica.NewChange(manifest, replica.Dot{
	Actor: durableActorID, Counter: nextDurableSequence,
}, encoded)
if err != nil {
	return err
}
if err := client.Publish(ctx, change); err != nil {
	// 需要时从应用拥有的 outbox 持久化并重试。
	return err
}
```

`Actor` 和 `Counter` 是投递标识，而不是 HLC tag。必须把它们与发出的 mutation/outbox
一起分配并持久化；重启后复用内存计数器会与旧 dot 冲突。除非授权回调已验证 actor
属于已认证主体，否则不得接受客户端自行选择的 actor。

## Wire 契约

WebSocket 握手选择 `crdt-sync-v1` 后，客户端先发送一个文本 hello，服务端回复一个：

```json
{"version":1,"manifest":{"GroupID":"...","SchemaID":"...","Epoch":1,"Protocol":{"StateID":1,"DeltaID":2,"CodecID":"","SemanticsVersion":1}}}
```

provider 使用 `Manifest.Compatible` 比较解码后的 Manifest；group、schema、epoch、
codec、frame ID 或语义版本不匹配都会在数据投递前被拒绝。

后续每条消息都为二进制，并使用如下规范 envelope：

```text
1 byte      provider wire version（1）
uvarint     UTF-8 actor 字节长度
bytes       actor
uvarint     非零的每 actor counter
uvarint     规范 CRDT delta frame 字节长度
bytes       规范 CRDT delta frame
```

该 envelope 会拒绝非规范、截断、超限、为空或带尾随字节的编码。它只是传输 envelope，
不能替代库 frame、具体 CRDT decoder、快照格式或成员协议。

## 受控压测证据

重复投递准入和 loopback 端到端扇出的 Linux/amd64 数据记录在
[2026-07-29 压测报告](../operations/benchmark-2026-07-29.zh-CN.md)。每个扇出操作都会等待
所有观察者完成解码和安装；它不代表 WAN 延迟、浏览器、TLS、持久化存储或生产容量。

## 生产使用前必须补齐的工作

在受控集成环境以外使用此模式前，嵌入服务必须实现并验证以下全部边界：

| 边界 | 应用责任 |
| --- | --- |
| 传输安全 | 终止 TLS 并使用 `wss`；明确配置 origin 策略并设置入口限额/超时。 |
| 身份与授权 | 升级前认证；将每个 actor 映射到允许的 subject/group；按需吊销或持续校验长连接。 |
| 持久投递 | 在所需事务中持久化 mutation、actor counter、CRDT state 和 delivery frontier；实现重连、outbox 重试、去重与恢复。 |
| 引导与反熵 | 向新成员或重入成员发送已校验 snapshot/checkpoint，并修复单个在线 WebSocket 会话无法回放的缺口。 |
| Decoder 与留存限制 | 根据真实负载与对抗输入预算设置 transport、frame、CRDT 对象、pending inbox、队列、速率和文档限制。 |
| 成员与 GC | 安装已认证、权威的成员视图；任何墓碑压缩前使用精确且绑定 epoch 的确认。 |
| 运维 | 增加可观测性、过载行为、背压策略、备份、部署/回滚流程和生产故障测试。 |

通过示例或其测试，只能证明内存内参考流程；不能证明浏览器/移动端兼容、真实身份提供方、
TLS 部署、持久化事务、恢复或生产容量。
