# 原生客户端 CRDT 类型覆盖决策

## 结论

Rust/Python/Swift/C++ 共享一个 Rust 语义核心和显式所有权 C ABI，不是四套独立
CRDT 实现。一个通用 `void *` 帧句柄不能把所有已注册 TypeID 安全地标记为“已支持”。

本次完成 `lww-map-v1` 原生纵向切片：空 codec、TypeID `9/10`、语义版本 `1`，与
现有 `rga-run-v2`（TypeID `19/20`）并列。它不表示全部稳定 TypeID 对已原生覆盖。

## 成熟度矩阵

| 原生状态 | 协议 | 结论 |
| --- | --- | --- |
| 已与 Go wire 互通 | LWW-Map 9/10、run-v2 RGA 19/20 | 有规范、Go 向量、有界 Rust merge/恢复和四语言真实调用链。 |
| 需独立实现 | G/PN Counter、OR-Set、LWW-Set、G-Set、MV-Register | actor/value codec、溢出、add/remove 观察、多值投影和墓碑不能退化为 Map。 |
| 必须协议专用 | scalar RGA、OR-Tree、List RGA、Rich Text、MoveRGA、Document Tree、packed RGA | 元素、移动、编辑器 schema 或递归声明需精确语义。 |
| 非原生 CRDT 类型 | attachment、awareness、Yjs relay | 分别是应用校验、临时会话状态或不透明 relay。 |

## LWW-Map 的边界

`LwwMap` 只接收空 codec 的 TypeID 9 完整状态和 TypeID 10 delta。修改状态/HLC 前会
拒绝 checksum/type/codec、非最短 varint、UTF-8/空白 key 或 replica、无序/重复 key、跨
key 重复 tag、同 tag 冲突、尾随字节和协商限制违规。最大 tag 胜出，删除保留为墓碑。

持久化单元必须是 `{state frame, HLC state, 应用 frontier/outbox}`。CRC、frontier、
HLC 最大值和本绑定均不提供认证、授权或墓碑删除许可。宿主仍负责 body limit、身份、
文档/schema/epoch 授权、重放策略、TLS/存储和 opaque value 校验。`keys` 仅是 FFI
读取列表，不是 wire 或持久化格式。

## 后续准入条件

每一个新 TypeID 对都需要规范和 Go 向量、有界 Rust codec/merge、拒绝原子性、乱序/
重复/分区收敛、恢复、四语言真调用链、性能证据，以及 manifest/授权/持久化/墓碑文档。
只有“能解帧”不构成客户端支持。
