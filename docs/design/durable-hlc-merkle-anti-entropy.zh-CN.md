# Durable HLC/Merkle 反熵

## 决策

`crdt-durable-v3` 是可选的无 state vector 恢复协议。它要求 durable Log 实现
`durable.MerkleLog`，并显式持久化 relay HLC；随库提供的 bbolt Store 通过稳定的
`StoreConfig.HLCReplicaID` 启用：

```go
store, err := durable.OpenStore("/srv/crdt/relay.db", durable.StoreConfig{
	MaxEvents:    100_000,
	MaxBytes:     256 << 20,
	HLCReplicaID: "relay-eu-1", // 对此 store 文件稳定且唯一
})
```

relay 在同一 bbolt 事务内写入事件 sequence、Dot 绑定、容量记账、HLC→sequence
索引、下一 HLC 状态和新 relay HLC tag。该 tag 只标识 durable relay 事件；它不改变
CRDT merge 语义，也不替代 HLC CRDT delta 内部的 tag/因果信息。

稳定的 cursor 协议 `crdt-durable-v1` 及已有可选 state-vector 协议
`crdt-durable-v2` 继续兼容。历史事件若写入于 relay HLC 持久化之前，不能安全地声称
拥有完整 v3 历史；该 group 必须走 v1/v2 或从已验证 checkpoint 重启。

## 两阶段、无 state vector 同步

```text
本地持久事件清单 root ── merkle hello(root) ──> relay
                                                │ 鉴权 + 订阅授权
                           快照 HLC/root/高水位 H，并在 group 锁内注册 peer
                                                │
client <──────── merkle welcome(root,H) ───────┘
  │ root 相同：持久化边界后直接开始 live H+1 …
  │ root 不同：
  ├─ 收完整且有上限的远端叶清单
  ├─ 对比持久化清单；本地独有叶或同 HLC 不同 digest 必须拒绝
  ├─ 只请求本地缺失的、有序 relay-HLC identity
  ├─ 安装返回事件并重建本地 root
  └─ 校验 root，原子持久化 boundary(root,H)，再开始 live H+1 …
```

relay 在锁内快照并注册 peer；`H` 后的 append 先进入该 peer 的有界队列，只有
`complete(root,H)` 之后才发送。因此 repair 与 live 之间没有空隙。客户端仅在
`OnMerkleCatchUp` 成功后将 cursor 前进到 `H`。

`MerkleIndex` 只是进程内的叶比对辅助，绝不是持久化层。宿主必须把等价 inventory
与具体 CRDT state、delivery frontier/outbox 及本地 CRDT HLC state 一起持久化；重连前
应从持久事件 inventory 重建 index。

```go
client, err := durable.NewReconnectClient(endpoint, manifest, durable.ClientConfig{
	MerkleRoot:      persistedIndex.Root,
	ReconcileMerkle: persistedIndex.Reconcile,
	OnEvent: func(event durable.Event) error {
		// 同一应用事务：验证/应用 delta，持久化 state/frontier 与 relay-HLC 叶；
		// 事务成功后才更新内存 MerkleIndex mirror。
		return installAndPersist(event)
	},
	OnMerkleCatchUp: checkpointMerkleBoundary,
})
```

同一 ClientConfig 不可同时设置 `StateVector` 与三个 Merkle 回调。配置 Merkle 的 client
只提供 v3；服务端未选择 v3 时必须 fail closed，不能静默降级为 cursor 恢复证明。若选择
v3 后无法返回完整有界 inventory，则以 `ErrAntiEntropyUnavailable` fail closed，绝不返回
部分 repair。v1/v2 client 继续走各自既有的兼容路径。

## 正确性、安全与资源边界

- root 是 `relay HLC identity + canonical durable change envelope` 的 SHA-256 承诺，
  只用于发现不一致；它不是身份认证、成员关系证明、持久回执、授权或 tombstone-GC 许可。
- 返回 inventory 前必须完成认证和 `AuthorizeSubscription`；写入授权仍将认证 peer 与
  CRDT Dot 绑定。TLS、租户隔离、限流和保留策略仍属于宿主。
- 所有 control 遵守现有 16 KiB 上限和严格 JSON；inventory/request 分块，并在分配前
  对总 `MaxMerkleLeaves`/`MaxMerkleBytes` 校验。event/byte、actor、queue、整消息上限仍生效。
- relay 在允许 v3 时预留 HLC envelope，避免保存一个合法 v1 但加上 HLC 后无法发送的事件。
- 本地有远端没有的叶、或同 HLC identity 有不同 digest 时返回 `ErrMerkleDiverged`；只能
  调查或 checkpoint bootstrap，绝不能为 root 相等而删除历史。
- 相同 inventory 仅说明确定性 CRDT replay 输入相同。应用 checkpoint 验证、解码资源边界、
  鉴权与完整 durable transaction 仍不可省略。

## 性能模型与验证

root 相等时网络只传 hello、welcome、complete，不传 actor 数量级 state vector 或历史
payload。root 不同则当前可审计基线交换完整有界 flat inventory，只传缺失事件。其网络
inventory 和内存对比均为 `O(N)`；bbolt 参考实现从已验证 retained events 重建 root，当前
首次实现的本地 root 构造也为 `O(N log N)`。

这不表示 v3 在所有 group 都优于紧凑 state vector。应实测 actor 基数、retained event
数、失配率、事件大小、磁盘延迟与 RTT；只有证明 flat inventory 是瓶颈后再加入 paged
subtree/multiproof。

| 维度 | 证据 |
| --- | --- |
| 正确性 | relay HLC 重启原子性、root/inventory 重建、缺失历史 repair 后 live、root 校验后才持久化边界。 |
| 安全/资源 | 严格 v3 control/event 解码、canonical 排序、分块/request 限制、HLC envelope 预留及 fail-closed 冲突测试。 |
| 并发/恢复 | group 锁内注册、client checkpoint boundary、`go test -race ./durable`。 |
| 模拟/真实性 | 三副本分区恢复既有测试，以及真实 WebSocket loopback 缺失历史 repair。 |
| 性能 | `BenchmarkMerkleInventoryReconcile`（256/4096、相等/稀疏）及 `BenchmarkMerkleLoopbackSession`（真实 bbolt + WebSocket）。 |

```sh
(cd durable && go test .)
(cd durable && go test -race .)
(cd durable && go test -run='^$' -fuzz=FuzzWire -fuzztime=250000x -parallel=1 .)
(cd durable && go test -run='^$' -bench='Merkle' -benchmem -count=3 -cpu=1,4 .)
```

以上仅是受控本地证据，不证明生产 TLS/身份、浏览器/移动端持久化事务、WAN 丢包、外部 store
配置或生产容量。
