# 贡献指南

感谢你为 `github.com/DarkInno/crdt` 做出贡献。本仓库是一个 Go 状态型 CRDT 库；每项变更都必须同时保持合并语义、并发安全和二进制协议兼容性。

提交 API、新的 CRDT 类型或任何帧格式变更前，请先开 issue 说明使用场景、冲突解决语义、持久化/回收策略和兼容性影响。不要把传输、身份认证、授权或业务一致性规则悄悄放进库中。

## 本地环境与快速验证

需要 Go 1.21 或更高版本。仓库当前没有外部 Go 模块依赖；以下命令均从仓库根目录执行：

```sh
go version
go test ./...
go test -race ./...
go vet ./...
make coverage
```

提交前的最小检查是：

```sh
gofmt -w <改动的 .go 文件>
make fmt-check
go test ./...
go vet ./...
```

按改动范围补充运行以下命令：

| 目的 | 命令 |
| --- | --- |
| 分包单元测试 | `make test-unit` |
| 三副本、快照恢复、批处理和 Merkle 反熵 | `make test-integration` |
| 高基数恢复与收敛场景 | `make test-extreme` |
| 数据竞争 | `make race` |
| 解码器模糊测试（每个目标 10 秒） | `make fuzz` |
| 每包覆盖率门槛（90%） | `make coverage` |
| TypeScript frame 解码器 | `make typescript-test` |
| 实际 Go Wasm + Node 三副本协作场景 | `make wasm-test` |
| 静态检查 | `make staticcheck`、`make lint` |
| 完整本地门禁 | `make verify` |
| CI 容器复现 | `make docker-test` |

`make staticcheck` 和 `make lint` 分别需要 `staticcheck` 和 `golangci-lint` 在 `PATH` 中。`make verify` 会包含这两项及 fuzz；不确定工具版本或本机环境时，使用 `make docker-test` 复现 CI 容器检查。性能改动使用 `make benchmark`，并在目标 Go 版本和硬件上与基线比较。

可用下面的可执行示例快速检查真实的编码、重复投递和恢复路径：

```sh
go run ./examples/collaborative-board
make test-integration
```

`cmd/crdt-sync-probe` 仅用于受控的本地/集成演练，默认应绑定 loopback；令牌使用受限权限的临时文件。完整启动、清理和预期结果见 [集成教程](docs/integration/overview.md)。它不是生产同步服务。

## 分支与发布列车

- `beta` 是唯一的集成分支：先在新的、基于 `origin/beta` 的隔离 worktree 提交并运行本地验证，再推送到 `beta`；等待远端 CI 通过后再发起发布 PR。不能对 `beta` 强推。
- `main` 是发布镜像：不在本地开发，只通过已审核的 `beta -> main` PR 更新。`main` 比 `beta` 多一个 merge commit 是正常的发布记录，不要为此 rebase、强推或反向合并。
- 需要更新一个干净的 `main` worktree 时，运行 `make sync-main`。该命令在工作区有改动、不在 `main`、或本地 `main` 存在未发布提交时拒绝执行，以保证只做快进更新。

最小日常流程：

```sh
git fetch origin --prune
git worktree add -b work/<topic> ../crdt-beta origin/beta
cd ../crdt-beta
# 完成修改后，运行与改动相称的检查并提交
git push origin HEAD:beta
```

如果推送被拒绝，先重新获取 `origin/beta`，在隔离 worktree 中检查和整合冲突；不要使用 `--force`。只有当所对应 SHA 的 beta CI 全部成功后，才创建到 `main` 的 PR。

## 架构边界

