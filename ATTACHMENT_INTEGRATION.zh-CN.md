# 附件引用集成

`attachment.Register` 是图片、音频、视频和任意二进制数据的稳定 CRDT 边界。它复制有边界、
不可变的引用；绝不传输对象原始字节。

完整的文本、附件、Manifest、快照和校验流程见[可运行附件协作示例](examples/attachment-collaboration)：

```sh
go run ./examples/attachment-collaboration
go test ./examples/attachment-collaboration
```

## 被复制的内容

| 字段 | 用途 | 安全规则 |
| --- | --- | --- |
| 应用键 | 文档内附件的稳定位置 | 有边界 UTF-8；不允许控制字符或首尾空白。 |
| `ObjectID` | 应用定义的不透明对象标识 | 不能是签名 URL、凭据、个人数据或原始媒体。 |
| `MediaType` | 规范 MIME 类型，例如 `image/png`、`audio/ogg` | 拒绝参数和非规范形式。 |
| `Size` | 对象预期字节长度 | 受 `attachment.Options.MaxObjectBytes` 限制。 |
| `Digest` | 精确对象字节的 SHA-256 | 解码、预览或渲染前必须校验。 |

可编辑文本应使用 `text.RGA`；附件引用只适合不可变的外部对象。可变结构化数据应按冲突语义
选择 `lww.Map`、OR-Set 或 OR-Tree。

## 复制契约

附件引用复用稳定 LWW-Map 状态/增量帧 TypeID 9/10。一个附件复制组必须具有经过认证的
`replica.Manifest`：

| Manifest 字段 | 必需值 |
| --- | --- |
| `StateID` / `DeltaID` | `crdt.TypeIDLWWMapState` / `crdt.TypeIDLWWMapDelta` |
| `SchemaID` | `github.com/im10furry/crdt/attachment-reference/v1` |
| `CodecID` | 空字符串 |
| `SemanticsVersion` | `attachment.SemanticsVersion` |
| policy | 零值 `crdt.ProtocolPolicy` |

不要把可编辑 RGA 文本与附件引用放进同一个 Manifest：一个 Manifest 只代表一种具体 CRDT
协议。对于同一应用文档，应分别建立文本组和附件组；可运行示例即按此方式组织。

传输边界必须先认证对端及精确 Manifest，再接收帧。使用
`attachment.UnmarshalDeltaWithLimits` 解码附件 delta，并同时使用传输层
`frame.DecoderLimits` 和匹配的 `attachment.Options`，之后才调用 `Register.ApplyDelta`。
校验和有效不能证明发送方已授权，也不能证明存储中的对象正确。

## 创建、投递、恢复与校验

1. 通过应用已授权的存储路径上传或选择不可变对象字节；发布引用前完成内容扫描和配额限制。
2. 计算 SHA-256 并创建 `attachment.Reference`；调用 `Register.Put` 获得 delta。将本地
   outbox/receipt 与 CRDT 快照、HLC 状态原子持久化。
3. 在持久化 `replica.Dot` 下只发送规范 delta 帧；接收方使用 `replica.Inbox` 保持连续投递 frontier。
4. 原子持久化 `Register.SnapshotCurrentState()`，通过
   `attachment.NewFromSnapshotWithOptions` 恢复同 ID 副本。
5. 授权下载后、解析或渲染前调用 `Reference.Verify`。它使用固定缓冲流式校验，不保留媒体
   字节，并拒绝截断、超长和 SHA-256 不匹配响应。

```go
file, err := os.Open(downloadedPath) // 已授权且有大小限制的下载目标。
if err != nil {
	return err
}
defer file.Close()
if err := ref.Verify(file); err != nil {
	// ErrContentMismatch 表示不可信/无效对象；不得继续解码。
	return err
}
```

## 限制、删除与运维

应按复制组设置 `attachment.Options`。`MaxEntries` 包含保留的删除元数据，因此预算必须覆盖
文档生命周期，而不能只计算当前可见媒体。删除是 LWW 墓碑；在线下副本仍可能发送旧引用时，
不得静默删除该墓碑。当前库不提供附件墓碑压缩；在实现之前，应监控保留条目并建立产品级
留存/恢复策略。

本 CRDT 库不实现对象存储、签名上传/下载 URL、身份、授权、加密、恶意内容扫描、内容策略或
重试队列。这些是应用责任，且必须与附件键使用相同的租户/文档授权。

## 验证

```sh
go test ./attachment
go test -race ./attachment
go test -run=^$ -fuzz=FuzzUnmarshalDelta -fuzztime=250000x ./attachment
go test -run=^$ -fuzz=FuzzReferenceVerify -fuzztime=250000x ./attachment
go test ./examples/attachment-collaboration
```
