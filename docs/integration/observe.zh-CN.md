# 应用层变更观察

`observe.Store` 是单个 CRDT 面向应用层的响应式边界：它串行化所有经由它
提交的变更，并在每次成功变更后发布类型化、不可变的 UI 投影。它刻意不负责
CRDT 协议、持久化重放、传输、认证或存储。

当浏览器、移动端、桌面视图或服务投影需要响应本地编辑、已安装的远端 delta、
合并、恢复或获授权维护时使用它。不要把 `observe.Event.Version` 发送给对端：
它只是进程内 UI 版本，不是因果时钟或持久化确认。

## 分布式 Counter 观察器

`NewGCounterObserver` 和 `NewPNCounterObserver` 把现有 Counter delta API
接入这一层本地观察边界。本地 `Increment` / `Decrement` 返回供已认证传输使用的
规范 delta；接收端仍按 Counter 的既有限额解码，再调用 `ApplyDelta`。

```go
model, err := observe.NewGCounterObserver("browser-tab")
if err != nil { /* handle */ }

subscription, err := model.Subscribe(func(event observe.Event[observe.GCounterView]) {
	if event.Value.Overflow {
		showAggregateOverflow()
		return
	}
	renderCounter(event.Value.Value)
})
if err != nil { /* handle */ }
defer subscription.Unsubscribe()

delta, err := model.Increment(1)
if err != nil { /* handle */ }
encoded, err := delta.MarshalBinary() // 在对象外认证并发送
if err != nil { /* handle */ }

received, err := counter.UnmarshalGCounterDelta(encoded)
if err != nil { /* reject */ }
changed, err := model.ApplyDelta(received)
if err != nil { /* reject */ }
// 重复或已被包含的 delta 会返回 changed == false，且不会产生新的 Remote UI 版本。
_ = changed
```

`PNCounterView.Value` 是十进制字符串，因此回调不会接触可变 `big.Int`，也不会
丢失合法 `uint64` 分量的数值范围。这些 helper 不增加网络、认证、确认、持久化或
恢复契约；主机仍负责 frame 准入、持久化 outbox/state 安装，以及 replica/manifest
协议边界。

## 将计数器绑定到界面

视图函数在 `Store` 串行化操作期间运行，必须返回可安全保留的值。标量和字符串
天然安全；map、slice、`[]byte` 和指针必须返回自有副本。所有回调共享同一投影，
并且都必须把 `Event.Value` 当作不可变值。

```go
value, err := counter.NewGCounter("browser-tab")
if err != nil { /* 处理错误 */ }

model, err := observe.New(value, func(current *counter.GCounter) uint64 {
	total, err := current.Value()
	if err != nil {
		panic(err) // 请替换为应用自己的不变量处理策略。
	}
	return total
})
if err != nil { /* 处理错误 */ }

subscription, err := model.Subscribe(func(event observe.Event[uint64]) {
	// 若框架要求 UI 线程，请在这里切换到对应 dispatcher。
	renderCounter(event.Value)
	if event.Coalesced != 0 {
		metrics.Record("crdt_ui_events_coalesced", event.Coalesced)
	}
})
if err != nil { /* 处理错误 */ }
defer subscription.Unsubscribe()

if err := model.Mutate(observe.Local, func(current *counter.GCounter) error {
	_, err := current.Increment(1)
	return err
}); err != nil { /* 处理错误 */ }
```

`Subscribe` 会原子地入队 `Origin == observe.Initial`，消除“读取当前状态”和
“开始订阅”之间漏通知的窗口。若回调尚未来得及运行便产生新变更，初始事件可以被
合并为包含最新完整投影的后续事件。只有已经获得一致 `Snapshot` 的调用方才应使用
`SubscribeFromNow`。

## 安装远端 delta

所有会改变状态的路径都必须经过同一个 Store。CRDT 仍负责 delta 幂等性与冲突
语义；`observe` 只提供本地视图版本和通知顺序。

```go
if err := model.Mutate(observe.Remote, func(current *counter.GCounter) error {
	return current.ApplyDelta(receivedDelta)
}); err != nil {
	// 无效 delta 被拒绝时不会发布事件。
	return err
}
```

合并、恢复和维护操作分别使用 `observe.Merge`、`observe.Restore`、
`observe.Maintenance`。通过通用 `Mutate` 成功安装的重复 delta 仍可能发出
`Remote` 事件，因为它不能判断所有 CRDT 的语义变化。`MutateIf` 和 Counter
观察器可以在类型本身提供 changed 结果时抑制这类冗余版本。

## 投递与生命周期契约

| 关注点 | 契约 |
| --- | --- |
| 顺序 | 每个订阅者看到的 Store 版本严格递增；但被替换的中间版本可以跳过。 |
| 背压 | 每个订阅者仅保留一个待投递槽位。慢回调得到带 `Coalesced > 0` 的最新状态，变更调用方不等待回调。 |
| 重入 | 回调在 Store 和 CRDT 锁释放后执行，因而可以调用后续 `Mutate`。变更闭包本身不能递归调用同一 Store。 |
| 回调失败 | 回调 panic 会被恢复并记录在 `Subscription.Panic`；只停止该订阅。`Options.OnPanic` 仅用于诊断。 |
| 关闭 | `Unsubscribe` 可重复调用；`Done` 在进行中的回调退出后关闭。`Store.Close` 拒绝之后的变更/订阅并取消所有监听器。 |
| 状态所有权 | CRDT 交给 Store 后不能绕开 Store 直接修改；读取用 `Snapshot`，更新一律使用 `Mutate`。 |

`Coalesced` 适合重绘指标和缓存失效，不是可靠操作流的替代品。业务若必须处理每
一个中间操作，应使用带认证与边界的传输，并遵循现有 `replica`/`durable` 恢复
契约。

## 安全与性能边界

- `observe` 不增加 frame 类型、网络端点、每次变更创建的 goroutine 或持久化；
  它不能认证对端，也不能让 tombstone GC 自动安全。
- 每个活跃订阅只有一个 dispatcher goroutine 和最多一个待投递事件；内存是
  O(订阅数)，不是 O(变更数)。
- 发布需要遍历活跃订阅，扇出成本为 O(订阅数)。没有订阅时，`Mutate` 只推进
  版本，不调用视图函数也不分配事件。
- 视图投影必须短小且复制有界。不能因为发生了状态变更就向不可信 UI 回调暴露
  CRDT 中的敏感载荷。

## 验证

```sh
go test ./observe -count=1
go test -race ./observe -count=1
GOMAXPROCS=1 go test -run '^$' -bench 'BenchmarkGCounterBinding' -benchmem ./observe
GOMAXPROCS=1 go test -run '^$' -bench 'BenchmarkGCounterObserverRemoteApply' -benchmem ./observe
```

包内测试覆盖初始投递、错误、慢订阅者合并、回调重入、回调 panic、关闭、并发
版本，以及包含重复/乱序远端 delta 的三副本 G-Counter 分区场景。
