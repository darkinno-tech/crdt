# CRDT 同类库文本基线对比 — 2026-07-30

本报告补齐的是一个严格限定的对比缺口，而不是“谁绝对更快”的宣传：darkinno-tech 使用 Go，
Yjs 使用 JavaScript，二者 wire format 不兼容，文本存储模型也刻意采用不同权衡。

## 工作负载契约

`initial` 场景每次操作新建两个副本，把预先构造的 ASCII 文本插入 source 的 offset 0，
编码 source 的初始 update，应用到 target，并验证 target 文本相同。fixture 创建和最终
完整 state 编码不计时；update/state 字节数单独记录。每个报告样本平均 20 次操作，每个
文本长度先做 2 个不报告 warm-up batch，再做 5 个报告 batch。

| 一侧 | 实现 | 协议 / API |
| --- | --- | --- |
| darkinno-tech | Go `text.RGA` | run-v2 `InsertRunBinaryWithLimits`、decode、`ApplyDelta` |
| 对照 | `yjs@13.6.31` | `Y.Text.insert`、`encodeStateAsUpdate`、`applyUpdate` |

这只是**无冲突初始同步**基线，不含 WebSocket/WAN、TLS、存储、认证、重连、GC 策略、
富文本格式、重复编辑、并发冲突和 retained heap。不同 runtime 的耗时不能相除后宣称
绝对性能高低。

## 本机受控结果

主机：Apple M4 Pro（12 logical CPUs、24 GiB），Darwin arm64；Go `1.26.5`、Node
`v26.5.0`；harness revision `8cc25755dc5dbc4163c7c68eb2de80075f5d29bf`。Yjs 依赖由
[`bench/competitors/package-lock.json`](../../bench/competitors/package-lock.json) 固定。

| Runes | darkinno-tech median ms/op | Yjs median ms/op | darkinno-tech update/state bytes | Yjs update/state bytes |
| ---: | ---: | ---: | ---: | ---: |
| 4,096 | 6.076 | 0.117 | 36,774 / 36,774 | 4,113 / 4,113 |
| 16,384 | 25.650 | 0.079 | 147,367 / 147,367 | 16,403 / 16,403 |

字节数只对这个“协议互不兼容但工作负载一致”的场景有效：darkinno-tech 稳定的每 Unicode
scalar RGA identity，相比 Yjs 紧凑的单字符串初始 update，约多九倍字节。时间数据可
作为各自 runtime 的回归基线，不能作为跨语言容量排名。特别是 Yjs 能紧凑表达这一条
连续插入；darkinno-tech 则保留独立 position，以兑现删除、乱序投递、anchor 和未来并发插入
的 RGA 语义。

这是设计权衡，而不是缺陷。经单独协商的 outer frame v2 可以为大粘贴和快照压缩 run-v2
payload 中重复的字段，但仍保留每个 scalar 可独立寻址的 HLC tag 与 parent link。不能把它表述成
改变上述跨库字节比例，或使两种 wire format 互操作。对已协商 v2 的 Go/Wasm 组，
`InsertRunFrameV2WithLimits`、`MarshalRunFrameV2` 和
`SnapshotRunFrameV2CurrentStateWithLimits` 可直接写出该表示；如果压缩不能降低完整 envelope，
小编辑仍可能使用 raw v2 payload。

## 离线并发、重复和乱序投递

`offline-concurrent` 在不改变任一稳定 wire 协议的前提下补齐首个计划对比单元。seed 写入
相同的 ASCII base text；两个 writer 收到该 base 后，独立把中间同一 scalar 替换为 `A` 或
`B`，随后各自重复接收对方 update 两次。第三个起始为空的 observer 先收到 right update
两次、再收到 left update 两次，最后才收到 base update 两次。每个 replica 必须收敛，
darkinno-tech observer 还必须没有 pending parent。报告的 update bytes 为一份 base update 加两份
唯一 writer update；state bytes 为最终 left replica state。

