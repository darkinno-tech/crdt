# 压缩感知的 CRDT 外层帧 v2

本文定义可选的**外层帧 v2**。它不改变 CRDT 的 TypeID、CodecID、解码后的 payload 或合并
语义；同一套外层表示可承载 G-Counter、RGA delta、run-v2 RGA state 等已实现的规范 payload。

它不能与 RGA **run-v2**（TypeID 19/20）混为一谈：run-v2 压缩 RGA 内部同副本链，外层帧 v2
压缩整个规范 frame payload。一个复制组可以分别选择两者，但必须协商精确组合。

## 启用与协商

v2 适合快照、大粘贴和含重复 actor ID、key 或 value 的批量更新。对很小的交互 delta，两个
长度字段的额外开销可能使其大于 v1；编码器仅在 DEFLATE 让**完整 v2 帧**更小时才压缩。请在
目标工作负载上测量后启用。

现有 `MarshalBinary` API 仍产出 v1。Provider 或存储边界可以显式转换：

```go
v1, err := delta.MarshalRunBinary()
v2, err := frame.ConvertFrameV1ToV2(v1, receiveLimits)
```

已经协商外层 v2 的 run-v2 RGA 组应优先使用直接 API。它们会对最终 v2 帧做预检，而不是先构造
再校验一个中间 v1 envelope：

```go
update, err := document.InsertRunFrameV2WithLimits(0, pastedText, receiveLimits)
if err != nil { /* 在 mutation 前拒绝本地编辑 */ }

checkpoint, err := document.SnapshotRunFrameV2CurrentStateWithLimits(receiveLimits)
if err != nil { /* handle */ }
```

`Delta.MarshalRunFrameV2`、`RGA.MarshalRunFrameV2` 及配套的 anti-entropy API
与其 v1 对应方法产出同一份规范 run-v2 payload。这只是外层表示优化：不会合并 scalar identity、
改变 RGA 排序，也不会让 outer-v2 组兼容 v1 peer。直接路径可以使用小于等价 v1 envelope 的
压缩后最终帧预算，但解码出的 payload 仍必须满足 `MaxPayload`。

v2 组必须在 `replica.Protocol` 中绑定
`WireFormatVersion: frame.FormatVersionV2`。`NewChange`、`Inbox`、快照、恢复计划和
checkpoint 会拒绝本应有效但格式为 v1 的帧，绝不静默降级。字段为零仍表示 v1，以兼容已有
Go 字面量和缺少该 JSON 字段的旧 Manifest。

```go
manifest, err := replica.NewManifest("notes/42", "example.com/note/v1", 1, replica.Protocol{
    StateID:           crdt.TypeIDRGARunState,
    DeltaID:           crdt.TypeIDRGARunDelta,
    SemanticsVersion:  2,
    WireFormatVersion: frame.FormatVersionV2,
}, crdt.ProtocolPolicy{})
```

协商后的帧版本会进入 Manifest hash 与 checkpoint digest。接收 bytes 前仍必须认证精确
Manifest；TypeID、CRC-32C 或压缩模式都不能认证对端。

## Wire 格式

所有整数均为最短形式 unsigned LEB128 `uvarint`；`bytes` 是长度 `uvarint` 后紧接的精确字节。

```text
frame             = "CRDT" version type-id codec-id payload-mode raw-length encoded-length encoded-payload checksum
version           = uvarint(2)
type-id           = uvarint ; 非零
codec-id          = bytes
payload-mode      = uvarint(0 raw / 1 raw-DEFLATE)
raw-length        = uvarint
encoded-length    = uvarint
encoded-payload   = 恰好 encoded-length 字节
checksum          = 四字节大端 CRC-32C (Castagnoli)
```

checksum 覆盖 `"CRDT"` 后至 `encoded-payload` 末尾的全部字节，但不提供认证或加密。模式 0
要求 `raw-length == encoded-length`；模式 1 是完整 RFC 1951 raw-DEFLATE 流，且必须解压出
**恰好** `raw-length` 字节。未知模式、非规范 varint、长度不匹配、非法流、输出过短或过长都
必须拒绝。

压缩后的 bit stream 本身不要求规范：不同 DEFLATE level 只要解出同一规范 CRDT payload 都可
接受。Go 编码器选择 `flate.BestSpeed` 平衡交互 CPU，并仅在 mode 1 的完整帧更小时使用它。

## 正确性、资源与安全

1. 解析前限制传输 body 和 `MaxFrameBytes`。
2. 先校验 CRC-32C 与外层字段，后分配解压输出。
3. 在解压前拒绝 `raw-length > MaxPayload`；最多读取 `raw-length + 1` 字节，并要求精确结束。
4. 将解出的 payload 交给不变的类型解码器。RGA run 和富文本的规范性检查比较的是**解出后的
   payload**，并未因外层 v2 放松约束。
5. 只有 Manifest、type、codec、语义版本和外层帧版本均相等才可应用。转换 API
   `ConvertFrameV1ToV2`/`ConvertFrameV2ToV1` 必须显式调用，解码从不自动降级。

run-v2 的规范性检查会重建并比较解码后的 payload，而不是构造临时 v1 envelope。这样规范性校验
与压缩传输预算相互独立，同时保留每个 scalar 的 HLC tag 与 parent link。

payload 上限限制内存放大，但压缩不保密。live 流量仍需 TLS 和应用层鉴权；不能仅因压缩体能在
默认进程上限内展开，就接受任意大的公网请求。

Go/Wasm RGA runtime 使用同一 Go decoder，认证 v2 Manifest 后可以接收外层 v2。当前无依赖
TypeScript `decodeFrame` 只验证 v1；纯 TypeScript consumer 在拥有同等有界 raw-DEFLATE decoder
前不得通告外层 v2。

已覆盖转换、损坏或过度展开输入、最终帧与解码 payload 的双重限额、fuzz 外层帧、
Manifest/checkpoint/recovery 版本绑定以及含重复和乱序 v2 帧的三编辑者 RGA 模拟。受控基准：

```sh
go test -run='^$' -bench='BenchmarkFrameUpdateFormats|BenchmarkRGADeltaWireProtocols' -benchmem ./encoding ./text
```
