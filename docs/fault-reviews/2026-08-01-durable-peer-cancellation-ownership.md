# 故障复盘：server peer 的取消函数所有权未被静态分析识别

## 基本信息

| 字段 | 内容 |
| --- | --- |
| 日期 | 2026-08-01 |
| 发现人 | beta 发布候选静态检查 |
| 严重程度 | P2-一般 |
| 影响范围 | durable WebSocket peer 的 goroutine 生命周期与 beta lint 门禁 |
| 关联 Issue/PR | 无 |
| 关联提交 | 本次 beta 稳定化修复 |

## 1. 问题描述

### 1.1 问题场景

`newServerPeer` 创建的取消函数存储在 peer 中，并由 `serverPeer.close` 统一执行。取消动作不在
构造函数的局部作用域内，静态分析器无法从赋值位置推导这一所有权路径，发布 lint 将其报告为
未调用的 cancel 函数。

### 1.2 具体表现

```text
G118: context cancel function is not used on all paths
durable/handler.go: newServerPeer
```

运行路径中 `ServeHTTP` 延迟调用 `client.close`，握手/写入失败和订阅撤销也调用同一关闭函数；
但这份所有权约定没有在构造位置被说明，导致 beta 候选无法通过完整 lint。

## 2. 根本原因分析

1. cancel 的生命周期跨越 `newServerPeer`、`ServeHTTP`、`writeLoop` 与 `close`，不是局部 defer
   可以表达的模式。
2. `closeOnce` 保证 `serverPeer.close` 仅执行一次，但构造函数没有声明它是 cancel 的唯一 owner。
3. 现有测试覆盖关闭后的写循环退出，却没有把该资源所有权与静态检查规则显式关联。

## 3. 解决方案

### 3.1 根本解决方案

- 在 `newServerPeer` 的 cancel 创建处增加精确的 `#nosec G118` 说明，限定原因是
  `serverPeer.close` 拥有并且只调用一次该函数；不扩大 gosec 忽略范围。
- 保留 `closeOnce`、上下文取消和连接关闭的既有顺序。
- 在 `durable/wire_state_vector_coverage_test.go` 覆盖关闭 peer 后 `writeLoop` 立即退出，防止
  未来改动破坏取消传播。

### 3.2 影响范围评估

没有改变 WebSocket 协议、队列行为或连接关闭语义。修复将资源所有权从隐式约定变为可审查的
代码注释和回归测试，使静态检查不会掩盖真正的取消泄漏。

## 4. 预防措施

### 4.1 代码层面

- [x] 对跨对象持有的 cancel 函数，将唯一 owner 和一次性关闭机制放在创建点说明。
- [x] 保持 `closeOnce` 作为取消、队列封闭和连接关闭的单一出口。

### 4.2 测试层面

- [x] 覆盖 peer 被关闭后写循环的退出边界。
- [x] 执行 `go test -race ./durable`，验证该生命周期测试在竞态检测下通过。

### 4.3 流程/规范层面

- [ ] 新增 goroutine 或 context owner 时，在代码审查中核对创建者、唯一取消者与所有退出路径。

## 5. 经验总结

> 当 cancel 的所有权跨越对象边界时，代码必须把唯一关闭者和幂等机制写清楚；精确抑制可解释的误报，不能用全局忽略代替生命周期设计。
