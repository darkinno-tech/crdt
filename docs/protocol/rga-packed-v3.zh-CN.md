# Packed RGA v3 线协议

本文定义紧凑 RGA v3：state TypeID `29`、delta TypeID `30`、空 codec ID、语义版本
`3`。它必须通过经过认证的 Manifest 显式协商；scalar RGA v1（`11/12`）和 run-v2
（`19/20`）保持不变，禁止降级、混用或隐式转换。

v3 不改变逻辑 RGA：每个 Unicode scalar 仍有独立的 `(position, parent, rune)`，墓碑、
并发排序、乱序 pending 和删除锚点语义与 run-v2 相同。它只对可精确重建的同副本线性
HLC 链减少编码字节，绝不合并 Position、弱化墓碑，或授权 GC。

## 协商、边界与 API

Manifest 必须绑定 `29/30`、语义版本 `3`、空 codec ID、schema/epoch，以及帧、节点、
墓碑、pending、replica ID 和留存状态上限。TypeID 与 CRC-32C 不能提供认证、授权、
重放防护或加密。

Go 使用 `text.PackedFrameType()` 和 `InsertPackedBinaryWithLimits`（及 delete、replace、
snapshot 对应 API）显式选择 v3。所有本地 mutation 均先按最终帧上限预检；恢复前必须原子
持久化 `{state, HLC state, delivery frontier/outbox}`。

## 编码

外层使用 canonical CRDT frame v1。整数均为最短 uvarint，`bytes` 为长度加精确字节：

```text
payload      = block-count block* tombstone-count tombstone*
node         = 0 tag parent rune
ordinary-run = 1 count replica-id parent (wall logical rune)*
packed-run   = 2 count replica-id parent first-wall first-logical
               transition-bits wall-gap* utf8-text
```

`packed-run` 的 bitset 长度必须是 `ceil((count-1)/8)`；按低位到高位处理每个后继节点：

- bit 为 `0`：保持 wall time，logical 必须加一；
- bit 为 `1`：读取一个正的 `wall-gap`，wall 增加该值，logical 必须归零。

UTF-8 文本必须有效且刚好有 `count` 个 Unicode scalar；第一项使用 block 的 parent，后续项
以刚重建的 Position 为 parent。高位未使用 bit、wall/logical 溢出、重复 tag、无效 UTF-8
或非规范编码都必须拒绝。

若链不是上述可精确重建的 HLC 序列，或 packed 反而更大，编码器必须使用 ordinary-run，
而不是有损压缩。

## 正确性、安全和验证

解码必须先限制完整帧及所有声明的字节/数量、校验 CRC，随后重建所有 Position、检查完整
父图/环/墓碑/资源预算，并将完整图逐字节重新编码。只有重编码结果完全相同的帧才合法。
state 必须父节点完整；delta 可以起于外部 parent，但只可在有界 pending 策略内保存，不能将
未完成状态写成 snapshot。失败不得改变文本、HLC、pending 或持久化状态。

v3 目前由 Go 与 Go/Wasm runtime 实现。原生或 TypeScript 实现未完成同样的有界 decoder、
canonical re-encoder 与向量前，不得宣称支持 `29/30`。它不是 Yjs 协议兼容层；Yjs 仍通过
独立有界的 opaque relay/store 处理。

[`testdata/rga-packed-v3-vectors.json`](testdata/rga-packed-v3-vectors.json) 提供确定性的
密集链、wall 跳变和墓碑向量。实现必须验证外层、重建节点图、逐字节重编码、空文档应用结果，
并对 checksum、TypeID、bitmap、varint 和资源上限变异作原子拒绝。

详细规范与命令以英文版 [rga-packed-v3.md](rga-packed-v3.md) 为准。
