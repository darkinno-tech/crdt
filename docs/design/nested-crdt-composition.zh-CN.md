# 嵌套 CRDT 组合：架构决策与后续准入门槛

## 结论

当前库的 Go framed CRDT 仍是**一组独立、单一语义的复制组**；不应将它们以
`interface{}`、递归 JSON 或“一个 Manifest 内多个 TypeID”的方式临时拼接为嵌套
文档。这样的改动会绕过 Manifest 的精确协议绑定、快照恢复与资源预算，不能证明
收敛。

`clients/typescript` 已提供独立协商的 `native-ts-nested-v1`。它组合 LWW Map 和
RGA Array 两种原语，子容器由其整合操作的不可变 ID 唯一命名；它**不是** Go frame
协议、Yjs 兼容层或“所有 CRDT 类型均可相互嵌套”的承诺。它已经覆盖有限乱序暂存、
重复投递、快照恢复、别名/循环拒绝和三副本 shuffled simulation。

因此本次选择是：保留现有 nested TypeScript 合同并加强全量 fuzz 的动态发现；Go
嵌套能力在具备下述协议、测试与运维证据前不发布。

## 事实与外部对照

| 事实 | 当前证据 | 架构含义 |
| --- | --- | --- |
| Go 一个 `replica.Manifest` 精确绑定一组 state/delta TypeID、codec、语义版本和 epoch | `replica/replica.go` | 子对象不能在同一复制组中悄悄切换协议 |
| `native-ts-v1` 的 JSON 值是复制后原子值 | `clients/typescript/src/native.ts` | 需要独立语义版本才可让子树独立合并 |
| `native-ts-nested-v1` 使用父操作 ID 绑定子容器 | `clients/typescript/src/nested.ts` | 单所有者、不可别名、不可移动是正确性前提 |
| Yjs 同样要求 shared type 只整合一次 | Yjs Shared Types 文档 | “可移动/复用子节点”必须是新的、可证明的操作语义，而不是引用赋值 |
| Automerge 用稳定对象 ID 识别嵌套对象 | Automerge data model 文档 | 将对象身份与用户可见路径分离，能抵抗并发改名/移动 |

外部实现只用于验证设计方向；本库不声称与其 wire format 或行为兼容。

## 方案比较

| 方案 | 正确性/安全 | 性能 | 结论 |
| --- | --- | --- | --- |
| 递归 JSON 放入现有 LWW/RGA 值 | 子节点是原子替换，不能独立合并；容易误导调用方 | 编码简单但整块复制/冲突放大 | 保持为原子值，不称为嵌套 CRDT |
| 一个 Manifest 混用任意 TypeID | 破坏当前协议、codec 与快照的一对一绑定 | 需要动态分派与更多恢复状态 | 拒绝 |
| 以不可变整合 ID 命名的单所有者子容器 | 能验证父子类型、拒绝别名/循环，并允许乱序重放 | 路径查找和元数据线性受限于容器数 | `native-ts-nested-v1` 已采用 |
| 新建 Go `document-tree-v1` 协议 | 可把对象表、类型表和根声明纳入同一有界、可版本化快照 | 必须先测量对象表、pending 队列和批量编解码 | 后续唯一可接受的 Go 路径 |

## Go `document-tree-v1` 的必要不变量

若启动该协议，必须新分配 state/delta TypeID、语义版本、规范向量和独立 Manifest；
不得改写既有 frame 的含义。最低设计为一个带稳定 `ObjectID` 的对象表：根声明、对象
种类和父引用均在快照中编码，所有操作携带目标 ObjectID。

1. ObjectID 仅由创建操作产生，单所有者，永不复用；引用必须与创建 dot 精确匹配。
2. 每个对象声明一种固定 CRDT kind；Map、sequence、text 等只在自己的 kind 内解释操作。
3. 未知父对象的操作只能进入按**对象数、操作数、字节数和等待时间**共同限制的队列；
   超限需在 mutation 前拒绝。快照和 compaction 不得遗漏该队列。
4. 删除、移动、墓碑回收必须定义并发语义与 epoch/ack 条件。初版禁止移动和跨父复用，
   直到有可重放的 move 证明。
5. 所有输入先做 frame 总长度、对象数、深度、字符串/值长度、操作数和总 pending
   bytes 的预检，再分配、递归或修改状态。
6. 恢复必须原子保存对象表、根声明、frontier、HLC 和本地计数器；恢复后的新 ID 不得
   与任何已保存 ID 重叠。
7. 宿主仍负责认证、授权、body-size 限制、重放窗口、加密与持久化事务；校验和或
   TypeID 不是身份认证。

## 验证与性能门槛

在发布 `document-tree-v1` 前，至少需要以下独立证据：

- **正确性**：每种 kind 的状态/增量规范向量；两、三、五副本的随机交错、乱序、重复、
  离线恢复、删除竞争和父后到达模拟；同种子可复现的状态/前沿一致性断言。
- **安全性**：fuzz frame、对象表和快照；深度/宽度/字节/待处理队列边界；别名、循环、
  type confusion、错误 parent ID、重复 ObjectID、压缩炸弹和恢复计数器回退均须证明
  拒绝且不改变状态。
- **并发**：`go test -race` 覆盖本地编辑与远程应用并发、观察回调与 snapshot/restore；
  容器锁顺序固定且不在锁内执行宿主回调。
- **性能**：基准同时报告合成的深/宽树、真实看板或文档编辑轨迹、快照恢复与乱序 pending
  洪峰；报告 CPU、alloc/op、峰值 retained bytes、p50/p95，并与独立 CRDT 复制组基线
  在同一 Go 版本和受控机器上比较。
- **互操作与运维**：Go、Wasm 和 TypeScript 向量只能在共享的明确协议下互通；迁移采用
  新复制组/epoch，而非旧客户端静默降级。真实 provider 端到端压测另行证明，不以本地
  单进程 benchmark 代替。

## 本次 Makefile 变更

`make fuzz` 现在枚举 `go list ./...` 中所有 `Fuzz*` 目标并逐个运行，因为 Go 每次
fuzz 调用只能选择一个目标。`make fuzz-list` 输出实际枚举结果，便于审计新增包/新增
目标是否进入 release gate。`make fuzz-smoke` 故意继续保留小而显式的攻击面清单，作为
PR 的快速门禁；`beta` release 仍运行全量动态 fuzz。

这消除了完整 fuzz 列表维护漂移，却没有将安全关键的 smoke 选择隐藏在自动发现中。
