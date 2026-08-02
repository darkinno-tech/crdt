# 故障复盘：Frame 解码长度的整数收窄未显式证明安全

## 基本信息

| 字段 | 内容 |
|------|------|
| 日期 | 2026-08-02 |
| 发现人 | `golangci-lint` CI 回归 |
| 严重程度 | P2-一般 |
| 影响范围 | `encoding` 的 v1/v2 outer frame 解码与 Wasm 初始同步验证；未观察到错误解码或线上数据损坏 |
| 关联 Issue/PR | 无 |
| 关联提交 | 告警 SHA `413144f`；修复提交待生成 |

## 1. 问题描述

### 1.1 问题场景

在 `413144f` 上执行 `golangci-lint run ./...`。Frame 解码从不可信 uvarint 读取 codec、payload 和长度前缀，并将它们与 `int` 的限制和切片下标交互。

### 1.2 具体表现

`gosec` 报告 5 个 G115 告警，分别位于 `encoding/frame.go:214,218,223,303,306`；另有同一基准辅助函数的 `unparam` 告警。CI 因静态安全门禁失败，尽管单元测试正常。

### 1.3 错误信息

```text
encoding/frame.go:214:32: G115: integer overflow conversion int -> uint64
encoding/frame.go:218:28: G115: integer overflow conversion uint64 -> int
encoding/frame.go:223:35: G115: integer overflow conversion int -> uint64
encoding/frame.go:303:51: G115: integer overflow conversion int -> uint64
encoding/frame.go:306:19: G115: integer overflow conversion uint64 -> int
internal/wasm/rga_test.go:759:77: populateInitialDocument - runes always receives 65536
```

## 2. 临时解决方案

未使用 `//nolint:gosec`、扩大 limits 或跳过 CI。它们不能为攻击者可控的长度建立可审计的转换前提。

## 3. 根本原因分析

### 3.1 问题分析过程

1. 本地复现 CI 的 6 项报告，确认工具本身正常。
2. 检查 `fed82ac^:encoding/frame.go`，发现 5 个 frame 转换在该提交父版本中已存在；`fed82ac` 触及文件后，差异归因将其显示在该提交范围内。
3. 检查已有 `encoding.uint64AsInt`，它已以 `maxIntValue` 明确拒绝无法表示的长度，并被 v2 解压路径使用。
4. 将 v1 codec/payload 与通用 `ReadBytes` 路径改为先调用该辅助函数，再以 `int` 进行 limits、剩余字节和切片端点校验。
5. 加入携带正确 checksum 的超 `int` codec/payload 长度测试，验证它们 fail-closed 为 `ErrFrameLimit`。
6. 将只用于 65,536 rune 初始快照的测试辅助函数固定为具名常量，消除无意义参数而不改变基准工作负载。

### 3.2 直接原因

解码器在转换前仅以 `uint64(intLimit)` 比较，然后直接执行 `int(length)`。即使实际上限使路径安全，静态分析和未来维护者都不能从转换本身获得该保证。

**相关代码位置**：`encoding/frame.go:213-235,306-327`（修复后），`internal/wasm/rga_test.go:16,759-768`（修复后）。

### 3.3 根本原因

- **设计层面**：安全的 uvarint-to-int 收窄辅助函数存在于 v2 文件，但 v1 与通用 helper 没有统一使用它。
- **开发层面**：先前实现依赖关联比较来保证安全，未将证明沉淀为可复用的转换边界。
- **流程层面**：初始同步工件变更前没有在最新 beta 基线上执行全仓 golangci-lint，因此历史告警在 CI 的差异归因中才集中暴露。

### 3.4 为什么没有提前发现

- **代码审查阶段**：关注点在 canonical frame 与资源上限，而不是每处 `uint64`/`int` 收窄的可证明性。
- **测试阶段**：已有长度边界测试没有构造带有效 checksum 的、超过本机 `int` 的 v1 frame 字段。
- **监控告警**：该问题由编译期安全门禁可靠发现；不应等待运行时异常或线上监控。

## 4. 解决方案

### 4.1 根本解决方案

`UnmarshalFrameView` 和 `ReadBytes` 现在只在 `uint64AsInt` 成功后使用 `int` 长度。随后仍验证 codec/payload 协商上限、剩余字节精确性和切片范围；失败返回原有的 `ErrFrameLimit`。新增回归测试覆盖 codec 与 payload 的溢出 uvarint。

初始快照辅助函数以 `initialSnapshotRunes = 64 << 10` 表达唯一受控场景，所有调用移除固定传入的冗余参数。

### 4.2 影响范围评估

- 有效 canonical frame 的字节与解码结果不变。
- 不能表示为 `int` 的长度现在显式 fail-closed，避免未来重构绕开收窄前提。
- 初始同步基准仍构造相同的 65,536 个中文 rune 和 12 Ki rune 分块。

## 5. 预防措施

### 5.1 代码层面

- [x] 所有本次触及的 wire 长度在转换为 `int` 前复用单一受限辅助函数。
- [ ] 审核新增 wire decoder 时，禁止未证明边界的 `uint64` 到 `int` 收窄。

### 5.2 测试层面

- [x] 覆盖 checksum 有效但 codec/payload 长度超 `int` 的 frame。
- [x] 执行 Frame fuzz 150,000 次和 race 回归。

### 5.3 监控层面

- [x] 保持 `gosec` G115 作为合并门禁；frame 解码不记录或上报 payload。

### 5.4 流程/规范层面

- [x] 初始同步协议变更在推送 beta 前执行全仓 lint，并对最新 beta 重新验证。

## 6. 经验总结（一句话）

> 对不可信 wire 长度，先把“可表示为 int”的证明封装成明确边界，再进行 limits 比较和切片；正确的关联比较不能替代可审计的收窄步骤。
