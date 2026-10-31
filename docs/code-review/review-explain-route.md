---
title: EXPLAIN ROUTE 与 DQL 路由可观测 review
files:
  - pkg/sql/explain_route.go
  - pkg/execute/pipeline.go
  - pkg/proxy/session.go
---

## Must Fix

（无）

## Should Fix

### 路由决策日志尚未落地
- status: wontfix
- file: pkg/execute/pipeline.go

D1 先做 `EXPLAIN ROUTE` 结果集；结构化路由日志/metrics 属于后续观测项，避免在
B1 阶段引入日志字段与采样策略。

### 合成结果集仍硬编码 DEPRECATE_EOF
- status: wontfix
- file: pkg/execute/pipeline.go

`writeTextResult` 按前端握手当前能力写 OK-as-EOF；与 raw forward 一致。后续做
per-session capability 时统一替换。

### EXPLAIN ROUTE 只显示首个/全部 target，不做代价估算
- status: wontfix
- file: pkg/execute/pipeline.go

输出 Kind/Slice/Role/DB/SQL/Reject reason，不做行数/代价估算；P0 目标是
可解释路由，不是优化器。

## Nit

### Error 行与 Reject 行共用 reason 列
- status: wontfix
- file: pkg/execute/pipeline.go

客户端可按 kind 区分；保持 6 列固定形状便于驱动解析。
