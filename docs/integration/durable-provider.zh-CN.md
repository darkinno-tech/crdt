# 可持久化 WebSocket relay 参考实现

`durable` 是 [`extensions`](extensions.zh-CN.md) 有界 live relay 之外的生产化传输参考：它提供持久化操作日志、有序重放与断线重连。它的部署边界是**一个进程持有一个持久卷**，不是多节点复制服务。

启用前请先阅读[设计与运行边界](../design/durable-transport.md)。特别是，只有在应用已将具体 CRDT 状态和投递 frontier 与游标放入同一持久事务后，重放游标才有效。

## 传输保证

```text
客户端本地状态和 outbox --发布--> 校验/授权 --> 持久 append
                                           |           |
                                           |           +--> 重放日志
                                           v
                                      live peer 队列
                                           |
                              持久化本地状态 + cursor
                                           |
                              从持久 cursor 断线重连
```

服务端对每个已接纳的 `(group, actor, counter)` 保存不可变的规范字节绑定。相同字节的重试返回原 server sequence；同 Dot 但不同字节会永久拒绝。只有 append 事务提交后才广播，因此提交后、广播前宕机只会导致重放，不会静默丢失。

## 挂载 group

应用提供 Manifest、认证、独立的读/写授权和具体 CRDT 的有界解码器。`Validate` 只能校验，不能修改应用状态。

```go
store, err := durable.OpenStore("/var/lib/crdt/relay.db", durable.StoreConfig{
	MaxEvents: 1_000_000,
	MaxBytes:  4 << 30,
})
if err != nil {
	return err
}

group, err := durable.NewGroup(durable.GroupConfig{
	Manifest: manifest,
	Validate: func(encoded []byte) error {
		_, err := counter.UnmarshalGCounterDeltaWithLimits(encoded, receiveLimits)
		return err
	},
})
if err != nil {
	return err
}

handler, err := durable.NewHandler(durable.Config{
	Store:  store,
	Groups: []*durable.Group{group},
	Authenticate: authenticateSession,
	Authorize:     authorizeActor,
	AuthorizeSubscription: authorizeRead,
})
if err != nil {
	return err
}
mux.Handle("/crdt/durable/", http.StripPrefix("/crdt/durable", handler))
```

服务端通过 `GET /ws` 暴露 `crdt-durable-v1` 子协议。`bbolt` 会对文件加独占锁，但部署仍必须保证一个持久卷只有一个 active pod/process；不要把同一文件挂到多副本。高可用需要替换为保留相同事务语义的存储实现。
`MaxEvents` 与 `MaxBytes` 对每个 replication group 生效；多租户服务还需要在此前增加固定的每租户 group 配额。

## 持久接收与重连

`ReconnectClient` 使用指数退避，并从 `Cursor()` 返回的 sequence 重放。`OnEvent` 必须先通过 manifest 兼容的 `replica.Inbox` 安装事件，再将 CRDT 状态、inbox frontier、outbox 和 `event.Sequence` 在同一个应用事务中提交。提交失败必须返回 error；客户端不会推进 cursor，重连后会再次收到同一事件。

当缺失历史超过服务端窗口，客户端返回 `ErrReplayUnavailable`。此时必须从经过校验的 checkpoint bootstrap；不能重置 cursor 或接收截断的流。

## 验证

```sh
go test ./durable
go test -race ./durable
go test -run='^$' -fuzz=FuzzWire -fuzztime=10s ./durable
go test -run='^$' -bench='Benchmark(DurableAppend|DurableReplay|Reconnect)' -benchmem ./durable
```

测试覆盖真实 loopback WebSocket、重启/重放、强制断线重连、重复/乱序模拟及本地文件存储。它们不证明线上 TLS、外部身份提供方、多节点存储、客户端 checkpoint 事务或墓碑 GC 策略。
