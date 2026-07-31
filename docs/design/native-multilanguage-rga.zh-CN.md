# 原生多语言 RGA 交付决策

## 结论

本轮不把 TypeScript 的 `native-ts-v1` JSON 更新误作 Go RGA 的半成品，也不在
Python、Rust、Swift 中各复制一份尚未验证的合并逻辑。仓库新增完整的 Rust
`rga-run-v2` 原生核心，并通过显式所有权的 C ABI 提供 Python 与 Swift 绑定。
这些绑定已经支持本地编辑、合并、快照与恢复，但应准确称为“共享 Rust 语义核心
的原生 runtime 绑定”，而不是独立 Python/Swift wire 实现。

目标协议固定为 `rga-run-v2`：状态/增量 TypeID 为 `19/20`、语义版本 `2`、codec
为空。它与 scalar RGA v1 (`11/12`) 及 `native-ts-v1` 必须分别协商，禁止混用或
自动降级。

## 多维度取舍

| 维度 | 独立三语言实现 | Rust 核心 + Python/Swift FFI | 本轮选择 |
| --- | --- | --- | --- |
| 正确性 | 三套 canonical 编码、HLC、乱序父节点和 tombstone 需同步修复 | 一套经过向量验证的解码、合并与 HLC 语义 | Rust 核心 + FFI |
| 安全性 | 三个不可信输入解码器与资源控制面 | 一个有界解码器；ABI 只转移拥有的 buffer | Rust 核心 + FFI |
| 性能 | 可能获得各语言最优实现，但前期无基准即有三倍优化面 | 原生 runtime；FFI 成本远小于一次 frame 合并 | Rust 核心 + FFI |
| 维护成本 | 三个发布、测试和兼容矩阵 | 一个 protocol core，语言层仅资源所有权/API | Rust 核心 + FFI |

## 正确性与安全不变量

1. 状态是不可变 `(position, parent, Unicode scalar)` 节点集与 tombstone 集；
   合并是集合并集，同一 position 的冲突内容必须拒绝。
2. 改变文档前校验 envelope、CRC-32C、最短 varint、长度、类型/codec、Unicode
   scalar、重复 ID、环、完整 state 父闭包及重新编码规范性。
3. delta 的缺失父节点只允许进入有界 pending 队列；pending 非空时拒绝生成 state。
4. `{state, HLC, frontier/outbox}` 是一个原子持久化单元；同 ID 重启只恢复 state
   会重复 tag，因而不允许。
5. 限制只用于资源防护，不能代替请求体限制、TLS、认证、授权、manifest 绑定或
   重放策略；CRC 只能发现偶然损坏。

Rust C ABI 使用 opaque mutex handle 与一次性释放的 `crdt_buffer`，不会把 Rust
可变内存借给 Python/Swift，也不会让 panic 越过 FFI 边界。宿主仍负责 handle
生命周期和动态库签名/分发。

## 性能与验证

当前合并采用 copy-on-validate：先验证并构造候选状态，再原子提交。因此被拒绝帧
绝不污染节点、pending、tombstone 或 HLC，但每次非重复合并有 `O(保留状态)` 复制
成本。先用真实编辑/快照/恢复基准判断它是否为瓶颈；只有有证据时才以
copy-on-write 事务计划优化，不能先为性能放松原子性。

- `make rust-test`：直接消费 Go 发布向量，并验证字节级重编码、畸形/超限原子
  拒绝、乱序父节点、重复/乱序三副本、快照+HLC 恢复。
- `make python-test`：真实 Python → C ABI → Rust runtime 的向量、收敛、恢复链路。
- `make swift-test`：macOS 上真实 Swift → C ABI → Rust runtime 对应链路。
- `make rust-benchmark`：受控本地 insert、frame relay、state/recovery 基准；仅是
  回归信号而不是生产容量承诺。

独立 Python 或 Swift 实现的准入门槛不是“能解帧”，而是必须通过相同向量、攻击性
输入原子性、与 Rust/Go 的随机重复/乱序/分区收敛、HLC 恢复和同一基准工作负载。
