# RGA 诊断混淆

`text.RGA` 可以导出一个结构仍然有效的隔离调试副本，而不共享插入的字符内容。它适用于 support
case，不是安全边界，也不能替代端到端加密。

```go
// 导出 state，不会修改 document。
debugState, err := document.MarshalObfuscatedRunBinary()

// 导出同一隔离调试时间线的增量。
debugDelta, err := delta.MarshalObfuscatedRunBinary()
```

旧标量 RGA 也可使用 `MarshalObfuscatedBinary`、
`MarshalObfuscatedBinaryWithLimits` 与 `Delta.Obfuscate`。所有 `WithLimits` 版本都沿用普通
编码器相同的 frame 预算。

## 保留与移除的信息

每个插入的 Unicode scalar 都会换成固定的惰性占位符，并保持相同的 uvarint 宽度。导出保留：

- node position、父子关系；
- tombstone、操作数量和可见/已删除结构；
- 规范 wire 校验，以及将来自同一原始时间线的多个**已混淆** delta 解码、去重与合并的能力；
- 正常 scalar/run encoder 的 frame payload 长度。

它移除真实文本值，也绝不修改源 `RGA` 或 delta。因此空的 debug replica 可以通过重复、乱序的
已混淆帧复现 parser、排序、待父节点、tombstone 和 snapshot 行为。

## 安全边界

混淆会保留 replica ID、HLC 派生 position、文档拓扑、操作数量和近似 rune 编码宽度；这可能泄露
作者标识、时间、文档规模、编辑形状和字符类别。因此它不适合对抗性场景、合规导出或必须隐藏元数据
的日志。应用还必须单独清理 schema 名称、provider header、附件引用和外围日志。

绝不可将已混淆 delta 应用到可能已经含有原 delta 的 replica：RGA position 是不可变的，同一
position 的不同字符会正确产生 `text.ErrTagConflict`。原始与混淆产物必须放在不同 support
namespace；发送它们时仍须正常认证、授权和传输加密。

自动模拟验证三份 debug replica 在 run-v2 帧重复、乱序时收敛，并验证原始 delta 在其混淆版之后
会被拒绝。实际 support 机器上可测量导出开销：

```sh
go test -run='^$' -bench='BenchmarkRGARunObfuscatedState' -benchmem ./text
```
