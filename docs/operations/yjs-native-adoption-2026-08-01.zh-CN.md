# Yjs 原生接入评估 — 2026-08-01

## 结论

协作能力明确分为两条互不伪装兼容的路径：

1. **Yjs 原生文档**：浏览器使用 `Y.Doc`、维护中的编辑器 binding 和标准
   `y-websocket` provider，经 `extensions.YJSHandler` 接入；不下载 Go/Wasm，也不协商
   Go manifest。
2. **Go CRDT 文档**：继续使用 manifest-bound frame、Go/Wasm RGA、Go 的持久化恢复和
   编辑器 binding。

两条路径可共用认证网关、观测和产品入口，但不得交换实时 update、client ID、cursor、墓碑或
持久化记录。路径迁移只能在停止写入后以冻结快照做单向导入，目标必须是新的身份与 epoch。

这直接降低浏览器侧交付复杂度：主流编辑器/provider 采用 Yjs 时可以走原生 JS 生态；需要本库
精确 Go 协议与恢复边界时，则端到端保留 Go CRDT。

## 多维度评估

| 维度 | Yjs 原生路径 | Go CRDT 路径 | 决策规则 |
| --- | --- | --- | --- |
| 浏览器与编辑器交付 | 原生 JS，可直接复用 `y-*` binding/provider；没有 Wasm 启动和 Go manifest。 | 保留本库精确的 RGA 语义，但需要 runtime 与协议兼容工作。 | 主流编辑器接入和 provider 复用优先选 Yjs。 |
| 正确性 | state vector、update、shared type 与编辑器 schema 均在一个 Yjs engine 内。 | frame、manifest、HLC、run-v2 均在一个 Go engine 内。 | 绝不在两个 engine 之间翻译实时 mutation。 |
| 安全与资源 | 网关在升级前认证，分别授权读/写/presence，校验 origin，关闭压缩，并限制消息、队列与 sidecar 输入。 | 相同应用控制加上 Go manifest 授权。 | 不从 Yjs client ID 认证；使用 cookie 或独立设计的短期 ticket。 |
| 性能与容量 | 本地编辑与 provider fan-out 由 Yjs 完成；耐久 state-vector 恢复增加一次有界 sidecar 调用。 | Go 文档不需要 Node sidecar；仍有 Wasm 下载和 frame codec 成本。 | 以真实文档大小和接收者数测量，不能从 codec microbenchmark 推导容量。 |
| 运维与恢复 | 当前 store 仅 loopback、单进程/单数据目录；HA 需要按文档分片或可跨进程串行化的 store。 | 只适用于 Go 文档的既有恢复流程。 | 无重连恢复需求时用 Level 0；确有恢复负载才启用 Level 1 store。 |

## 浏览器最短安全接入

```ts
const document = new Y.Doc();
const provider = new WebsocketProvider("wss://collab.example/yjs", "notes", document);
```

这仍是原生 Yjs document。CodeMirror plain-text 可选用
[`@im10furry/crdt-client/yjs` binding](../integration/yjs-native-editor-bindings.md) 获得有界增量投影，
但不会引入 Go frame、Go/Wasm 或 `native-ts-v1`；其他编辑器直接使用其维护中的 Yjs binding。
生产第一要求是同源的 Secure、HttpOnly session cookie：浏览器 WebSocket API 不支持任意
`Authorization` header，不要把长期 bearer token 放入 provider query string。应用仍必须预配置
room，在网关绑定 user/tenant/document 权限，并在撤权时关闭已有连接。

`YJSStore` 不面向浏览器；其 bearer token 只能用于受信 Go 到 loopback sidecar。V1/V2 必须按
耐久文档固定；schema label 只用于 identity fencing，不能声称已校验任意 ProseMirror、Quill 或
自定义 schema。

## 验证矩阵

| 场景 | 已验证内容 | 命令 |
| --- | --- | --- |
| 真实 sidecar V1/V2、merge、重启、畸形输入 | 维护中的 Yjs engine 校验格式、生成 snapshot，拒绝输入不覆盖最后有效记录。 | `make yjs-store-test` |
| 标准 `y-websocket` → Go relay → Node store | 原生 client handshake、离线并发文本/嵌套类型收敛、awareness，以及全新 client 的耐久恢复；`disableBc` 排除同进程 BroadcastChannel 伪阳性。 | `make yjs-store-test` |
| Go relay wire fuzz/race/capacity | 畸形外层、重复/乱序 opaque update、awareness 所有权、队列与内存 fan-out。 | `go test -race ./extensions -run 'TestYJS'` |
| 1/4/16/64 receivers 的受控 loopback store 压测 | 有界模拟接收者下 apply/diff/snapshot 延迟与恢复字节；不代表 WAN、TLS 或生产 fan-out 容量。 | `make yjs-store-benchmark` |

上线前需在目标 CPU、Node 内存限制、真实文档、认证网关、TLS 终止以及 1/4/16/64 个真实浏览器
client 上重测，记录 reconnect/apply p50/p95/p99、CPU、heap、queue drop、sidecar 错误和恢复字节
分布。本地通过不等同于生产网络的 provider 流量验收。

## 受控开发环境测量

2026-08-01 在 Apple M4 Pro、Node v26.5.0 上执行；场景为 4 KiB 初始 `Y.Text`、5 次 warmup
后 40 次增量编辑、loopback HTTP，以及 store 的 1 MiB update / 16 MiB snapshot 限制：

| 模拟接收者 | Apply p50 / p95（ms） | Diff p50 / p95（ms） | Snapshot p50 / p95（ms） | 平均 diff 字节 |
| --- | --- | --- | --- | --- |
| 1 | 10.267 / 11.203 | 1.933 / 2.189 | 1.818 / 2.070 | 22.875 |
| 4 | 10.084 / 11.183 | 1.781 / 2.041 | 1.787 / 1.918 | 22.875 |
| 16 | 9.672 / 10.836 | 1.691 / 1.931 | 1.711 / 1.953 | 20.875 |
| 64 | 10.547 / 19.722 | 1.646 / 2.224 | 1.691 / 2.266 | 22.875 |

Go relay 的本地 opaque-wrapper decode 与重复准入 benchmark 在单逻辑处理器下连续运行三次，
得到 115.0–118.1 ns/op、136 B/op、3 allocs/op。上述数据不包含 TLS、浏览器渲染、认证数据库、
WAN 或实际 WebSocket fan-out，只能作为开发基线，不能作为生产容量结论。

## 非目标

- 不实现 Yjs ↔ Go RGA、rich-text、awareness 或 cursor 的实时转换。
- 不将 checksum、state vector、WebSocket write 或 provider `sync` 误称为用户认证、对端确认或
  业务事务。
- 不持久化 Yjs awareness，也不用 Level 0 opaque history 充当耐久恢复日志。
