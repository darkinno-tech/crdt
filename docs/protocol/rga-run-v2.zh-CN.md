# RGA run-v2 线协议

本文是紧凑 RGA run-v2 的实现说明；精确的规范性措辞、全部字段定义和向量以英文版
[RGA run-v2 wire protocol](rga-run-v2.md) 为准。它使浏览器、移动端和服务端能在不嵌入
Go 或 WebAssembly 的情况下交换协作文本状态和 delta。复用 Go/Wasm runtime 风险最低；原生
实现必须满足英文规范的每一项规则，并通过其[规范向量](testdata/rga-run-v2-vectors.json)。

## 协商边界

在传输任何字节前，经过认证的复制组 Manifest 必须精确绑定：state TypeID `19`、delta TypeID
`20`、空 codec ID、语义版本 `2`、应用自有的 schema ID/epoch，以及输入和留存资源上限。
run-v2 与旧标量 RGA v1（11/12）是两种不同协议，禁止降级或混用。TypeID 与 CRC-32C 不提供
认证、授权、重放防护或加密。

## 帧与 Position

每个消息都是完整的 canonical CRDT 外层帧：

```text
"CRDT" | uvarint(1) | uvarint(type ID) | bytes(codec ID) |
bytes(payload) | big-endian CRC-32C
```

所有整数是最短形式的无符号 LEB128（低七位优先）；任何过长 varint 都必须拒绝。`bytes` 是
`uvarint(length)` 后的定长字节。CRC-32C（Castagnoli）覆盖 magic 后到 payload 末尾的所有字节，
不覆盖 magic 和自身。run-v2 的 codec 字节串必须为空。

每个 Position/Tag 按 `bytes(replica ID) | wall time | logical` 编码。排序顺序为
`wall time`、`logical`、`replica ID` 原始字节字典序，均为升序。live logical replica 的 ID
必须全局唯一；同 ID 恢复前必须原子恢复 HLC 状态。

## Payload 与 canonical 编码

Payload 包含 block 数、若干 node/chain block、墓碑数和墓碑：

```text
node block:  uvarint(0) | node tag | parent flag | [parent tag] | rune
chain block: uvarint(1) | count>=2 | replica ID | parent flag |
             [parent tag] | (wall time | logical | rune) * count
```

chain 第一项使用给出的 parent，后续项的 parent 必须是 chain 的前一项。state（19）必须是
无环、父节点完整的图；delta（20）可引用尚未到达的初始 parent，但接收方只能在明确的待处理
节点/字节上限内保存它，且不得把未完成状态序列化为 snapshot。

解码后必须按英文规范第 5 节重新构造 block 并逐字节重新编码。只有重新编码后完全相同的输入
才合法；这会拒绝 map 遍历顺序、不同 block 切分、非规范 tag 顺序和过长 varint。

## 语义、安全与验证

逻辑状态是不可变 `(position, parent, rune)` 节点集合和墓碑集合；两者均按集合并集合并。墓碑可
先于插入到达，随后插入仍必须隐藏。可见文本从 synthetic root 作深度优先遍历，同一 parent 的
child 按 tag 降序访问；被删除节点不输出 rune，但仍遍历子节点。因此墓碑仍是结构锚点，不能仅凭
此线协议回收。

所有解码和应用都必须原子完成：先限制输入、校验外层/CRC、Manifest、完整图、canonical 编码和
留存预算，之后才修改文本、HLC、pending queue 或持久化。建议至少限制传输体、帧/payload、ID、
节点/墓碑、pending 节点/字节、留存节点/墓碑和本地编辑大小。Go/Wasm 默认使用 1 MiB 帧、
100,000 tags、64 KiB replica ID、10,000 pending nodes 和 512 KiB pending bytes；应用可选择
更低的兼容值。

持久化单元必须是 `{state frame, HLC clock state, frontier/outbox position}`。回收墓碑还需要权威
成员 epoch、每个 tag 的精确确认、回收后持久 checkpoint 和旧 epoch 帧退役。

向量文件中的 hex 帧和元数据是跨语言契约；64 位值在 JSON 中以字符串表示。至少应验证外层、
展开后的节点/parent/rune/墓碑、逐字节重编码和空文档应用后的文本；并对 checksum、varint、
TypeID 和超限输入做原子拒绝测试。仓库通过 `go test ./text`、`make typescript-test`、
`make wasm-test`（真实 Go/Wasm 三副本会话）持续验证。
