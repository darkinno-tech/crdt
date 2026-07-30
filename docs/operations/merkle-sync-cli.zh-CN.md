# Merkle 状态修复 CLI 手册

`crdt-merkle-sync` 是一个有边界、需要认证、离线运行的修复工具，面向只存放稳定
G-Counter 状态帧的专用目录。它把既有的 Merkle 摘要串成一次完整的反熵修复：先比较根；
不同才获取清单；只拉取缺失或不同的状态；两端使用 G-Counter `Merge`；最后再次比较根。

它刻意不是“只要 frame 合法就复制”的通用工具。帧外层和 SHA-256 摘要不能提供其他
CRDT 所需的语义合并、应用 codec、HLC 恢复、墓碑留存、认证 Manifest 或持久事务边界。

## 多维度决策

| 维度 | 决策与依据 |
| --- | --- |
| 正确性 | 仅接收稳定 G-Counter 状态帧（TypeID 1）。每个落盘和接收状态均由 `counter.UnmarshalBinaryWithLimits` 校验，G-Counter `Merge` 提供交换、结合、幂等的 join。未知、delta 与实验状态全部 fail-closed。 |
| 完整性 | 清单携带每个状态的 SHA-256；客户端下载后会校验 body 和响应摘要，再比较修复后的完整 Merkle 根。CRC-32C/frame 校验只是输入校验，不是对端认证。 |
| 资源安全 | 默认限制为单状态 1 MiB、1,024 文件、65,536 个 counter 分量、1,024 字节副本 ID、128 字节扁平 key；文件数硬上限为 4,096。限制在解码分配和状态修改前生效。 |
| 安全 | 默认仅监听 loopback，必须携带非空共享 token，并以常数时间比较哈希后的 token。优先使用 `-token-file`；非 loopback 必须显式开启，工具本身不终止 TLS。 |
| 性能 | 根相等时使用缓存的 `merkle.Tree.Root()`，不会请求清单。根不同时只传输一份有边界清单以及缺失/不同的帧，不传输相同帧。 |
| 运维 | 使用临时文件、文件 sync、rename、目录 sync 写入。最终根不一致即修复失败，不会伪报成功；写入并发时需待其稳定后重试。 |

Merkle 根不能被当作确认、成员资格证明或授权依据；它不能授权墓碑回收，也不能让离线成员安全地重新加入。

## 状态目录契约

一个目录同一时刻只能被一个工具进程拥有。不得让 `serve`、`sync`、`gcounter-add`
同时打开同一个目录：长生命周期的 server 持有已验证的内存视图。另一个进程打开目录前先停止
server，之后如有需要再启动。

每个状态文件必须是常规平坦文件 `<key>.frame`。key 只允许 ASCII 字母、数字、`.`、`_`、`-`；
路径、嵌套目录、symlink、空状态及无关文件都会被拒绝。目录必须专供本工具使用，并接受与应用状态
相同的账户和卷权限保护。

当前只支持规范化 G-Counter 状态帧。未来新增类型时，必须先具备具体的有边界解码器以及已文档化的
恢复/合并生命周期，不能只因 `crdt.ProtocolPolicy` 暴露了 TypeID 就加入。

## HTTP 契约

所有路径都要求 `X-CRDT-Merkle-Token`，响应均带 `Cache-Control: no-store`。

| 方法与路径 | 含义 |
| --- | --- |
| `GET /v1/merkle/root` | 版本、缓存根与状态数。 |
| `GET /v1/merkle/inventory` | 根不同后返回有序 `{key, sha256, type_id}` 清单。 |
| `GET /v1/state/{key}` | 返回一个状态帧，并携带 `X-CRDT-State-SHA256`。 |
| `PUT /v1/state/{key}` | 校验并合并一个入站状态；成功返回 `204`。 |

客户端会拒绝版本、排序、key、摘要或类型不合法的清单，也会拒绝与清单摘要/类型不符的状态 body。
若清单发现期间根发生变化，会在写入前返回可重试错误；合并后根仍不同也会返回错误，通常意味着
并发写入或远端未完成兼容的 join。

## 本地端到端演练

```sh
go build -o ./dist/crdt-merkle-sync ./cmd/crdt-merkle-sync
umask 077
openssl rand -hex 32 > ./merkle.token
chmod 600 ./merkle.token
mkdir -m 700 ./state-a ./state-b

./dist/crdt-merkle-sync -mode gcounter-add -state-dir ./state-a \
  -key orders -replica web-a -amount 2
./dist/crdt-merkle-sync -mode gcounter-add -state-dir ./state-b \
  -key orders -replica warehouse-b -amount 3
./dist/crdt-merkle-sync -mode gcounter-add -state-dir ./state-b \
  -key returns -replica warehouse-b -amount 1
```

先启动受控接收端：

```sh
./dist/crdt-merkle-sync -mode serve -state-dir ./state-b \
  -listen 127.0.0.1:49821 -token-file ./merkle.token
```

在另一个终端修复 A：

```sh
./dist/crdt-merkle-sync -mode sync -state-dir ./state-a \
  -target http://127.0.0.1:49821 -token-file ./merkle.token
```

JSON 的 `final_root` 只会在两端根相等后输出。再次执行同一命令，`already_equal` 应为 `true`。
在另一个工具进程打开 `./state-b` 前先停止接收端。三副本分区演练可依次修复 A↔B、停止 B 后
修复 B↔C、停止 C 后启动 A 并修复 C↔A；每一步均为 join 而非覆盖，循环结束后三端收敛。

跨主机时应交叉编译后核验 SHA-256、将目录/token 保持为 `0700`/`0600`，并使用私网或有访问控制的
TLS 终止代理。裸非 loopback 监听需要 `-allow-non-loopback`；bearer token 不会加密流量或证明身份。

## 验证与性能测量

命令包覆盖端到端缺失/分歧状态修复、三副本分区恢复、未认证/超限/路径/摘要负向路径、并发客户端，
以及 loopback HTTP + 临时文件系统上的同根与稀疏修复基准：

```sh
go test ./cmd/crdt-merkle-sync
go test -race ./cmd/crdt-merkle-sync
go vet ./cmd/crdt-merkle-sync
go test -run '^$' -bench '^BenchmarkSynchronize(SameRoot|SparseRepair)$' \
  -benchmem -count=3 ./cmd/crdt-merkle-sync
```

对于 `N` 个对象、`K` 个差异对象，同根路径是缓存根与一次 HTTP 往返；不同时传输 `O(N)` 清单元数据和
`O(K)` 状态/合并。首版在对象上限内保持扁平清单，未引入子树或 multiproof 分页，以降低审计复杂度。
提高限制或引入证明协议前，应先测量真实状态大小、分量数、修复频率、磁盘 sync 延迟与网络 RTT。

这些测试只证明当前版本的有边界 G-Counter 修复流；不证明生产 TLS、身份提供方、应用 checkpoint
事务、实时多进程状态所有权或墓碑 GC 策略。