| 层/目录 | 职责 | 贡献时的约束 |
| --- | --- | --- |
| 根包 `crdt` | 类型 ID、协议策略、通用 CRDT 合约、标签和状态摘要 | 已发布的类型 ID 与语义属于 wire contract；不得复用或改变既有载荷含义。 |
| `counter`、`set`、`lww`、`register` | CRDT 数据类型和类型特定 delta | 明确并测试合并/冲突语义；稳定类型不得破坏兼容性。 |
| `clock` | HLC 与可持久化时钟状态 | 重用副本 ID 的 HLC 型 CRDT 必须原子持久化状态和时钟。 |
| `encoding` | 规范化、有版本和有边界的二进制帧 | 维持确定性编码；未知版本、类型或非规范输入必须拒绝。 |
| `delta`、`snapshot`、`merkle`、`replica` | 批处理、恢复计划、反熵摘要和检查点/收件箱 | 不把“最大标签”误当作无缺口确认；所有边界和兼容性都要显式验证。 |
| `tombstonegc` | 基于精确确认和成员 epoch 的墓碑回收 | 只能在应用提供已认证、权威的成员视图后回收；不能从本库推断成员身份。 |
| `text`、`tree`、帧化 `lww.Map` | 实验性协议 | 必须通过每复制组的 `ProtocolPolicy` 明确启用；在其 GC 生命周期稳定前保留墓碑。 |
| `extensions` | 显式启用的 WebSocket 与 HTTP/SSE live relay 参考实现 | 默认不暴露端点；应用拥有 listener/TLS、认证授权、持久化 outbox、重连、反熵和容量策略。 |
| `cmd/` 与 `examples/` | 诊断和可执行集成示例 | 不把它们当成生产传输、鉴权或持久化实现。 |

新增 CRDT 前，先定义：状态偏序/`Merge` 的 join、并发冲突语义、delta 与状态帧、快照和副本重启行为、墓碑生命周期、资源上限，以及稳定或实验性的发布策略。若语义要求排他预订、余额、不变量校验或工作流顺序，应由权威业务服务实现，不能依赖 CRDT 收敛替代。

## 实现与兼容性要求

- 运行 `gofmt`。导出标识符使用 `PascalCase`，未导出标识符使用 `camelCase`；新增导出 API 必须有 Go 文档注释。
- 采用 Go 的显式错误返回；库代码不应因调用者输入而 `panic`。错误应保留上下文并可用 `errors.Is`/`errors.As` 判断。
- `Merge` 必须满足交换律、结合律和幂等性；若返回错误，接收者状态不得被部分修改。`ApplyDelta` 同样必须能安全地重复、乱序投递。
- 任何 map 遍历进入状态摘要、帧、哈希或公开序列时必须使用规范顺序，确保相同逻辑状态得到相同字节和摘要。
- 对共享状态保持锁粒度清晰；不得在持锁时调用可重入的外部回调。涉及锁、复制或缓存的改动必须通过 `go test -race ./...`。
- 只有在已定义帧、解码器、快照恢复和测试时，新的 type ID 才能成为可协商协议；保留 ID 不是功能实现，也不是对外承诺。
- 使用短生命周期分支（如 `docs/contributing-guide`），提交保持原子。仓库采用 Conventional Commits，例如 `docs: add contribution guide`、`fix(set): reject non-canonical delta`。破坏性公开 API 或 wire 变更须在 issue、PR 和提交中明确标注。

## 安全与数据边界

二进制帧、delta、快照、codec 输出和网络请求都应视为不可信输入。CRC 只检测意外损坏，不能提供身份认证、完整性防篡改或加密。

- 接收外部数据时，先在应用层认证和授权，再以适合传输层的 `DecoderLimits` 调用 `Unmarshal*WithLimits`；不要无条件使用默认上限。
- 限制请求体、帧、payload、元素、标签、字符串、待处理队列和批处理总字节数；拒绝畸形、超限、未知/不匹配类型和非规范编码，且不要部分更新状态。
- `ElementCodec.ID`、`Marshal`、`Unmarshal` 是跨副本协议的一部分：保持 ID 和编码稳定、可重现、可并发调用；更改字节格式必须显式版本化。
- 为每个存活逻辑副本分配全局唯一、非空的 ID。恢复同 ID 的 OR-Set、LWW-Map、RGA 或 OR-Tree 前，原子恢复其 HLC 状态；不能仅恢复状态字节。
- 外部应用负责 TLS、认证、授权、重放/重试策略、持久化事务、密钥和日志脱敏。不要提交令牌、私钥、真实快照或包含敏感值的测试夹具。
- 墓碑压缩需要当前成员 epoch 下的精确确认；成员退出后再次加入必须从压缩后的快照引导。未经授权的成员视图或仅凭 frontier 不能证明可安全回收。

