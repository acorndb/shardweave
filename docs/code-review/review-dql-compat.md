---
title: DQL 兼容（EXPLAIN ROUTE / prepared SELECT / SHOW-USE-SET NAMES）review
files:
  - pkg/route/shard/router.go
  - pkg/mysql/forward.go
  - pkg/backend/stream.go
  - pkg/execute/forward.go
  - pkg/execute/pipeline.go
  - pkg/proxy/session.go
  - pkg/session/state.go
  - pkg/session/stmt.go
  - pkg/sql/statement.go
---

## Must Fix

（无）

## Should Fix

### prepared 分片键必须是字面量
- status: wontfix
- file: pkg/route/shard/router.go

`COM_STMT_PREPARE` 时才能确定目标片；`WHERE shard_key = ?` 的参数值在
execute 才可见。D2 v1 要求分片键在 prepare 时是字面量，参数化分片键返回
1235。完整支持需要 execute-time 参数解码 + 广播/惰性 prepare，单独立项。

### prepared 会话会长期 pin 后端连接
- status: wontfix
- file: pkg/execute/pipeline.go

prepare 成功后 `MarkReserved()` 保持 lease，避免后端 stmt id 随连接归还失效；
D2 v1 不在最后一个 stmt close 时解除（reserved 还承载临时表/用户变量语义）。
`COM_RESET_CONNECTION` 会清理。后续引入 prepared-only pin 计数再优化。

### SHOW 走默认片，不做跨片合并
- status: wontfix
- file: pkg/route/shard/router.go

`SHOW TABLES/COLUMNS/CREATE/INDEX/VARIABLES` 路由默认片，依赖外部迁移保证各片
schema 一致；不做结果合并/差异检测。`SHOW STATUS/MASTER` 等仍 1235。

### USE / SET NAMES 只在默认片执行
- status: wontfix
- file: pkg/route/shard/router.go

语句本身在默认片执行，但 `Effect` 写入 `session.State`，后续 lease 通过
`Align` 重放 database/charset，所以其他片也能看到会话效果。

## Nit

### proxy 为 prepared 分配 client stmt id
- status: wontfix
- file: pkg/mysql/forward.go

proxy 改写 prepare OK 的 statement_id，并维护 client→backend 映射；这样不同
slice 的后端 id 冲突不会泄漏给客户端。已有 mysql 层 fixture 单测。