| Base runes | darkinno-tech median ms/op | Yjs median ms/op | darkinno-tech update/state bytes | Yjs update/state bytes |
| ---: | ---: | ---: | ---: | ---: |
| 4,096 | 14.639 | 0.444 | 36,949 / 36,897 | 4,177 / 4,181 |
| 16,384 | 62.830 | 0.527 | 147,414 / 147,362 | 16,473 / 16,477 |

该场景验证的是两套实现各自在相同编辑与投递 trace 下的收敛，而不是两种独立 CRDT
算法必然呈现相同的并发 tie-break 可见顺序；它也不证明浏览器/WAN 性能、retained heap
或 provider 端到端恢复。耗时列仍只在各自 runtime 内有效；wire-byte 列展示表示法权衡，
不能上升为通用容量结论。

## 提供测试机上的 darkinno-tech 确认

同一源码以 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0` 交叉编译。每台临时远端副本执行
前均核验 SHA-256：`06e91e54085e031b08d521fa225c40e7bbf3047094f30911a88409457bb0d1a2`。
两台提供的主机均报告 Debian GNU/Linux 13、`linux/amd64`、4 vCPU。每格仍为 5 个样本、
每样本平均 20 次操作；结束后已删除 mode-0700 临时目录和二进制。

| 工作负载 | Host A median ms/op | Host B median ms/op | Update/state bytes |
| --- | ---: | ---: | ---: |
| 4,096 runes | 22.609 | 22.700 | 36,774 / 36,774 |
| 16,384 runes | 102.368 | 103.812 | 147,111 / 147,111 |

两台主机均未安装 Go 或 Node。自包含 Go binary 使 darkinno-tech 远端数字仍是有效受控证据；
为了得到 Yjs 数字而安装全局包并不在本次授权范围内。因此 Yjs 栏暂保持本机 Node 证据；
待单独批准在一次性远端环境配置固定 Node runtime 后再补远端对照。

## 复现

```sh
npm --prefix bench/competitors ci --ignore-scripts
revision="$(git rev-parse HEAD)"
reports="$(mktemp -d)"
go run ./cmd/crdt-compare \
  -scenario=initial -sizes=4096,16384 -samples=5 -warmups=2 -iterations=20 \
  -revision="$revision" -output="$reports/darkinno-tech-initial.json"
npm --prefix bench/competitors run yjs -- \
  --scenario initial --sizes 4096,16384 --samples 5 --warmups 2 --iterations 20 \
  --revision "$revision" --report "$reports/yjs-initial.json"
go run ./cmd/crdt-compare \
  -scenario=offline-concurrent -sizes=4096,16384 -samples=5 -warmups=2 -iterations=20 \
  -revision="$revision" -output="$reports/darkinno-tech-offline.json"
npm --prefix bench/competitors run yjs -- \
  --scenario offline-concurrent --sizes 4096,16384 --samples 5 --warmups 2 --iterations 20 \
  --revision "$revision" --report "$reports/yjs-offline.json"
```

两侧必须在同一台空闲主机运行，并保留两份 JSON、host、runtime、revision 和
package-lock。两个 runner 都会创建指定的报告父目录。runner 会拒绝非法文本长度、每次
都校验收敛；`node --no-experimental-webstorage --expose-gc` 会避免 Node localStorage
warning，并在每个 Yjs 样本前尝试 GC。

## 下一批对比单元

已经完成的两个单元仍不足以得出产品级速度结论。任何结论前，至少为两库按同一 trace
补齐：

1. 长编辑会话加重连和 state-vector/snapshot catch-up：用独立 runtime profile 测 peak
   retained memory。
2. 在提供的 Linux 主机上做带认证的 1/4/16 observer provider fan-out，拆开 loopback
   relay 与 WAN/TLS/persistence。
3. 富文本格式和相对 cursor：按功能覆盖和失败处理报告，不能只看 bytes。

darkinno-tech 后续应据此决定协议演化。当前 stable run-v2 绝不能为了改善这一个无冲突数字，
悄悄换成 Yjs framing 或新的 chunk 模型。
