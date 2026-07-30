# 跨机器同步探针部署手册

本文是 `cross-host-probe.md` 的中文对照版本；英文文档为主版本。

## 目标与范围

`crdt` 是库，不是网络服务。`cmd/crdt-sync-probe` 仅用于在受控主机间验证已编码 delta 的传输。它不是生产复制守护进程，也不会为库提供 TLS、成员管理、持久化、权限策略或重试语义。

本手册仅适用于短时间、受控的测试窗口。示例中不得使用真实凭据、生产 CRDT 状态或可长期复用的公开令牌。

## 前置条件

- 已验证的本机代码副本，验证使用 Go 1.26.x（模块语言最低版本仍为 Go 1.21）。
- 两台可通过 SSH 访问的 Linux amd64 测试主机。应优先使用专用非 root 账户；下文的 `/opt/crdt-e2e` 仅为示例目录。
- 网络策略仅在测试参与方之间临时放行探针端口。优先使用私网或 SSH 转发。
- 测试主机具备 `openssl`、`sha256sum` 和 `curl`。

## 1. 本机构建与验证

在发布探针二进制前，先运行库的验证门禁：

```sh
make test-unit test-integration
make test-extreme
go test -race ./...
make coverage
go vet ./...
```

在本机交叉编译静态 Linux 二进制，避免在测试主机安装编译器：

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags='-s -w' \
  -o ./dist/crdt-sync-probe ./cmd/crdt-sync-probe

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags='-s -w' \
  -o ./dist/crdt-analyze ./cmd/crdt-analyze

sha256sum ./dist/crdt-sync-probe ./dist/crdt-analyze
```

记录两个哈希值，并在每次上传后核验。远端哈希与本机记录不一致时不得运行二进制。

## 2. 创建短期令牌

为本次演练生成新令牌，不能将令牌放入命令行参数或版本控制：

```sh
umask 077
openssl rand -hex 32 > ./probe.token
chmod 600 ./probe.token
```

探针支持 `-token-file`。应优先使用它，而非 `-token`，避免进程列表或 shell 历史泄露参数。

## 3. 在每台测试主机安装

执行前替换占位符。密码和令牌绝不能写入本文档或源代码仓库。

```sh
ssh <user>@<host-a> 'install -d -m 700 /opt/crdt-e2e'
scp ./dist/crdt-sync-probe ./dist/crdt-analyze ./probe.token \
  <user>@<host-a>:/opt/crdt-e2e/

ssh <user>@<host-a> '
  cd /opt/crdt-e2e &&
  chmod 700 crdt-sync-probe crdt-analyze &&
  chmod 600 probe.token &&
  sha256sum crdt-sync-probe crdt-analyze
'
```

对主机 B 重复执行，并将远端哈希与本机值比较。部署目录只能由部署账户读取。

## 4. 启动两个接收端

默认监听地址为 `127.0.0.1:49511`，且命令会拒绝非回环地址，除非显式指定
`-allow-non-loopback`。优先使用私网或认证隧道；只有在临时测试且防火墙受限时，才绑定
`0.0.0.0:49511`。

```sh
cd /opt/crdt-e2e
nohup ./crdt-sync-probe \
  -mode serve \
  -listen 0.0.0.0:49511 \
  -allow-non-loopback \
  -replica host-a \
  -token-file ./probe.token \
  > server.log 2>&1 &
echo $!
```

主机 B 必须使用不同的 `-replica` 值，并记录两端 PID。对外绑定前，必须将入站访问限制为两台测试主机和本机发送端。

## 5. 执行同步场景

每个发送端都必须使用全局唯一的 replica ID。以逗号分隔的目标列表会生成一条 counter delta 与一条 OR-Set delta，再把同一份字节投递到每个接收端。

```sh
./crdt-sync-probe \
  -mode send \
  -target http://<host-a>:49511,http://<host-b>:49511 \
  -replica sender-a \
  -token-file ./probe.token \
  -counter-increment 2 \
  -element alpha \
  -duplicates 11 \
  -timeout 15s