## 性能要求

性能工作必须先说明工作负载：CRDT 类型、元素/副本/墓碑数量、编码器、重复率、并发模型、Go 版本和硬件。不要用小样本或不同语义的基准宣称性能提升。

- 避免在热路径引入不必要的排序、深拷贝、反射、字符串格式化、全局锁或未受限的累积缓存；确定性输出所需排序应只在序列化/摘要边界进行，并覆盖大状态场景。
- 保留编码、解码和批处理的长度上限，避免攻击者驱动的大分配或超线性工作；优化不得绕过验证或改变规范化格式。
- 修改 `Merge`、`ApplyDelta`、编解码、快照、Merkle 或锁策略时，运行相关 `Benchmark*`：

  ```sh
  go test -run='^$' -bench=. -benchmem ./...
  # 需要对照时可固定运行时设置，例如：
  GOMAXPROCS=1 go test -run='^$' -bench=. -benchmem ./set
  ```

- 在 PR 中给出命令、提交基线、环境和 `ns/op`、`B/op`、`allocs/op` 对比；若结果波动或回退，要说明原因和可接受性。必要时用 `pprof` 证明优化针对真实瓶颈。

## 测试用例清单

测试与实现同一变更提交。优先使用标准库 `testing` 的表驱动测试；测试文件命名为 `<name>_test.go`，基准为 `Benchmark...`，fuzz 目标为 `Fuzz...`。

| 变更类型 | 至少覆盖的用例 |
| --- | --- |
| CRDT 语义 | `Merge` 的交换律、结合律、幂等性；重复/乱序 delta；并发冲突的明确胜出规则；非法输入不改变接收者。 |
| 新 mutation 或状态 | 空值、边界值、溢出、无效 replica ID、相同与不同副本、错误返回，以及状态/摘要的不可变副本。 |
| 编解码或 type ID | 同一状态重复编码字节一致；状态与 delta 不能混用；未知/错误版本、校验和、截断、非最短 varint、尾随字节、codec/type 不匹配和所有限制都被拒绝。 |
| 快照、时钟和恢复 | 状态与 HLC 时钟的原子恢复；同 ID 恢复后的新 tag 不冲突；恢复计划的类型/codec/大小限制；反熵后收敛。 |
| 并发或锁 | 多 goroutine mutation/merge/encode 的行为和 `-race`；交叉合并不死锁；外部回调 panic/错误不污染状态。 |
| 墓碑和成员 | 精确确认、epoch 变化、成员退役/重新引导、乱序历史下拒绝不安全压缩。 |
| 输入处理 | Fuzz 合法种子和随机/截断字节；目标是不 panic、不超限分配，并且错误路径不留下部分状态。 |
| 集成路径 | 至少三副本、重复和乱序投递、分区后的冲突、快照/批处理/反熵修复，以及最终确定性状态。 |

修复缺陷时，先加入能在修复前失败的回归测试。公开行为、示例输出或配置边界变化时，同步更新 `README.md`、`docs/integration/overview.md` 或相关 Go 文档。

## 提交前检查与 PR

在 PR 描述中说明问题、语义/兼容性影响、测试证据，以及（如适用）安全边界与性能对比。不要混入无关格式化、生成物或他人的未提交改动。合并前确认：

- [ ] 变更范围小且可审阅，导出 API、协议和实验性状态已说明。
- [ ] 已运行与改动相称的格式化、测试、race、vet、覆盖率、fuzz 和静态检查。
- [ ] 所有新外部输入都有明确的验证、资源上限和错误路径。
- [ ] CRDT 语义、确定性编码、快照/HLC 恢复和 tombstone 生命周期没有被破坏。
- [ ] 若改动影响热路径，已附上可复现的基准环境和对比。
- [ ] 文档、示例和兼容性说明与代码一致。

感谢你帮助这个库在真实的重复投递、乱序、分区和重启条件下仍保持可验证的收敛行为。
