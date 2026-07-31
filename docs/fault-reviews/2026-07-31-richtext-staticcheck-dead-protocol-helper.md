# 故障复盘：rich-text protocol helper 阻断静态检查

## 基本信息

| 字段 | 内容 |
| --- | --- |
| 日期 | 2026-07-31 |
| 发现人 | Codex beta release gate |
| 严重程度 | P3-轻微 |
| 影响范围 | `staticcheck ./...` |

## 问题与原因

`RichTextProtocol.valid` 没有调用点。该未使用私有方法不会改变运行时协议，但会触发
`U1000`，使仓库的 `make verify` 静态检查阶段失败。

## 修复与预防

移除无调用点的私有 helper，不改变公开 API、协议常量或运行时行为。后续新增私有校验函数时，
应在同一提交运行 `staticcheck ./...`；只需值比较的测试应直接比较公开 `Protocol()` 结果。
