# Formal verification 范围与路线

darkinno-tech 已新增可检查的 Lean 模型：[`formal/rga`](../../formal/rga)。首个目标选择风险
最高的 RGA 不变量：结构 tombstone 必须跨乱序投递保留，state merge 不能悄悄遗忘它。

初始模型已证明 delta join 的幂等、交换、结合，tombstone 单调性，delete-before-insert
不复活，以及有限 duplicate/reordered 交付收敛。本次扩展还检查严格的 sibling order relation、
解析后 run-v2 envelope refinement，以及原子 HLC/snapshot recovery state
machine。它们明确只是**抽象模型**，不是 Go 实现的证明；精确 checked command 和边界见
Lean 源码旁的 README。

## 为什么先证明这一层

仓库同时有 stable framed 和 local-only API。忽略这些边界的证明
会制造虚假的安全感，因此首个 formal surface 只覆盖 stable RGA state algebra；parser
limits、认证、relay policy、checkpoint durability 和 GC acknowledgement 都仍是显式外部
假设。

| 层 | 当前证据 | Formal 状态 |
| --- | --- | --- |
| Delta set/tombstone merge | Lean `Delta.lean` | 已机器检查的抽象证明 |
| RGA sibling order | 单测、fuzz、shuffled simulation | Lean strict-order proof；parent-index 和 pending-parent refinement 仍待完成 |
| run-v2 frame codec/limits | decoder tests/fuzz | Lean parsed-envelope refinement；byte parser、CRC 和 limits 仍待完成 |
| HLC/snapshot recovery | recovery/race tests | Lean atomic-record state machine；Go/provider durability refinement 仍待完成 |
| structural tombstone compaction | exact-ack/checkpoint tests | 待 epoch/receipt proof |
| provider authorization/awareness | 认证集成测试 | 属安全策略，不是 CRDT algebra |

`lean-yjs` 是重要先例：Yjs 文档称其已有 preservation/commutativity proof，而社区说明
工作仍正从 algorithmic model 走向更贴近实现的验证。darkinno-tech 同样只发布已完成 theorem
和 abstraction boundary，绝不笼统声称“整个实现已形式化验证”。

## Checked command 与 CI gate 建议

执行 `make formal-rga` 可检查所有 pinned Lean model。它故意不进入 `make verify`：若在
必需 Go gate 中安装新的 toolchain，会弱化已有 supply-chain boundary。

只有在 Lean toolchain bootstrap 同样被固定并完成评审后，才应把此命令作为独立 CI job。
项目已经固定 Lean 版本；待仓库 action/toolchain pinning policy 确定后即可加入隔离的
确定性 job。
