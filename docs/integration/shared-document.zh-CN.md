# 不理解 CRDT 算法也能使用共享文档

[English](shared-document.md)

`shared.Document` 是面向结构化协作内容的 Go 高层入口。它将稳定的
`document/tree-v1` 协议包装为命名 `Map`、`Array` 和直接读写方法：业务代码
从“看板”“任务”“字段”开始，不需要手写 delta、TypeID 或 HLC。

它借鉴 Yjs 的“文档 + 命名共享类型”交互方式，但**不兼容** Yjs API 或二进制
update。已有 Yjs 客户端应使用独立的 [Yjs relay](yjs-relay.zh-CN.md)。

## 最短业务代码

```go
doc, err := shared.New("editor-a")
if err != nil {
	return err
}

board, err := doc.Map("board") // 类似 Y.Doc.getMap("board")
if err != nil {
	return err
}
if err := board.SetString("title", "Release plan"); err != nil {
	return err
}

tasks, err := board.CreateArray("tasks")
if err != nil {
	return err
}
task, err := tasks.InsertMap(0)
if err != nil {
	return err
}
if err := task.SetJSON("task", map[string]any{"id": "release-notes", "done": false}); err != nil {
	return err
}
```

常用操作是 `Set` / `Get`、`SetString` / `String`、`SetJSON` / `JSON`。值在
写入和读取时都会复制；`CreateMap`、`CreateArray`、`InsertMap` 和
`InsertArray` 创建单一所有者的嵌套对象，因此一个子对象不会被移动或挂到两个
位置，离线并发时仍有确定结果。

你不需要实现 CRDT 算法，但需要选对业务含义：

- 并发写同一个 Map key 使用 document tree 的 LWW 规则，最终只会确定性地保留一个
  可见值。
- Array 插入使用确定的 RGA 顺序；删除保留锚点，以便稍后到达的离线更新仍可合并。
- CRDT 不能保证独占预约、余额、权限决策或工作流流转；这些必须留在权威服务中。

| 协作动作 | Yjs | 此 Go facade |
| --- | --- | --- |
| 创建文档 | `new Y.Doc()` | `shared.New("editor-a")` |
| 获取/创建命名 Map | `doc.getMap("board")` | `doc.Map("board")` |
| 获取/创建命名 Array | `doc.getArray("tasks")` | `doc.Array("tasks")` |
| 订阅本地 update | `doc.on("update", ...)` | `doc.OnUpdate(...)` |
| 应用 update | `Y.applyUpdate(doc, update)` | `doc.ApplyUpdate(update)` |

Go 显式返回错误，所有错误都必须处理。每一次修改会产生一帧 update；当前 v1
没有把多次方法调用合成为一个 Yjs transaction 的语义。可以在已认证的传输层批量
发送这些彼此独立的帧，但不能把它们误当作原子业务事务。

## 真实网络接入：只增加一个明确的预算

本地原型可使用单参数构造器。网络组应选择与 transport body limit 和租户配额一致
的上限，并将本地 update 交给持久 outbox：

```go
limits := frame.DecoderLimits{
	MaxFrameBytes:  64 << 10,
	MaxPayload:     60 << 10,
	MaxCodecID:     128,
	MaxElements:    512,
	MaxTags:        512,
	MaxStringBytes: 1024,
}
options := shared.DefaultOptions()
options.FrameLimits = limits
doc, err := shared.NewWithOptions("editor-a", options)
if err != nil {
	return err
}

stop, err := doc.OnUpdate(func(update []byte) {
	// 写入已经认证的 outbox；此处不要执行阻塞网络 I/O。
	outbox.Append(update)
})
if err != nil {
	return err
}
defer stop()
```

接收端先在 HTTP/WebSocket 层限制 body，再认证 peer、授权精确的
group/schema/值策略，最后才调用：

```go
if !authorized(peer, groupID, manifest) {
	return errUnauthorized
}
if len(body) > limits.MaxFrameBytes {
	return errTooLarge
}
if err := doc.ApplyUpdate(body); err != nil {
	return fmt.Errorf("reject shared update: %w", err)
}
```

`ApplyUpdate` 在变更状态前会按 frame 和 retained-state 上限完整校验。重复帧、
父对象晚到的子帧都可以收敛；未完成依赖只在配置的 pending 上限内保留。CRC、profile
或解码成功都不是身份认证或授权。

同一个 `FrameLimits` 也约束本地发出的 update。它必须容纳 `DocumentOptions`
允许的最大一次操作；若本地操作已改变状态但因 output-frame limit 返回错误，应持久化
checkpoint，并在兼容的组预算下修复 outbox，不能静默丢弃。单个值和删除范围必须保持
在已协商的 update 预算内。

## 重启：State 和 HLC 必须一起保存

复用相同 replica ID 写入前，原子保存 `{State, ClockState, delivery frontier,
outbox}`：

```go
checkpoint, err := doc.Checkpoint()
if err != nil {
	return err
}
// 在同一宿主事务中保存 checkpoint.State、checkpoint.ClockState、frontier 与 outbox。

recovered, err := shared.Restore(checkpoint, options)
if err != nil {
	return err
}
```

恢复必须传入组实际使用的 `Options`。`Checkpoint` 不负责认证、加密或存储。

## 人和 AI 都可检查的协议选择

高层文档固定使用稳定的 `document/tree-v1` profile：

```go
profile := shared.Profile()
fmt.Println(profile.ConflictRule)
```

需要构建 Manifest 时，先在[按业务意图配置](intent-first-setup.zh-CN.md)中选择并认证
精确的 group/schema/epoch，再用 profile helper 构造。`shared.Document` 不会代替宿主
协商协议、认证、授权、加密、持久化或业务不变量校验。

运行包含乱序和重复投递的完整示例：

```sh
go run ./examples/shared-document
# title=Release plan
# task=release-notes
# done=false
# updates=5
```

深入理解单一所有权、pending 上限、恢复和 wire contract，请继续阅读
[document-tree v1 架构](../design/document-tree-v1.md)与
[document-tree v1 协议](../protocol/document-tree-v1.md)。