```

从主机 B 和本机重复执行，使用不同的 ID 与元素。两个目标返回的 JSON 必须包含相同的 counter 分量映射和相同的有序元素集合。

### 可选的显式 RGA 诊断路径

探针**不会**协商 `replica.Manifest` 或 `ProtocolPolicy`。RGA 只能作为显式、受控的测试：
每个接收端和发送端都必须使用相同的 `-rga-protocol`（`v1` 或稳定的 `run-v2`）。未指定该参数时
`/rga` 保持关闭。两种 wire shape 不得混用；协议不匹配的接收端会在修改 RGA 前拒绝帧。
该路由不能建立生产 run-v2 复制组所需的 Manifest 认证。

```sh
# 第 4 步中的两个主机接收端都需增加相同的 flag。
./crdt-sync-probe -mode serve -listen 0.0.0.0:49511 -allow-non-loopback \
  -replica host-a -rga-protocol run-v2 -token-file ./probe.token

# 在受控发送端验证重复投递与最终收敛。
./crdt-sync-probe -mode send \
  -target http://<host-a>:49511,http://<host-b>:49511 \
  -replica rga-sender-a -token-file ./probe.token \
  -counter-increment 0 -element '' -rga-protocol run-v2 \
  -rga-runes 4096 -rga-rune 'λ' -duplicates 3 -timeout 30s
```

每个已接受变更返回带 `X-CRDT-Apply-Micros` 的空 `204 No Content`；发送端只在随后获取一次
`/state`。两个目标报告的 `text.protocol`、`text.runes`、`text.sha256` 必须一致，且
`text.pending` 为零。每个 RGA delta 限制为 16 MiB 和 200,000 个生成 rune。这些诊断不能证明
持久 HLC 恢复、墓碑 GC 安全性或生产延迟 SLO。`run-v2` 可能减少单个同副本线性编辑的字节数，
但规范化解码会带来独立的 CPU 与分配成本；应通过下列命令在目标负载上比较：

```sh
go test -run='^$' -bench='BenchmarkRGADeltaWireProtocols$' -benchmem ./text
```

在每个接收端验证负向路径：

- 不带令牌请求 `GET /state`：应返回 HTTP `401`。
- 带合法令牌但向 `POST /counter` 发送非帧数据：应返回 `400`。
- 带合法令牌发送超过 1 MiB 的请求体：应返回 `400`。
- 向显式配置为 `v1` 的接收端发送 `run-v2` RGA 帧：应返回 `400`，且文本状态不变。
- 每次拒绝请求后，确认合法状态未改变。

跨机演练前可使用本地容量门禁 `make test-extreme`：它会在普通和竞态模式下验证 3 个副本共 6,144 个 OR-Set 元素、状态合并、快照恢复、恢复后的重复 delta、Merkle 一致性，以及 256 分量 G-Counter 批处理。

2026-07-28 的本机验证中，竞态检测连续 10 轮得到的 OR-Set 状态帧为 150,830–151,409 字节，counter batch 为 7,047 字节，均未失败。这些数值是测试观测，不构成通用生产容量承诺；传输与解码限额仍必须基于应用的 payload、成员规模、延迟和内存预算确定。

## 6. 分析已捕获帧

只能分析已受传输层限额保护、且位于受保护测试目录中的帧：

```sh
./crdt-analyze -file ./captured.frame -max-bytes 1048576
```

该 JSON 报告会先验证外层帧，再输出类型、codec ID、payload 大小及 SHA-256 指纹。它不验证具体 CRDT payload，也不认证帧来源。

## 7. 停止与清理验证

仅停止第 4 步记录的精确 PID；共享主机上不得使用宽泛进程匹配：

```sh
kill <host-a-probe-pid>
kill <host-b-probe-pid>
ss -ltn 'sport = :49511'
```

最后的 `ss` 输出必须没有监听器。测试结束后删除或轮换 `probe.token`。只有受保护测试环境仍需使用时才保留二进制；否则应按主机的变更流程删除精确部署目录。

## 验收记录模板

| 项目 | 必需证据 |
| --- | --- |
| 二进制完整性 | 本机及两台远端 SHA-256 值一致 |
| 传输幂等性 | 重复 delta 后只保留一个 counter 分量和一个集合成员关系 |
| 多目标一致性 | 两接收端对同一广播 delta 返回相等状态 |
| 输入保护 | 未授权 = 401；损坏和超限请求体 = 400 |
| RGA 协议一致性 | 两报告协议、摘要、rune 数一致且无 pending；协议不匹配 = 400 |
| 暴露清理 | 已记录 PID 停止，探针监听端口不存在 |
