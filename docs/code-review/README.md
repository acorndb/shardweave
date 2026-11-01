# Code Review 报告

## 规范（不变）

新报告命名：`review-<变更主题>.md`，格式遵循 `AGENTS.md`（Must/Should/Nit + status）。
`files:` 与本次 diff 有交集的报告不允许残留 `status: open`。

## 归档

2026-11-01 之前的 30 份存量报告已按主题合并进
[archive.md](archive.md)（归并引擎 / 语句与会话 / 路由与装配 / 协议与池 / 测试与重构），
条目格式与状态原样保留。归档的 `files:` 并集与 diff 有交集时同样适用
「无 open」要求（当前成立）。更早的 R1–R6 重构期报告已于 2026-10-31 清理，
历史见 git log。
